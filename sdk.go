package userApiGolang

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultBaseURL = "https://proxy-seller.com/personal/api/v2/"

// URL is kept for source compatibility. New code should use WithBaseURL.
var URL = DefaultBaseURL

type Client struct {
	mu           sync.RWMutex
	baseURL      string
	apiKey       string
	paymentID    string
	paymentCode  string
	generateAuth string
	httpClient   *http.Client
}

type ClientOption func(*Client)

func WithBaseURL(baseURL string) ClientOption {
	return func(c *Client) { c.baseURL = normalizeBaseURL(baseURL) }
}

func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if timeout > 0 {
			if c.httpClient == nil {
				c.httpClient = &http.Client{}
			}
			copy := *c.httpClient
			copy.Timeout = timeout
			c.httpClient = &copy
		}
	}
}

func NewClient(apiKey string, options ...ClientOption) *Client {
	c := &Client{baseURL: DefaultBaseURL, apiKey: apiKey, generateAuth: "N", httpClient: &http.Client{Timeout: 30 * time.Second}}
	for _, option := range options {
		option(c)
	}
	return c
}

// DefaultClient backs all deprecated package-level helpers.
var DefaultClient = NewClient("")

type ResultData struct {
	Slice  []interface{}
	Map    map[string]interface{}
	Value  interface{}
	Raw    json.RawMessage
	Status string
}

func NewResultData() ResultData {
	return ResultData{
		Slice: nil,
		Map:   nil,
		Value: nil,
	}
}

// APIErrorCode — значение errors[].code конверта client-api v2.
//
// На бэкенде код ВСЕГДА целый: ProxySellerApiErrorItemDto.code и ClientApiErrorsDto.code
// объявлены как `int`, у резидентских маршрутов — ApiErrorDto.code, тоже `int`.
// Раньше это поле лежало в interface{}, и encoding/json раскладывал число в float64:
// сравнение `item.Code == 503` было тихо ложным всегда, потому что сравнивался int-литерал
// с float64. Тип хранит исходный литерал (как json.Number) и умеет отдать его числом;
// строковый код тоже принимается — чтобы неожиданный формат не ронял разбор всего ответа.
type APIErrorCode string

func (c *APIErrorCode) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*c = ""
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*c = APIErrorCode(strings.TrimSpace(value))
		return nil
	}
	*c = APIErrorCode(text)
	return nil
}

// MarshalJSON возвращает код в исходном виде: число числом, всё остальное строкой.
func (c APIErrorCode) MarshalJSON() ([]byte, error) {
	text := strings.TrimSpace(string(c))
	if text == "" {
		return []byte("null"), nil
	}
	if _, err := strconv.ParseInt(text, 10, 64); err == nil {
		return []byte(text), nil
	}
	return json.Marshal(text)
}

func (c APIErrorCode) String() string { return string(c) }

// Int — код числом. 0, если код пустой или нечисловой (0 у сервера тоже валиден:
// ClientApiErrors.IP_NOT_FOUND и ошибки, собранные вручную, используют code 0).
func (c APIErrorCode) Int() int {
	value, err := c.Int64()
	if err != nil {
		return 0
	}
	return int(value)
}

func (c APIErrorCode) Int64() (int64, error) {
	text := strings.TrimSpace(string(c))
	value, err := strconv.ParseInt(text, 10, 64)
	if err == nil {
		return value, nil
	}
	// Подстраховка на случай кода, пришедшего дробным литералом (503.0): сам сервер
	// объявляет код int, но ронять разбор из-за формата числа не хочется.
	if asFloat, floatErr := strconv.ParseFloat(text, 64); floatErr == nil {
		return int64(asFloat), nil
	}
	return 0, err
}

type APIErrorItem struct {
	Message string       `json:"message"`
	Code    APIErrorCode `json:"code"`
	// CustomData — свободная полезная нагрузка ошибки. Заполняется, например,
	// balance/autotopup/set: там сюда уезжают границы значений
	// (minAmount / minThreshold / minDailyCountCap), см. AutoTopupLimitsFromError.
	CustomData interface{} `json:"customData,omitempty"`
}

// CodeInt — код этой ошибки числом.
func (i APIErrorItem) CodeInt() int { return i.Code.Int() }

type APIError struct {
	HTTPStatus int
	Status     string
	Errors     []APIErrorItem
	Data       interface{}
	Body       string
}

// ApiError and ApiErrorItem are aliases for callers that prefer Go's mixed-case acronym style.
type ApiError = APIError
type ApiErrorItem = APIErrorItem

func (e *APIError) Error() string {
	if len(e.Errors) > 0 {
		return fmt.Sprintf("client api error %v: %s", e.Errors[0].Code, e.Errors[0].Message)
	}
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("client api HTTP %d", e.HTTPStatus)
	}
	return "client api error"
}

// CodeInt — код ПЕРВОЙ ошибки. Ошибок в ответе может быть несколько (например тройка
// ошибок доступа), поэтому для проверок лучше HasCode/Messages.
func (e *APIError) CodeInt() int {
	if len(e.Errors) == 0 {
		return 0
	}
	return e.Errors[0].CodeInt()
}

// HasCode — есть ли такой код где-либо в errors[], а не только в errors[0].
func (e *APIError) HasCode(code int) bool {
	for _, item := range e.Errors {
		if item.CodeInt() == code {
			return true
		}
	}
	return false
}

func (e *APIError) Messages() []string {
	messages := make([]string, 0, len(e.Errors))
	for _, item := range e.Errors {
		messages = append(messages, item.Message)
	}
	return messages
}

// FirstCustomData — customData первой ошибки, у которой он есть.
func (e *APIError) FirstCustomData() interface{} {
	for _, item := range e.Errors {
		if item.CustomData != nil {
			return item.CustomData
		}
	}
	return nil
}

// IsAccessError — ошибка доступа: битый ключ, IP не в белом списке либо превышенный
// rate limit. Сервер во всех трёх случаях отвечает HTTP 200 и ОДНОЙ И ТОЙ ЖЕ тройкой
// ошибок (LegacyClientApiErrorHelper.legacyAccessErrors): "Error api key" /
// "IP not allowed <ip>" / "Request limit reached", у всех code 503. Отличить причину по
// ответу нельзя — HTTP 429 в v2 не существует. Условие повторяет серверный
// LegacyClientApiErrorHelper.isAccessError.
func (e *APIError) IsAccessError() bool {
	for _, item := range e.Errors {
		switch item.CodeInt() {
		case 2, 3: // ERROR_API_KEY, ERROR_AUTH_IP
			return true
		}
		if item.Message == "Error api key" || item.Message == "Request limit reached" ||
			strings.HasPrefix(item.Message, "Error auth.") ||
			strings.HasPrefix(item.Message, "IP not allowed") {
			return true
		}
	}
	return false
}

func normalizeBaseURL(baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	return strings.TrimRight(baseURL, "/") + "/"
}

// SetApiKey Key placed in https://proxy-seller.com/personal/api/
func SetApiKey(key string) {
	DefaultClient.SetAPIKey(key)
}

func (c *Client) SetAPIKey(key string) { c.mu.Lock(); c.apiKey = key; c.mu.Unlock() }

func (c *Client) SetBaseURL(baseURL string) {
	c.mu.Lock()
	c.baseURL = normalizeBaseURL(baseURL)
	c.mu.Unlock()
}

// SetPaymentId payment system id (MongoDB ObjectId from balance/payments/list)
func SetPaymentId(id string) {
	DefaultClient.SetPaymentID(id)
}

func SetPaymentCode(code string) { DefaultClient.SetPaymentCode(code) }

func (c *Client) SetPaymentID(id string) {
	c.mu.Lock()
	c.paymentID, c.paymentCode = id, ""
	c.mu.Unlock()
}

func (c *Client) SetPaymentCode(code string) {
	c.mu.Lock()
	c.paymentCode, c.paymentID = code, ""
	c.mu.Unlock()
}

func GetPaymentId() string {
	return DefaultClient.GetPaymentID()
}

func GetPaymentCode() string { return DefaultClient.GetPaymentCode() }

func (c *Client) GetPaymentID() string   { c.mu.RLock(); defer c.mu.RUnlock(); return c.paymentID }
func (c *Client) GetPaymentCode() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.paymentCode }

// SetGenerateAuth Y or N
func SetGenerateAuth(yn string) {
	DefaultClient.SetGenerateAuth(yn)
}

func (c *Client) SetGenerateAuth(yn string) {
	c.mu.Lock()
	if yn == "Y" {
		c.generateAuth = "Y"
	} else {
		c.generateAuth = "N"
	}
	c.mu.Unlock()
}

func GetGenerateAuth() string {
	return DefaultClient.GetGenerateAuth()
}

func (c *Client) GetGenerateAuth() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.generateAuth }

func tryConvert(i interface{}) ResultData {
	result := NewResultData()
	result.Value = i
	// Попытка преобразования в map[string]interface{}
	if m, ok := i.(map[string]interface{}); ok {
		//fmt.Println("Value is a map[string]interface{}:", m)
		result.Map = m
		return result
	}

	// Попытка преобразования в []interface{}
	if s, ok := i.([]interface{}); ok {
		//fmt.Println("Value is a []interface{}:", s)
		result.Slice = s
		return result
	}

	return result
}

// Request Send request into server
func Request(method string, uri string, data map[string]interface{}) (ResultData, error) {
	return legacyClient().Request(method, uri, data)
}

func legacyClient() *Client {
	if URL != "" {
		DefaultClient.SetBaseURL(URL)
	}
	return DefaultClient
}

func RequestBinary(uri string) []byte {
	data, _ := RequestBinaryE(uri)
	return data
}

func RequestBinaryE(uri string) ([]byte, error) { return legacyClient().RequestBinary(uri) }

func (c *Client) snapshot() (string, string, *http.Client, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.apiKey == "" {
		return "", "", nil, fmt.Errorf("client api key is required")
	}
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return normalizeBaseURL(c.baseURL), c.apiKey, httpClient, nil
}

func (c *Client) Request(method, uri string, data interface{}) (ResultData, error) {
	body, status, err := c.do(method, uri, data)
	if err != nil {
		return NewResultData(), err
	}
	return parseEnvelope(body, status)
}

func (c *Client) do(method, uri string, data interface{}) ([]byte, int, error) {
	baseURL, key, httpClient, err := c.snapshot()
	if err != nil {
		return nil, 0, err
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid client api base URL: %w", err)
	}
	relative, err := url.Parse(uri)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid client api path: %w", err)
	}
	escapedPath := strings.TrimRight(parsed.EscapedPath(), "/") + "/" + url.PathEscape(key) + "/" + strings.TrimLeft(relative.EscapedPath(), "/")
	parsed.Path, err = url.PathUnescape(escapedPath)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid client api escaped path: %w", err)
	}
	parsed.RawPath = escapedPath
	parsed.RawQuery = relative.RawQuery
	var reader io.Reader
	if method != http.MethodGet && method != http.MethodHead && data != nil {
		payload, marshalErr := json.Marshal(data)
		if marshalErr != nil {
			return nil, 0, marshalErr
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, parsed.String(), reader)
	if err != nil {
		return nil, 0, err
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func parseEnvelope(body []byte, httpStatus int) (ResultData, error) {
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Errors []APIErrorItem  `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		if httpStatus < 200 || httpStatus >= 300 {
			return NewResultData(), &APIError{HTTPStatus: httpStatus, Body: string(body)}
		}
		return NewResultData(), err
	}
	var value interface{}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, &value); err != nil {
			return NewResultData(), err
		}
	}
	httpOK := httpStatus >= 200 && httpStatus < 300
	if httpOK && (envelope.Status == "success" || (envelope.Status == "error" && len(envelope.Errors) == 0 && value != nil)) {
		result := tryConvert(value)
		result.Raw, result.Status = envelope.Data, envelope.Status
		return result, nil
	}
	if envelope.Status != "" || !httpOK {
		if len(envelope.Errors) == 0 {
			var direct APIErrorItem
			if json.Unmarshal(body, &direct) == nil && direct.Message != "" {
				envelope.Errors = []APIErrorItem{direct}
			}
		}
		return NewResultData(), &APIError{HTTPStatus: httpStatus, Status: envelope.Status, Errors: envelope.Errors, Data: value, Body: string(body)}
	}
	return NewResultData(), fmt.Errorf("unexpected client api response")
}

func (c *Client) RequestBinary(uri string) ([]byte, error) {
	body, status, err := c.do(http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		_, parseErr := parseEnvelope(body, status)
		return nil, parseErr
	}
	var marker struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &marker) == nil && marker.Status == "error" {
		_, parseErr := parseEnvelope(body, status)
		return nil, parseErr
	}
	return body, nil
}

// MaxProxyDownloadExtLength — предел длины ext на выгрузках, как на сервере
// (ResidentUserApiService.downloadProxyList: `e.length() > 250`).
const MaxProxyDownloadExtLength = 250

// forbiddenExtRunes — символы, которые сервер в ext не пропускает: ими подставляют
// перевод строки в заголовок Content-Disposition и путь в имя файла.
var forbiddenExtRunes = []string{"\r", "\n", "/", "\\"}

// assertExt повторяет серверную проверку ext на download-маршрутах.
//
// Важно, почему это стоит ловить локально: при нарушении сервер отвечает НЕ конвертом
// {status,data,errors}, а голым HTTP 400 с плайн-текстом в теле ("ext is too long (max 250)"
// либо "ext contains forbidden characters"). Такой ответ не разбирается в APIError, и вызывающий
// получает невнятную ошибку разбора вместо понятной причины.
//
// Длину считаем в рунах: на сервере это String.length() (UTF-16), для ASCII-шаблонов
// (а ext — это txt/csv либо шаблон из %ip%/%port%/%login%/...) значения совпадают.
func assertExt(ext string) error {
	if ext == "" {
		return nil
	}
	if len([]rune(ext)) > MaxProxyDownloadExtLength {
		return fmt.Errorf("ext is too long (max %d characters), the server replies with a bare HTTP 400 instead of the usual envelope", MaxProxyDownloadExtLength)
	}
	for _, forbidden := range forbiddenExtRunes {
		if strings.Contains(ext, forbidden) {
			return fmt.Errorf("ext contains forbidden character %q (CR, LF, %q and %q are rejected by the server with a bare HTTP 400)", forbidden, "/", "\\")
		}
	}
	return nil
}

/////////////////////////////// Auth ///////////////////////////////

// AuthList Get auths.
// data — голый массив (AuthListResponseClientDto.data — List<AuthItemClientDto>), без обёртки
// items. Элементы: id (ObjectId string), active, login, password, ip, orderNumber.
//
// Неэкспортируемая обёртка authList, которая заворачивала результат в {"items": ...}, удалена:
// это была форма v1, на неё никто не ссылался, и она противоречила фактическому ответу.
func AuthList() ([]interface{}, error) { return legacyClient().AuthList() }

func (c *Client) AuthList() ([]interface{}, error) {
	result, err := c.Request(http.MethodGet, "auth/list", nil)
	if err != nil {
		return nil, err
	}
	return result.Slice, nil
}

type AuthChangeRequest struct {
	ID       string  `json:"id"`
	Active   *bool   `json:"active,omitempty"`
	Login    *string `json:"login,omitempty"`
	Password *string `json:"password,omitempty"`
	IP       *string `json:"ip,omitempty"`
}

func (c *Client) ChangeAuth(change AuthChangeRequest) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "auth/change", change)
	return result.Map, err
}

/**
 * Set auth active state
 * @param string $id - ObjectId string of the authorization (auth/list -> id)
 * @param bool $active
 * @return array Returns current auth
 */
func AuthChange(id string, active bool, login string, password string, ip string) (map[string]interface{}, error) {
	change := AuthChangeRequest{ID: id, Active: &active}
	if login != "" {
		change.Login = &login
	}
	if password != "" {
		change.Password = &password
	}
	if ip != "" {
		change.IP = &ip
	}
	return legacyClient().ChangeAuth(change)
}

/////////////////////////////// Balance ///////////////////////////////

// Balance Get balance statistic
func Balance() float64 {
	value, err := legacyClient().Balance()
	if err != nil {
		return -1
	}
	return value
}

func BalanceE() (float64, error) { return legacyClient().Balance() }

func (c *Client) Balance() (float64, error) {
	result, err := c.Request(http.MethodGet, "balance/get", nil)
	if err != nil {
		return 0, err
	}
	value, ok := result.Map["summ"].(float64)
	if !ok {
		return 0, fmt.Errorf("balance/get: data.summ is not a number")
	}
	return value, nil
}

// BalanceAdd Replenish the balance.
// Returns data.url — a link to the payment page of the chosen payment system.
//
// paymentId — ObjectId string from balance/payments/list. paymentCode здесь НЕ работает:
// ClientApiService.addBalance сверяет только id и не зовёт normalizeOrderReferenceCodes
// (см. AddBalance). Старая подпись глотает ошибку — используйте BalanceAddE.
func BalanceAdd(summ float64, paymentId string) string {
	value, _ := legacyClient().AddBalance(summ, paymentId)
	return value
}

func BalanceAddE(summ float64, paymentID string) (string, error) {
	return legacyClient().AddBalance(summ, paymentID)
}

func (c *Client) AddBalance(summ float64, paymentID string) (string, error) {
	if paymentID == "" {
		paymentID = c.GetPaymentID()
	}
	// balance/add — единственная точка, где paymentCode НЕ работает.
	// ClientApiService.addBalance ищет платёжку строго как
	// availableBalancePaymentSystems(userId).find { it.id == dto.paymentId } и НЕ зовёт
	// normalizeOrderReferenceCodes / resolvePaymentSystemByClientCode (в отличие от
	// order/calc, order/make и prolong/*). Раньше при заданном только paymentCode SDK
	// уезжал с paymentId="" и получал невнятное "paymentId not exists".
	if paymentID == "" {
		if code := c.GetPaymentCode(); code != "" {
			return "", fmt.Errorf("balance/add does not resolve paymentCode %q: pass a paymentId taken from balance/payments/list (paymentCode works only for order/calc, order/make and prolong/*)", code)
		}
		return "", fmt.Errorf("balance/add: paymentId is required, take it from balance/payments/list")
	}
	result, err := c.Request(http.MethodPost, "balance/add", map[string]interface{}{"summ": summ, "paymentId": paymentID})
	if err != nil {
		return "", err
	}
	value, ok := result.Map["url"].(string)
	if !ok {
		return "", fmt.Errorf("balance/add: data.url is not a string")
	}
	return value, nil
}

// BalancePaymentsList List of payment systems for balance replenishing.
// data.items[].id — ObjectId string, not a number (BalancePaymentItemClientDto.id is String).
// return array Example items:
// [
//
//	[
//	   'id' => '68b1f0c4e13a4c0f1a2b3c4d',
//	   'name' =>'PayPal'
//	],
//	[
//	  'id' => '68b1f0c4e13a4c0f1a2b3c51',
//	   'name' => 'Visa / MasterCard'
//	]
//
// ]
func BalancePaymentsList() ([]interface{}, error) { return legacyClient().BalancePaymentsList() }

func (c *Client) BalancePaymentsList() ([]interface{}, error) {
	result, err := c.Request(http.MethodGet, "balance/payments/list", nil)
	if err != nil {
		return nil, err
	}
	if items, ok := result.Map["items"].([]interface{}); ok {
		return items, nil
	}
	return nil, fmt.Errorf("balance/payments/list: data.items is not an array")
}

/////////////////////////////// Balance auto top-up ///////////////////////////////

// AutoTopupSetRequest — тело POST balance/autotopup/set (AutoTopupSetRequestClientDto).
//
// PARTIAL UPDATE: сервер меняет только те поля, которые реально пришли в JSON, остальные
// берёт из сохранённых настроек. Поэтому все поля — указатели с omitempty: не заданное поле
// вообще не попадает в запрос, а не уезжает как null (null сервер трактовал бы как "прислали
// пустое значение" и валидация мержа могла бы отбить весь запрос).
//
// Границы значений (минимальная сумма, минимальный порог, минимальный дневной лимит) при
// ошибке валидации приходят в errors[0].customData — см. AutoTopupLimitsFromError.
// Коды ошибок авто-пополнения: 49 disabled, 50 min threshold, 51 min amount,
// 52 amount below threshold, 53 no payment method, 54 min daily count cap,
// 55 monthly cap below amount, 56 payment method invalid.
type AutoTopupSetRequest struct {
	// Enabled — включить/выключить. Не задано — состояние не меняется.
	Enabled *bool `json:"enabled,omitempty"`
	// Threshold — списываем, когда баланс становится меньше этого значения.
	Threshold *float64 `json:"threshold,omitempty"`
	// Amount — сумма одного автопополнения (минимум $5 и не меньше Threshold).
	Amount *float64 `json:"amount,omitempty"`
	// SubscriptionID — подписка Paddle, которой списывать; это paymentMethod.id либо
	// subscriptionId из ответа balance/autotopup/get.
	SubscriptionID *string `json:"subscriptionId,omitempty"`
	// DailyCountCap — свой (более строгий) лимит числа списаний в сутки.
	DailyCountCap *int `json:"dailyCountCap,omitempty"`
	// MonthlyAmountCap — свой лимит суммы списаний за 30 дней.
	MonthlyAmountCap *float64 `json:"monthlyAmountCap,omitempty"`
}

// AutoTopupPaymentMethod — привязанный платёжный метод (AutoTopupPaymentMethodClientDto).
type AutoTopupPaymentMethod struct {
	// ID — id подписки Paddle; его же можно отправить обратно в AutoTopupSetRequest.SubscriptionID.
	ID string `json:"id,omitempty"`
	// Status — "active" / "expired".
	Status string `json:"status,omitempty"`
	// PaymentMethod — "card" / "PayPal" / "Google Pay" / "Apple Pay", как отдаёт Paddle.
	PaymentMethod string `json:"paymentMethod,omitempty"`
	// Brand — для card visa/mastercard/..., для остальных paypal/google_pay/apple_pay.
	Brand string `json:"brand,omitempty"`
	// Last4 — пусто для не-карточных методов.
	Last4 string `json:"last4,omitempty"`
	// Exp — "MM/YYYY", только для card.
	Exp string `json:"exp,omitempty"`
}

// AutoTopupLastEvent — последнее срабатывание (AutoTopupLastEventClientDto).
type AutoTopupLastEvent struct {
	// Status — TRIGGERED | SUCCEEDED | FAILED | SKIPPED_CAP | SETTINGS_SAVED | PAUSED
	// (enum AutoTopupEventStatus).
	Status string   `json:"status,omitempty"`
	Amount *float64 `json:"amount,omitempty"`
	// At — дата события. Сервер отдаёт java.util.Date, формат зависит от конфигурации
	// Jackson, поэтому SDK его не парсит.
	At interface{} `json:"at,omitempty"`
	// Reason — причина неудачи/пропуска; null для SUCCEEDED.
	Reason string `json:"reason,omitempty"`
}

// AutoTopupState — data обоих ответов, get и set (AutoTopupStateClientDto).
// set отдаёт состояние ПОСЛЕ сохранения, второй запрос за актуальным состоянием не нужен.
type AutoTopupState struct {
	Configured bool `json:"configured"`
	Enabled    bool `json:"enabled"`
	// State — NO_PAYMENT_METHOD | DISABLED | ACTIVE | PAYMENT_INVALID | PAUSED_FAILURES
	// (enum AutoTopupState на сервере).
	State            string                  `json:"state,omitempty"`
	Threshold        *float64                `json:"threshold,omitempty"`
	Amount           *float64                `json:"amount,omitempty"`
	SubscriptionID   string                  `json:"subscriptionId,omitempty"`
	PaymentMethod    *AutoTopupPaymentMethod `json:"paymentMethod,omitempty"`
	DailyCountCap    *int                    `json:"dailyCountCap,omitempty"`
	MonthlyAmountCap *float64                `json:"monthlyAmountCap,omitempty"`
	// FailCount — подряд идущие неудачные попытки списания.
	FailCount int `json:"failCount"`
	// LastAttemptAt — java.util.Date с сервера; SDK не парсит (см. AutoTopupLastEvent.At).
	LastAttemptAt interface{}         `json:"lastAttemptAt,omitempty"`
	LastEvent     *AutoTopupLastEvent `json:"lastEvent,omitempty"`
}

// AutoTopupLimits — границы значений из errors[].customData ответа balance/autotopup/set.
// Ключи задаёт ClientApiService.setAutoTopup: minAmount, minThreshold, minDailyCountCap.
// Присутствуют только те, что относятся к сработавшей проверке.
type AutoTopupLimits struct {
	MinAmount        *float64
	MinThreshold     *float64
	MinDailyCountCap *int
}

// AutoTopupLimitsFromError достаёт границы из ошибки balance/autotopup/set.
// Второе значение — false, если это не APIError либо в customData границ нет.
func AutoTopupLimitsFromError(err error) (AutoTopupLimits, bool) {
	limits := AutoTopupLimits{}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr == nil {
		return limits, false
	}
	found := false
	for _, item := range apiErr.Errors {
		data, isMap := item.CustomData.(map[string]interface{})
		if !isMap {
			continue
		}
		if value, exists := autoTopupLimitFloat(data, "minAmount"); exists {
			limits.MinAmount, found = &value, true
		}
		if value, exists := autoTopupLimitFloat(data, "minThreshold"); exists {
			limits.MinThreshold, found = &value, true
		}
		if value, exists := autoTopupLimitFloat(data, "minDailyCountCap"); exists {
			intValue := int(value)
			limits.MinDailyCountCap, found = &intValue, true
		}
	}
	return limits, found
}

func autoTopupLimitFloat(data map[string]interface{}, key string) (float64, bool) {
	raw, exists := data[key]
	if !exists || raw == nil {
		return 0, false
	}
	switch value := raw.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		if parsed, err := value.Float64(); err == nil {
			return parsed, true
		}
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// AutoTopupGet Get auto top-up configuration (GET balance/autotopup/get).
// Если фича выключена Property-флагом enabled_autopopup_balance, сервер отвечает
// ошибкой 49 "Auto top-up is not available".
func AutoTopupGet() (*AutoTopupState, error) { return legacyClient().GetAutoTopup() }

func (c *Client) GetAutoTopup() (*AutoTopupState, error) {
	result, err := c.Request(http.MethodGet, "balance/autotopup/get", nil)
	if err != nil {
		return nil, err
	}
	return decodeAutoTopupState("balance/autotopup/get", result)
}

// AutoTopupSet Enable/disable auto top-up or update thresholds and caps
// (POST balance/autotopup/set). Partial update: отправляются только заданные поля.
// Ответ — состояние ПОСЛЕ сохранения.
func AutoTopupSet(request AutoTopupSetRequest) (*AutoTopupState, error) {
	return legacyClient().SetAutoTopup(request)
}

func (c *Client) SetAutoTopup(request AutoTopupSetRequest) (*AutoTopupState, error) {
	result, err := c.Request(http.MethodPost, "balance/autotopup/set", request)
	if err != nil {
		return nil, err
	}
	return decodeAutoTopupState("balance/autotopup/set", result)
}

func decodeAutoTopupState(endpoint string, result ResultData) (*AutoTopupState, error) {
	if len(result.Raw) == 0 || string(result.Raw) == "null" {
		return nil, fmt.Errorf("%s: data is empty", endpoint)
	}
	state := &AutoTopupState{}
	if err := json.Unmarshal(result.Raw, state); err != nil {
		return nil, fmt.Errorf("%s: %w", endpoint, err)
	}
	return state, nil
}

// Bool, Float64, Int and String build pointers for AutoTopupSetRequest, where a missing
// field means "keep the stored value" and must not be sent as null.
func Bool(value bool) *bool          { return &value }
func Float64(value float64) *float64 { return &value }
func Int(value int) *int             { return &value }
func String(value string) *string    { return &value }

/////////////////////////////// Order ///////////////////////////////

// ReferenceList Necessary guides for creating an order
// @param string proxyType - ipv4 | ipv6 | mobile | isp | mix | ""
// - Countries + operators and rotation periods (mobile only)
// - Proxy periods
// - Purposes and services (only for ipv4,ipv6,isp,mix,mix_isp,resident)
// - Quantities allowed (only for mix proxy)
//
// What the reference really carries, field by field (ClientApiService.buildLegacyReferenceItem):
//
//	country[]                  id, name, alpha3         → alpha3 IS the country code
//	period[]                   id, name                 → no period code in the reference
//	country[].operators.*[]    id, name, rotations[]    → no operator code (tag) in the reference
//	   .rotations[]            id, name                 → id is the interval in MINUTES, 0 = By Link
//	quantities[]               id, name, quantities[]   → mix package id + allowed quantities
//	country[] for mix/mix_isp  id, name, alpha3(null), tag → tag IS the mix code
//	tarifs[] (resident)        id, name, personal       → no tariff code in the reference
//
// So only countryCode (alpha3) and mixCode (tag, mix/mix_isp only) can be read out of the reference.
// periodCode, operatorCode and tarifCode exist as request fields but are not published here — take
// the corresponding id instead.
func ReferenceList(proxyType string) (map[string]interface{}, error) {
	return legacyClient().ReferenceList(proxyType)
}

func (c *Client) ReferenceList(proxyType string) (map[string]interface{}, error) {
	uri := "reference/list"
	if proxyType != "" {
		uri += "/" + url.PathEscape(proxyType)
	}
	result, err := c.Request(http.MethodGet, uri, nil)
	return result.Map, err
}

// OrderCalcIpv4 Calculate the order IPv4
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param string countryId - ObjectId string from reference/list (v2 ids are not numbers) OR the
// country code: a *Id value that is not an ObjectId is resolved as a code
// (ClientApiService.normalizeOrderReferenceCodes). The code is the alpha3 from
// reference/list → country[].alpha3, uppercased by the server ("USA").
// @param string periodId - ObjectId string from reference/list OR the period code, lowercased by
// the server ("1m", "3m"). reference/list publishes only period[].id and period[].name — the code
// itself is not part of the reference, so hardcode it or use periodId.
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string customTargetName - required for ipv4/ipv6/isp and for unresolved mix/mix_isp
// @return array Example
// [
//
//	'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//	'balance' => 2,
//	'total' => 35.1,
//	'quantity' => 5,
//	'currency' => 'USD',
//	'discount' => 0.22,
//	'price' => 7.02
//
// ]
func OrderCalcIpv4(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return legacyClient().OrderCalcIpv4(countryId, periodId, quantity, authorization, coupon, customTargetName)
}

func (c *Client) OrderCalcIpv4(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return c.orderCalc(c.prepareRegular("ipv4", countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderCalcIsp Calculate the order ISP
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param string countryId - ObjectId string from reference/list (v2 ids are not numbers) OR the
// country code: a *Id value that is not an ObjectId is resolved as a code
// (ClientApiService.normalizeOrderReferenceCodes). The code is the alpha3 from
// reference/list → country[].alpha3, uppercased by the server ("USA").
// @param string periodId - ObjectId string from reference/list OR the period code, lowercased by
// the server ("1m", "3m"). reference/list publishes only period[].id and period[].name — the code
// itself is not part of the reference, so hardcode it or use periodId.
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string customTargetName - required for ipv4/ipv6/isp and for unresolved mix/mix_isp
// @return array Example
// [
//
//	'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//	'balance' => 2,
//	'total' => 35.1,
//	'quantity' => 5,
//	'currency' => 'USD',
//	'discount' => 0.22,
//	'price' => 7.02
//
// ]
func OrderCalcIsp(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return legacyClient().OrderCalcIsp(countryId, periodId, quantity, authorization, coupon, customTargetName)
}

func (c *Client) OrderCalcIsp(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return c.orderCalc(c.prepareRegular("isp", countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderCalcMix Calculate the order Mix
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// For mix, countryId doubles as the MIX package selector: the package id, or "packageId:quantity"
// (ClientApiService.parseMixSelection). The mix tag also fits, since the id fallback resolves it.
// @param string countryId - ObjectId string from reference/list (v2 ids are not numbers) OR the
// country code: a *Id value that is not an ObjectId is resolved as a code
// (ClientApiService.normalizeOrderReferenceCodes). The code is the alpha3 from
// reference/list → country[].alpha3, uppercased by the server ("USA").
// @param string periodId - ObjectId string from reference/list OR the period code, lowercased by
// the server ("1m", "3m"). reference/list publishes only period[].id and period[].name — the code
// itself is not part of the reference, so hardcode it or use periodId.
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string customTargetName - required for ipv4/ipv6/isp and for unresolved mix/mix_isp
// @return array Example
// [
//
//	'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//	'balance' => 2,
//	'total' => 35.1,
//	'quantity' => 5,
//	'currency' => 'USD',
//	'discount' => 0.22,
//	'price' => 7.02
//
// ]
func OrderCalcMix(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return legacyClient().OrderCalcMix(countryId, periodId, quantity, authorization, coupon, customTargetName)
}

func (c *Client) OrderCalcMix(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return c.orderCalc(c.prepareRegular("mix", countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderCalcIpv6 Calculate the order IPv6
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param string countryId - ObjectId string from reference/list (v2 ids are not numbers) OR the
// country code: a *Id value that is not an ObjectId is resolved as a code
// (ClientApiService.normalizeOrderReferenceCodes). The code is the alpha3 from
// reference/list → country[].alpha3, uppercased by the server ("USA").
// @param string periodId - ObjectId string from reference/list OR the period code, lowercased by
// the server ("1m", "3m"). reference/list publishes only period[].id and period[].name — the code
// itself is not part of the reference, so hardcode it or use periodId.
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string customTargetName - required for ipv4/ipv6/isp and for unresolved mix/mix_isp
// @param string protocol - http | https | socks | socks5 (case-insensitive), or "" for the default
// (https). Anything else is rejected with "Incorrect protocol", code 13 (parseClientApiProtocol).
// @return array Example
// [
//
//	'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//	'balance' => 2,
//	'total' => 35.1,
//	'quantity' => 5,
//	'currency' => 'USD',
//	'discount' => 0.22,
//	'price' => 7.02
//
// ]
func OrderCalcIpv6(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string, protocol string) (map[string]interface{}, error) {
	return legacyClient().OrderCalcIpv6(countryId, periodId, quantity, authorization, coupon, customTargetName, protocol)
}

func (c *Client) OrderCalcIpv6(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string, protocol string) (map[string]interface{}, error) {
	return c.orderCalc(c.prepareIpv6(countryId, periodId, quantity, authorization, coupon, customTargetName, protocol))
}

// OrderCalcMobile Calculate the order Mobile
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// This helper always sends mobileServiceType=dedicated; for shared use CalculateOrder.
// Reference ids may be replaced by stable codes right here, positionally — the server resolves
// a *Id value that is not an ObjectId as a code (ClientApiService.normalizeOrderReferenceCodes),
// so the *Code fields of OrderRequest are never required for that.
// @param string countryId - ObjectId string OR country code (alpha3, uppercased by the server: "USA")
// @param string periodId - ObjectId string OR period code (lowercased by the server: "1m", "3m")
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string operatorId - ObjectId string OR operator tag (matched by MobileOperator.tag);
// the id comes from reference/list/mobile → country[].operators.dedicated[].id
// @param string rotationId - rotation interval in MINUTES, as a numeric string: "5", "10", "0" = By Link,
// "" = not set. Values come from reference/list/mobile → operators[].rotations[].id, which is the
// minute count itself. There are no rotation codes and no code fallback here: the server does
// `requestDto.rotationId as int`, so "5m" is not a rotation — it throws and the envelope comes back
// as "Unknown error", code 35.
// @return array Example
// [
//
//	'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//	'balance' => 2,
//	'total' => 35.1,
//	'quantity' => 5,
//	'currency' => 'USD',
//	'discount' => 0.22,
//	'price' => 7.02
//
// ]
func OrderCalcMobile(countryId string, periodId string, quantity int, authorization string, coupon string, operatorId string, rotationId string) (map[string]interface{}, error) {
	return legacyClient().OrderCalcMobile(countryId, periodId, quantity, authorization, coupon, operatorId, rotationId)
}

func (c *Client) OrderCalcMobile(countryId string, periodId string, quantity int, authorization string, coupon string, operatorId string, rotationId string) (map[string]interface{}, error) {
	return c.orderCalc(c.prepareMobile(countryId, periodId, quantity, authorization, coupon, operatorId, rotationId))
}

// OrderCalcResident Calculate the order Resident
// Preliminary order calculation
// An error in warning must be corrected before placing an order.
// @param string tarifId - ObjectId string from reference/list/resident → tarifs[].id, OR the tariff
// code (ResidentTariffPlan.code, exact match). Careful: the code is NOT published by the reference —
// tarifs[] carries only id, name and personal — so unless you know the code from elsewhere, use the id.
// @param string coupon - optional, pass "" when not needed
// @return array Example
// [
//
//	'warning' => 'Insufficient funds. Total $2. Not enough $33.10',
//	'balance' => 2,
//	'total' => 35.1,
//	'quantity' => 5,
//	'currency' => 'USD',
//	'discount' => 0.22,
//	'price' => 7.02
//
// ]
func OrderCalcResident(tarifId string, coupon string) (map[string]interface{}, error) {
	return legacyClient().OrderCalcResident(tarifId, coupon)
}

func (c *Client) OrderCalcResident(tarifId string, coupon string) (map[string]interface{}, error) {
	return c.orderCalc(c.prepareResident(tarifId, coupon))
}

// OrderMakeIpv4 Create an order IPv4
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param string countryId - ObjectId string from reference/list (v2 ids are not numbers) OR the
// country code: a *Id value that is not an ObjectId is resolved as a code
// (ClientApiService.normalizeOrderReferenceCodes). The code is the alpha3 from
// reference/list → country[].alpha3, uppercased by the server ("USA").
// @param string periodId - ObjectId string from reference/list OR the period code, lowercased by
// the server ("1m", "3m"). reference/list publishes only period[].id and period[].name — the code
// itself is not part of the reference, so hardcode it or use periodId.
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string customTargetName - required for ipv4/ipv6/isp and for unresolved mix/mix_isp
// @return array Example
// [
//
//	'orderId' => '68b1f0c4e13a4c0f1a2b3c4d', // ObjectId string, never a number
//	'total' => 35.1,
//	'balance' => 10.19
//
// ]
func OrderMakeIpv4(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return legacyClient().OrderMakeIpv4(countryId, periodId, quantity, authorization, coupon, customTargetName)
}

func (c *Client) OrderMakeIpv4(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return c.orderMake(c.prepareRegular("ipv4", countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderMakeIsp Create an order ISP
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param string countryId - ObjectId string from reference/list (v2 ids are not numbers) OR the
// country code: a *Id value that is not an ObjectId is resolved as a code
// (ClientApiService.normalizeOrderReferenceCodes). The code is the alpha3 from
// reference/list → country[].alpha3, uppercased by the server ("USA").
// @param string periodId - ObjectId string from reference/list OR the period code, lowercased by
// the server ("1m", "3m"). reference/list publishes only period[].id and period[].name — the code
// itself is not part of the reference, so hardcode it or use periodId.
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string customTargetName - required for ipv4/ipv6/isp and for unresolved mix/mix_isp
// @return array Example
// [
//
//	'orderId' => '68b1f0c4e13a4c0f1a2b3c4d', // ObjectId string, never a number
//	'total' => 35.1,
//	'balance' => 10.19
//
// ]
func OrderMakeIsp(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return legacyClient().OrderMakeIsp(countryId, periodId, quantity, authorization, coupon, customTargetName)
}

func (c *Client) OrderMakeIsp(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return c.orderMake(c.prepareRegular("isp", countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderMakeMix Create an order Mix
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// For mix, countryId doubles as the MIX package selector — see OrderCalcMix.
// @param string countryId - ObjectId string from reference/list (v2 ids are not numbers) OR the
// country code: a *Id value that is not an ObjectId is resolved as a code
// (ClientApiService.normalizeOrderReferenceCodes). The code is the alpha3 from
// reference/list → country[].alpha3, uppercased by the server ("USA").
// @param string periodId - ObjectId string from reference/list OR the period code, lowercased by
// the server ("1m", "3m"). reference/list publishes only period[].id and period[].name — the code
// itself is not part of the reference, so hardcode it or use periodId.
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string customTargetName - required for ipv4/ipv6/isp and for unresolved mix/mix_isp
// @return array Example
// [
//
//	'orderId' => '68b1f0c4e13a4c0f1a2b3c4d', // ObjectId string, never a number
//	'total' => 35.1,
//	'balance' => 10.19
//
// ]
func OrderMakeMix(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return legacyClient().OrderMakeMix(countryId, periodId, quantity, authorization, coupon, customTargetName)
}

func (c *Client) OrderMakeMix(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) (map[string]interface{}, error) {
	return c.orderMake(c.prepareRegular("mix", countryId, periodId, quantity, authorization, coupon, customTargetName))
}

// OrderMakeIpv6 Create an order IPv6
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param string countryId - ObjectId string from reference/list (v2 ids are not numbers) OR the
// country code: a *Id value that is not an ObjectId is resolved as a code
// (ClientApiService.normalizeOrderReferenceCodes). The code is the alpha3 from
// reference/list → country[].alpha3, uppercased by the server ("USA").
// @param string periodId - ObjectId string from reference/list OR the period code, lowercased by
// the server ("1m", "3m"). reference/list publishes only period[].id and period[].name — the code
// itself is not part of the reference, so hardcode it or use periodId.
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string customTargetName - required for ipv4/ipv6/isp and for unresolved mix/mix_isp
// @param string protocol - http | https | socks | socks5, or "" for the default (https)
// @return array Example
// [
//
//	'orderId' => '68b1f0c4e13a4c0f1a2b3c4d', // ObjectId string, never a number
//	'total' => 35.1,
//	'balance' => 10.19
//
// ]
func OrderMakeIpv6(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string, protocol string) (map[string]interface{}, error) {
	return legacyClient().OrderMakeIpv6(countryId, periodId, quantity, authorization, coupon, customTargetName, protocol)
}

func (c *Client) OrderMakeIpv6(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string, protocol string) (map[string]interface{}, error) {
	return c.orderMake(c.prepareIpv6(countryId, periodId, quantity, authorization, coupon, customTargetName, protocol))
}

// OrderMakeMobile Create an order Mobile
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param string countryId - ObjectId string OR country code (alpha3: "USA")
// @param string periodId - ObjectId string OR period code ("1m", "3m")
// @param integer quantity
// @param string authorization - optional, pass "" when not needed
// @param string coupon - optional, pass "" when not needed
// @param string operatorId - ObjectId string OR operator tag
// @param string rotationId - rotation interval in MINUTES as a numeric string ("5", "0" = By Link);
// never a code — see OrderCalcMobile
// @return array Example
// [
//
//	'orderId' => '68b1f0c4e13a4c0f1a2b3c4d', // ObjectId string, never a number
//	'total' => 35.1,
//	'balance' => 10.19
//
// ]
func OrderMakeMobile(countryId string, periodId string, quantity int, authorization string, coupon string, operatorId string, rotationId string) (map[string]interface{}, error) {
	return legacyClient().OrderMakeMobile(countryId, periodId, quantity, authorization, coupon, operatorId, rotationId)
}

func (c *Client) OrderMakeMobile(countryId string, periodId string, quantity int, authorization string, coupon string, operatorId string, rotationId string) (map[string]interface{}, error) {
	return c.orderMake(c.prepareMobile(countryId, periodId, quantity, authorization, coupon, operatorId, rotationId))
}

// OrderMakeResident Create an order Resident
// Attention! Calling this method will deduct $ from your balance!
// The parameters are identical to the /order/calc method. Practice there before calling the /order/make method.
// @param string tarifId - ObjectId string from reference/list/resident → tarifs[].id, OR the tariff
// code (ResidentTariffPlan.code, exact match). Careful: the code is NOT published by the reference —
// tarifs[] carries only id, name and personal — so unless you know the code from elsewhere, use the id.
// @param string coupon - optional, pass "" when not needed
// @return array Example
// [
//
//	'orderId' => '68b1f0c4e13a4c0f1a2b3c4d', // ObjectId string, never a number
//	'total' => 35.1,
//	'balance' => 10.19
//
// ]
func OrderMakeResident(tarifId string, coupon string) (map[string]interface{}, error) {
	return legacyClient().OrderMakeResident(tarifId, coupon)
}

func (c *Client) OrderMakeResident(tarifId string, coupon string) (map[string]interface{}, error) {
	return c.orderMake(c.prepareResident(tarifId, coupon))
}

func (c *Client) prepareRegular(sectionCode string, countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string) map[string]interface{} {
	data := map[string]interface{}{
		"sectionCode":      sectionCode,
		"countryId":        countryId,
		"periodId":         periodId,
		"quantity":         quantity,
		"authorization":    authorization,
		"coupon":           coupon,
		"customTargetName": customTargetName,
	}
	return c.withPayment(data)
}

func (c *Client) prepareIpv6(countryId string, periodId string, quantity int, authorization string, coupon string, customTargetName string, protocol string) map[string]interface{} {
	data := map[string]interface{}{
		"sectionCode":      "ipv6",
		"countryId":        countryId,
		"periodId":         periodId,
		"quantity":         quantity,
		"authorization":    authorization,
		"coupon":           coupon,
		"customTargetName": customTargetName,
		"protocol":         protocol,
	}
	return c.withPayment(data)
}

// prepareMobile — коды кладём прямо в *Id-поля: сервер резолвит значение, не являющееся id,
// как код (normalizeOrderReferenceCodes), поэтому *Code-поля здесь не нужны. Исключение —
// rotationId: это минуты числом ("5", "0" = By Link), у него нет ни кода, ни фолбэка.
func (c *Client) prepareMobile(countryId string, periodId string, quantity int, authorization string, coupon string, operatorId string, rotationId string) map[string]interface{} {
	data := map[string]interface{}{
		"sectionCode":       "mobile",
		"mobileServiceType": "dedicated",
		"countryId":         countryId,
		"periodId":          periodId,
		"quantity":          quantity,
		"authorization":     authorization,
		"coupon":            coupon,
		"operatorId":        operatorId,
		"rotationId":        rotationId,
	}
	return c.withPayment(data)
}

func (c *Client) prepareResident(tarifId string, coupon string) map[string]interface{} {
	data := map[string]interface{}{
		"sectionCode": "resident",
		"tarifId":     tarifId,
		"coupon":      coupon,
	}
	return c.withPayment(data)
}

// withPayment — paymentCode/paymentId заказа. Здесь код действительно резолвится сервером
// (ClientApiService.normalizeOrderReferenceCodes), в отличие от balance/add.
func (c *Client) withPayment(data map[string]interface{}) map[string]interface{} {
	if code := c.GetPaymentCode(); code != "" {
		data["paymentCode"] = code
	} else if id := c.GetPaymentID(); id != "" {
		data["paymentId"] = id
	}
	return data
}

// Calculate the order
// requireCustomTargetName повторяет проверку client-api v1: для ipv4/ipv6/isp у заказа
// обязательна цель. В v1 её можно было задать targetId+targetSectionId либо своим текстом,
// в v2 остался только customTargetName. mix освобождён, если задан mixId/mixCode —
// иначе сервер резолвит тип в ipv4 и цель снова обязательна.
// Проверяем локально, чтобы не платить сетевым запросом за "Incorrect goal" (код 14).
func requireCustomTargetName(data map[string]interface{}) error {
	section, _ := data["sectionCode"].(string)
	switch section {
	case "ipv4", "ipv6", "isp", "mix", "mix_isp":
	default:
		return nil
	}
	if isMixSection(section) && mixResolvedLocally(
		targetString(data["mixId"]),
		targetString(data["mixCode"]),
		targetString(data["countryId"]),
		targetQuantity(data["quantity"]),
	) {
		return nil
	}
	if s, _ := data["customTargetName"].(string); strings.TrimSpace(s) != "" {
		return nil
	}
	return fmt.Errorf("customTargetName is required for %s orders (client api returns \"Incorrect goal\", code 14)", section)
}

// isMixSection — секции, которые сервер способен резолвить в mix-пакет.
func isMixSection(sectionCode string) bool {
	switch sectionCode {
	case "mix", "mix_isp":
		return true
	}
	return false
}

// mixResolvedLocally повторяет ClientApiService.parseMixSelection: сервер распознаёт mix
// не только по mixId/mixCode, но и через countryId — строкой "packageId:quantity" либо
// countryId=packageId вместе с quantity. Если mix распознан, requiresClientApiGoal
// возвращает false и цель НЕ требуется. Раньше здесь проверялись только mixId/mixCode,
// из-за чего legacy-путь OrderCalcMix/OrderMakeMix (пакет уезжает в countryId) блокировался
// локально и запрос вообще не уходил на сервер.
//
// Сомнительные случаи трактуем в пользу отправки: лишний сетевой запрос дешевле,
// чем отказ SDK на валидном заказе.
func mixResolvedLocally(mixID, mixCode, countryID string, quantity int64) bool {
	if strings.TrimSpace(mixID) != "" || strings.TrimSpace(mixCode) != "" {
		return true
	}
	countryID = strings.TrimSpace(countryID)
	if countryID == "" {
		return false
	}
	if strings.Contains(countryID, ":") {
		return true
	}
	return quantity > 0
}

func targetString(v interface{}) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	default:
		return fmt.Sprintf("%v", s)
	}
}

func targetQuantity(v interface{}) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i
		}
	}
	return 0
}

// deleteResultMap приводит data delete-эндпоинтов к map.
// Сервер отдаёт здесь СТРОКУ, а не объект: resident/list/delete → "delete",
// residentsubuser/delete и residentsubuser/list/delete → JSON внутри строки
// (например {"status":"not-found"} при конверте status="success"). Раньше обёртки
// возвращали result.Map, который в этом случае nil, — то есть неудавшееся удаление
// было неотличимо от успешного.
func deleteResultMap(result ResultData) map[string]interface{} {
	if result.Map != nil {
		return result.Map
	}
	if result.Value == nil {
		return nil
	}
	s, ok := result.Value.(string)
	if !ok {
		return map[string]interface{}{"status": result.Value}
	}
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "{") {
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
			return parsed
		}
	}
	return map[string]interface{}{"status": trimmed}
}

func (c *Client) orderCalc(data map[string]interface{}) (map[string]interface{}, error) {
	if err := requireCustomTargetName(data); err != nil {
		return nil, err
	}
	result, err := c.Request(http.MethodPost, "order/calc", data)
	return result.Map, err
}

// withGenerateAuth generateAuth is accepted by order/make only, order/calc silently drops it
func (c *Client) withGenerateAuth(data map[string]interface{}) map[string]interface{} {
	if _, exists := data["generateAuth"]; !exists {
		data["generateAuth"] = c.GetGenerateAuth()
	}
	return data
}

// Create an order
func (c *Client) orderMake(data map[string]interface{}) (map[string]interface{}, error) {
	if err := requireCustomTargetName(data); err != nil {
		return nil, err
	}
	result, err := c.Request(http.MethodPost, "order/make", c.withGenerateAuth(data))
	return result.Map, err
}

/////////////////////////////// Proxy ///////////////////////////////

// ProxyList Proxies list
// @param string proxyType - ipv4 | ipv6 | mobile | isp | mix | ""
// id, order_id, order_number and basket_id are ObjectId strings (ProxyListItemClientDto
// declares all of them as String) — never parse them as numbers.
// @return array Example
// [
//
//	'id' => '68b1f0c4e13a4c0f1a2b3c4d',
//	'order_id' => '68b1f0c4e13a4c0f1a2b3c11',
//	'order_number' => 'LH-100500',
//	'basket_id' => '68b1f0c4e13a4c0f1a2b3c22',
//	'ip' => 127.0.0.2,
//	'ip_only' => 127.0.0.2,
//	'protocol' => 'HTTP',
//	'port_socks' => 50101,
//	'port_http' => 50100,
//	'login' => 'login',
//	'password' => 'password',
//	'auth_ip' => '',
//	'rotation' => '',
//	'link_reboot' => '#',
//	'country' => 'France',
//	'country_alpha3' => 'FRA',
//	'status' => 'Active',
//	'status_type' => 'ACTIVE',
//	'can_prolong' => true,
//	'date_start' => '26.06.2023',
//	'date_end' => '26.07.2023',
//	'comment' => '',
//	'auto_renew' => 'Y',
//	'auto_renew_period' => '',
//	'is_uptime' => false
//
// ]
func ProxyList(proxyType string) (map[string]interface{}, error) {
	return legacyClient().ProxyList(proxyType)
}

func (c *Client) ProxyList(proxyType string) (map[string]interface{}, error) {
	return c.ListProxies(proxyType, ProxyListOptions{})
}

// ProxyDownload Proxy export of certain type in txt or csv.
// The response is a file (Content-Disposition: attachment), not the usual envelope.
// @param string type - ipv4 | ipv6 | mobile | isp | mix | resident | subresident
// $param string ext - txt | csv | custom template with %ip%/%port%/%login%/%password%/...
// $param string proto - https | socks5 | ”
// $param string listId - only for resident, if not set - will return ip from all sheets
//
// package_key is NOT a parameter here: only proxy/download/subresident reads it. The literal
// route proxy/download/resident is served by ResidentUserController.downloadProxyList, which
// accepts listId/id/ext/maxLine only and exports the parent package. Use
// DownloadProxies("subresident", ProxyDownloadOptions{PackageKey: ...}) for a subpackage.
//
// An oversized or malformed ext (see assertExt) is rejected by the server with a bare
// HTTP 400 plain-text body, outside the envelope — the legacy signature swallows that and
// returns "", ProxyDownloadE returns the error.
// @return string Example
// login:password@127.0.0.2:50100
func ProxyDownload(proxyType string, ext string, proto string, listId string) string {
	value, _ := ProxyDownloadE(proxyType, ext, proto, listId)
	return value
}

func ProxyDownloadE(proxyType string, ext string, proto string, listId string) (string, error) {
	return legacyClient().ProxyDownload(proxyType, ext, proto, listId)
}

func (c *Client) ProxyDownload(proxyType string, ext string, proto string, listId string) (string, error) {
	if err := assertExt(ext); err != nil {
		return "", err
	}
	query := url.Values{"ext": {ext}, "proto": {proto}, "listId": {listId}}
	data, err := c.RequestBinary("proxy/download/" + url.PathEscape(proxyType) + "?" + query.Encode())
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ProxyCommentSet Set proxy comment
// @param array ids Any ObjectId string, regardless of the type of proxy
// @param string comment
// @return integer Count updated proxy
func ProxyCommentSet(ids string, comment string) (map[string]interface{}, error) {
	return legacyClient().ProxyCommentSet(ids, comment)
}

func (c *Client) ProxyCommentSet(ids string, comment string) (map[string]interface{}, error) {
	return c.SetProxyComment(splitIDs(ids), comment)
}

func splitIDs(ids string) []string {
	parts := strings.Split(ids, ",")
	result := make([]string, 0, len(parts))
	for _, id := range parts {
		if id = strings.TrimSpace(id); id != "" {
			result = append(result, id)
		}
	}
	return result
}

/////////////////////////////// Resident ///////////////////////////////

// ResidentPackage Package Information
// Remaining traffic, end date. Traffic values are strings in the response
// (PackageResponseDto declares traffic_limit/traffic_usage/traffic_left as String),
// expired_at is a formatted string "d.m.Y H:i:s" — unlike the SUBPACKAGE expired_at,
// which is a PHP date object (see ResidentsubuserPackages).
// @return array Example
// [
//
//	'package_key': 'abc123def456',
//	'is_active': true,
//	'rotation': 60,
//	'tarif_id': 2,
//	'traffic_limit': '7516192768',
//	'traffic_usage': '10',
//	'traffic_left': '7516192758',
//	'expired_at': "31.12.2025 23:59:59",
//	'auto_renew': false
//
// ]
func ResidentPackage() (map[string]interface{}, error) { return legacyClient().ResidentPackage() }

func (c *Client) ResidentPackage() (map[string]interface{}, error) {
	result, err := c.Request(http.MethodGet, "resident/package", nil)
	return result.Map, err
}

// ResidentGeo Full geo structure (countries -> regions -> cities -> ISPs).
//
// The server answers with a JSON FILE — Content-Type application/json,
// Content-Disposition: attachment; filename="geo.json" (ResidentUserApiService.downloadGeoFile).
// It is NOT a zip archive, despite what the older SDK docs claimed: the body is
// pretty-printed JSON you can unmarshal directly.
// @return binary (geo.json contents)
func ResidentGeo() []byte {
	data, _ := ResidentGeoE()
	return data
}

func ResidentGeoE() ([]byte, error) { return legacyClient().ResidentGeo() }

func (c *Client) ResidentGeo() ([]byte, error) { return c.RequestBinary("resident/geo") }

// ResidentList List of existing ip list in a package
// You can download the list via endpoint /proxy/download/resident?listId=123
// Element ids are numeric (ListItemResponseDto.id is Long) — resident list ids are the one
// exception to the "ids are ObjectId strings" rule of v2.
// @return array
func ResidentList() ([]interface{}, error) { return legacyClient().ResidentLists() }

// ResidentListAdd Create list in package
// You can download the list via endpoint /proxy/download/resident?listId=123
// @return array
func ResidentListAdd(title string, whitelist string, country string, region string, city string, isp string) (map[string]interface{}, error) {
	return legacyClient().ResidentListAdd(title, whitelist, country, region, city, isp)
}

func (c *Client) ResidentListAdd(title string, whitelist string, country string, region string, city string, isp string) (map[string]interface{}, error) {
	return c.CreateResidentList(ResidentListRequest{
		Title:     title,
		Whitelist: whitelist,
		Geo:       GeoFilter{Country: country, Region: region, City: city, ISP: isp},
	})
}

// ResidentListRename Rename list in user package
// @param int64 id - listId (ListRenameRequestDto.id is Long)
// @param string title
// @return array Updated list model
func ResidentListRename(id int64, title string) (map[string]interface{}, error) {
	return legacyClient().ResidentListRename(id, title)
}

func (c *Client) ResidentListRename(id int64, title string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"id":    id,
		"title": title,
	}
	result, err := c.Request(http.MethodPost, "resident/list/rename", data)
	return result.Map, err
}

// ResidentListDelete Remove list from user package
// @param int64 id - listId (ListDeleteRequestDto.id is Long)
// data приходит СТРОКОЙ "delete", а не объектом — см. deleteResultMap.
// @return array Delete result
func ResidentListDelete(id int64) (map[string]interface{}, error) {
	return legacyClient().ResidentListDelete(id)
}

func (c *Client) ResidentListDelete(id int64) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"id": id,
	}
	result, err := c.Request(http.MethodDelete, "resident/list/delete", data)
	return deleteResultMap(result), err
}

//////////////////////////// Resident Subusers ////////////////////////////

// ResidentsubuserPackages Package Information
// Remaining traffic, expiration date.
//
// Внимание на expired_at: у СУБПАКЕТА это не строка, а ОБЪЕКТ PHP-даты
// (SubPackageDto.expired_at имеет тип PhpDateDto) — {date, timezone_type, timezone}.
// Строкой "d.m.Y H:i:s" приходит только expired_at родительского пакета
// (resident/package). Traffic-поля здесь тоже строки, а не числа.
//
// @return array Example
// [[
//
//	'package_key': 'f9ea7063d7a699b12b3e',
//	'is_active': true,
//	'is_link_date': false,
//	'rotation': 60,
//	'traffic_limit': '7516192768',
//	'traffic_usage': '10',
//	'traffic_left': '7516192758',
//	'expired_at': ['date' => '2025-12-31 23:59:59.000000', 'timezone_type' => 3, 'timezone' => 'UTC']
//
// ]]
func ResidentsubuserPackages() ([]interface{}, error) {
	return legacyClient().ResidentsubuserPackages()
}

func (c *Client) ResidentsubuserPackages() ([]interface{}, error) {
	result, err := c.Request(http.MethodGet, "residentsubuser/packages", nil)
	return result.Slice, err
}

// ResidentsubuserCreate Create subpackage
// Creates a subpackage from your current active tariff with traffic.
// The validity period must not exceed the current active tariff validity period with traffic
// The amount of traffic is specified in bytes.
// The specified amount of traffic will be reserved from the current active tariff with traffic, but not more than is available in the active tariff with traffic.
// Rotation can take values:
// -1 No rotation
// 0 Every request
// 1...3600 interval in seconds
//
// @param integer rotation -1...3600
// @param integer traffic_limit - in bytes (sent as a string, the server field is String)
// @param string expired_at
// @return array Example (expired_at приходит объектом PHP-даты, см. ResidentsubuserPackages)
// [
//
//	'package_key': 'f9ea7063d7a699b12b3e',
//	'is_active': true,
//	'rotation': 60,
//	'traffic_limit': '7516192768',
//	'traffic_usage': '10',
//	'expired_at': ['date' => '2025-12-31 23:59:59.000000', 'timezone_type' => 3, 'timezone' => 'UTC']
//
// ]
func ResidentsubuserCreate(rotation int, traffic_limit int, expired_at string) (map[string]interface{}, error) {
	return legacyClient().ResidentsubuserCreate(rotation, traffic_limit, expired_at)
}

func (c *Client) ResidentsubuserCreate(rotation int, traffic_limit int, expired_at string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"rotation":      rotation,
		"traffic_limit": fmt.Sprint(traffic_limit),
		"expired_at":    expired_at,
	}
	result, err := c.Request(http.MethodPost, "residentsubuser/create", data)
	return result.Map, err
}

// ResidentsubuserUpdate Update subpackage
// When deactivating a subpackage, the remaining traffic will stay in the subpackage.
// The validity period must not exceed the current active tariff validity period with traffic
// The total amount of traffic in bytes that should be in the subpackage is specified
// Rotation can take values:
// -1 No rotation
// 0 Every request
// 1...3600 interval in seconds
//
// @param integer rotation -1...3600
// @param integer traffic_limit - in bytes (sent as a string, the server field is String)
// @param string expired_at
// @param bool is_active
// @param string package_key
// @return array Example (expired_at приходит объектом PHP-даты, см. ResidentsubuserPackages)
// [
//
//	'package_key': 'f9ea7063d7a699b12b3e',
//	'is_active': true,
//	'rotation': 60,
//	'traffic_limit': '7516192768',
//	'traffic_usage': '10',
//	'expired_at': ['date' => '2025-12-31 23:59:59.000000', 'timezone_type' => 3, 'timezone' => 'UTC']
//
// ]
func ResidentsubuserUpdate(rotation int, traffic_limit int, expired_at string, is_active bool, package_key string) (map[string]interface{}, error) {
	return legacyClient().ResidentsubuserUpdate(rotation, traffic_limit, expired_at, is_active, package_key)
}

func (c *Client) ResidentsubuserUpdate(rotation int, traffic_limit int, expired_at string, is_active bool, package_key string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"rotation":      rotation,
		"traffic_limit": fmt.Sprint(traffic_limit),
		"expired_at":    expired_at,
		"is_active":     is_active,
		"package_key":   package_key,
	}
	result, err := c.Request(http.MethodPost, "residentsubuser/update", data)
	return result.Map, err
}

// ResidentsubuserDelete Delete subpackage
// When deleting, the unused remaining traffic of the subpackage will be returned.
//
// data приходит СТРОКОЙ с JSON внутри ({"status":"delete"}), а не объектом, — разбирается
// через deleteResultMap. При этом конверт может быть status="success", а внутри "not-found".
//
// @param string package_key
// @return array Delete result, e.g. ['status' => 'delete']
func ResidentsubuserDelete(package_key string) (map[string]interface{}, error) {
	return legacyClient().ResidentsubuserDelete(package_key)
}

func (c *Client) ResidentsubuserDelete(package_key string) (map[string]interface{}, error) {
	result, err := c.DeleteResidentSubuser(package_key)
	return deleteResultMap(result), err
}

// ResidentsubuserLists List of existing IP lists in the subpackage
// The list can be downloaded via the endpoint
// /proxy/download/subresident?listId=123&package_key=123456789 — package_key works only on
// the subresident route, /proxy/download/resident ignores it and exports the parent package.
// @param string package_key
// @return array
func ResidentsubuserLists(package_key string) ([]interface{}, error) {
	return legacyClient().ResidentSubuserLists(package_key)
}

// ResidentsubuserListAdd Create list in subpackage
// You can download the list via endpoint
// /proxy/download/subresident?listId=123&package_key=123456789
// @return array
func ResidentsubuserListAdd(package_key string, title string, whitelist string, country string, region string, city string, isp string) (map[string]interface{}, error) {
	return legacyClient().ResidentsubuserListAdd(package_key, title, whitelist, country, region, city, isp)
}

func (c *Client) ResidentsubuserListAdd(package_key string, title string, whitelist string, country string, region string, city string, isp string) (map[string]interface{}, error) {
	return c.CreateResidentSubuserList(ResidentSubuserListRequest{
		PackageKey: package_key,
		Title:      title,
		Whitelist:  whitelist,
		Geo:        GeoFilter{Country: country, Region: region, City: city, ISP: isp},
	})
}

// ResidentsubuserListRename Rename list in subuser package
// @param integer id - listId (RenameSubPackageListRequestDto.id is Integer)
// @param string title
// @param string package_key
// @return array Updated list model
func ResidentsubuserListRename(package_key string, id int, title string) (map[string]interface{}, error) {
	return legacyClient().ResidentsubuserListRename(package_key, id, title)
}

func (c *Client) ResidentsubuserListRename(package_key string, id int, title string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"package_key": package_key,
		"id":          id,
		"title":       title,
	}
	result, err := c.Request(http.MethodPost, "residentsubuser/list/rename", data)
	return result.Map, err
}

// ResidentsubuserListDelete Remove list from subuser package
// @param string id - listId. Здесь именно СТРОКА: DeleteSubPackageListRequestDto.id
// объявлен как String с @NotBlank, в отличие от rename/rotation того же субпакета
// (там Integer) и от resident/list/* (там Long).
// @param string package_key
// @return array Delete result, e.g. ['status' => 'delete'] или ['status' => 'not-found']
func ResidentsubuserListDelete(package_key string, id string) (map[string]interface{}, error) {
	return legacyClient().ResidentsubuserListDelete(package_key, id)
}

func (c *Client) ResidentsubuserListDelete(package_key string, id string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"package_key": package_key,
		"id":          id,
	}
	result, err := c.Request(http.MethodDelete, "residentsubuser/list/delete", data)
	return deleteResultMap(result), err
}

/////////////////////////////// v2 additions ///////////////////////////////

// AuthAdd Create login/password authorization
func AuthAdd(orderNumber string, generateAuth string) (map[string]interface{}, error) {
	return legacyClient().AuthAdd(orderNumber, generateAuth)
}

func (c *Client) AuthAdd(orderNumber string, generateAuth string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"orderNumber":  orderNumber,
		"generateAuth": generateAuth,
	}
	result, err := c.Request(http.MethodPost, "auth/add", data)
	return result.Map, err
}

// AuthAddIp Create IP authorization
func AuthAddIp(orderNumber string, ip string) (map[string]interface{}, error) {
	return legacyClient().AuthAddIp(orderNumber, ip)
}

func (c *Client) AuthAddIp(orderNumber string, ip string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"orderNumber": orderNumber,
		"ip":          ip,
	}
	result, err := c.Request(http.MethodPost, "auth/add/ip", data)
	return result.Map, err
}

// AuthDelete Delete authorization
// @param string id - ObjectId string of the authorization
func AuthDelete(id string) (map[string]interface{}, error) {
	return legacyClient().AuthDelete(id)
}

func (c *Client) AuthDelete(id string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"id": id,
	}
	result, err := c.Request(http.MethodDelete, "auth/delete", data)
	return result.Map, err
}

// Replacement reasons accepted by proxy/replace (enum ProxyReplaceType on the server).
const (
	ProxyReplaceReasonNotWork           = "NOT_WORK"
	ProxyReplaceReasonIncorrectLocation = "INCORRECT_LOCATION"
	ProxyReplaceReasonCantChangeNetwork = "CANT_CHANGE_NETWORK"
	ProxyReplaceReasonLowSpeed          = "LOW_SPEED"
	ProxyReplaceReasonCustom            = "CUSTOM"
)

// ProxyReplaceReasons — допустимые значения параметра type запроса proxy/replace.
var ProxyReplaceReasons = []string{
	ProxyReplaceReasonNotWork,
	ProxyReplaceReasonIncorrectLocation,
	ProxyReplaceReasonCantChangeNetwork,
	ProxyReplaceReasonLowSpeed,
	ProxyReplaceReasonCustom,
}

// assertProxyReplaceReason повторяет две первые проверки ClientApiService.replaceProxies:
// ProxyReplaceType.fromString (регистр не важен — сервер делает value.toUpperCase()) и
// обязательный непустой comment при CUSTOM (иначе ошибка "Set comment", code 503).
func assertProxyReplaceReason(reason string, comment string) error {
	normalized := strings.ToUpper(strings.TrimSpace(reason))
	valid := false
	for _, allowed := range ProxyReplaceReasons {
		if normalized == allowed {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("proxy/replace: type must be a replacement reason, one of %s (got %q)",
			strings.Join(ProxyReplaceReasons, " / "), reason)
	}
	if normalized == ProxyReplaceReasonCustom && strings.TrimSpace(comment) == "" {
		return fmt.Errorf("proxy/replace: comment is required when type is %s (the server replies \"Set comment\", code 503)", ProxyReplaceReasonCustom)
	}
	return nil
}

// ProxyReplace Replace proxy IPs.
//
// ВНИМАНИЕ: replaceReason (поле type в запросе) — это ПРИЧИНА замены, а НЕ тип прокси.
// Сервер валидирует его через enum ProxyReplaceType:
// NOT_WORK | INCORRECT_LOCATION | CANT_CHANGE_NETWORK | LOW_SPEED | CUSTOM,
// и превращает в текст комментария заявки (ProxyReplaceType.legacyComment). Тип прокси здесь
// вообще не передаётся: он определяется по первому IP из ids (ipAddresses[0].proxyTypeId).
// При CUSTOM обязателен непустой comment, при остальных причинах comment необязателен.
//
// @param ids - ObjectId strings: одна строка с запятыми либо []string
// @param replaceReason - NOT_WORK | INCORRECT_LOCATION | CANT_CHANGE_NETWORK | LOW_SPEED | CUSTOM
// @param comment - обязателен только для CUSTOM
func ProxyReplace(ids interface{}, replaceReason string, comment string) (map[string]interface{}, error) {
	return legacyClient().ProxyReplace(ids, replaceReason, comment)
}

func (c *Client) ProxyReplace(ids interface{}, replaceReason string, comment string) (map[string]interface{}, error) {
	if err := assertProxyReplaceReason(replaceReason, comment); err != nil {
		return nil, err
	}
	data := map[string]interface{}{
		"ids":     normalizeIDs(ids),
		"type":    replaceReason,
		"comment": comment,
	}
	result, err := c.Request(http.MethodPost, "proxy/replace", data)
	return result.Map, err
}

// ProxyDownloadResident Export the resident proxy list.
//
// Маршрут /proxy/download/resident литеральный (ResidentUserController.downloadProxyList) и
// принимает только listId (алиас id), ext и maxLine. package_key он ИГНОРИРУЕТ — выгрузится
// родительский пакет. Для листа субпакета нужен другой маршрут:
// DownloadProxies("subresident", ProxyDownloadOptions{ListID: ..., PackageKey: ...}).
//
// Ответ — файл-attachment, а не конверт. Слишком длинный или запрещённый ext сервер отбивает
// голым HTTP 400 плайн-текстом (см. assertExt): legacy-подпись вернёт "",
// ProxyDownloadResidentE — ошибку.
func ProxyDownloadResident(id string, ext string, maxLine string) string {
	value, _ := ProxyDownloadResidentE(id, ext, maxLine)
	return value
}

func ProxyDownloadResidentE(id string, ext string, maxLine string) (string, error) {
	return legacyClient().ProxyDownloadResident(id, ext, maxLine)
}

func (c *Client) ProxyDownloadResident(id string, ext string, maxLine string) (string, error) {
	if err := assertExt(ext); err != nil {
		return "", err
	}
	query := url.Values{"id": {id}, "ext": {ext}}
	if maxLine != "" {
		query.Set("maxLine", maxLine)
	}
	data, err := c.RequestBinary("proxy/download/resident?" + query.Encode())
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ResidentConsumption Traffic consumption of the resident package
func ResidentConsumption(filter map[string]interface{}) (map[string]interface{}, error) {
	return legacyClient().ResidentConsumption(filter)
}

func (c *Client) ResidentConsumption(filter map[string]interface{}) (map[string]interface{}, error) {
	if filter == nil {
		filter = map[string]interface{}{}
	}
	result, err := c.Request(http.MethodPost, "resident/consumption", filter)
	return result.Map, err
}

// ResidentTrafficDetails Detailed traffic statistics of the resident package.
//
// Ключ пакета здесь называется packageKey ЛИБО key — НЕ package_key
// (ResidentUserApiService.getTrafficDetails: request.get("packageKey") ?: request.get("key")).
// Он обязателен: без него сервер отвечает ошибкой "key is required".
// Остальные фильтры: login, date_start, date_end.
func ResidentTrafficDetails(filter map[string]interface{}) (map[string]interface{}, error) {
	return legacyClient().ResidentTrafficDetails(filter)
}

func (c *Client) ResidentTrafficDetails(filter map[string]interface{}) (map[string]interface{}, error) {
	if filter == nil {
		filter = map[string]interface{}{}
	}
	result, err := c.Request(http.MethodPost, "resident/traffic/details", filter)
	return result.Map, err
}

// ResidentGeoIsp Database of ISP codes.
// Как и resident/geo, это ФАЙЛ-attachment с JSON внутри (isp.json), а не конверт.
func ResidentGeoIsp() []byte {
	data, _ := ResidentGeoIspE()
	return data
}

func ResidentGeoIspE() ([]byte, error) { return legacyClient().ResidentGeoIsp() }

func (c *Client) ResidentGeoIsp() ([]byte, error) { return c.RequestBinary("resident/geo/isp") }

// ResidentGeoCount Number of available IPs by geo
func ResidentGeoCount() ([]interface{}, error) { return legacyClient().ResidentGeoCount() }

func (c *Client) ResidentGeoCount() ([]interface{}, error) {
	result, err := c.Request(http.MethodGet, "resident/geo/count", nil)
	return result.Slice, err
}

// ResidentListRotation Change the rotation interval of a list
// rotation: -1 sticky, 0 per request, 1-3600 seconds
// @param int64 id - listId (ListRotationRequestDto.id is Long)
func ResidentListRotation(id int64, rotation int) (map[string]interface{}, error) {
	return legacyClient().ResidentListRotation(id, rotation)
}

func (c *Client) ResidentListRotation(id int64, rotation int) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"id":       id,
		"rotation": rotation,
	}
	result, err := c.Request(http.MethodPost, "resident/list/rotation", data)
	return result.Map, err
}

// ResidentListTools Create the tools list for the package
func ResidentListTools() (map[string]interface{}, error) { return legacyClient().ResidentListTools() }

func (c *Client) ResidentListTools() (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPut, "resident/list/tools", nil)
	return result.Map, err
}

// ResidentsubuserListRotation Change the rotation interval of a list inside a subpackage
// @param integer id - listId (ChangeSubPackageListRotationRequestDto.id is Integer)
func ResidentsubuserListRotation(package_key string, id int, rotation int) (map[string]interface{}, error) {
	return legacyClient().ResidentsubuserListRotation(package_key, id, rotation)
}

func (c *Client) ResidentsubuserListRotation(package_key string, id int, rotation int) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"package_key": package_key,
		"id":          id,
		"rotation":    rotation,
	}
	result, err := c.Request(http.MethodPost, "residentsubuser/list/rotation", data)
	return result.Map, err
}

// ResidentsubuserListTools Create the tools list inside a subpackage
func ResidentsubuserListTools(package_key string) (map[string]interface{}, error) {
	return legacyClient().ResidentsubuserListTools(package_key)
}

func (c *Client) ResidentsubuserListTools(package_key string) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"package_key": package_key,
	}
	result, err := c.Request(http.MethodPut, "residentsubuser/list/tools", data)
	return result.Map, err
}

/////////////////////////////// Prolong ///////////////////////////////

func (c *Client) prepareLegacyProlong(ids interface{}, periodId string, coupon string) map[string]interface{} {
	data := map[string]interface{}{
		"ids":      normalizeIDs(ids),
		"periodId": periodId,
		"coupon":   coupon,
	}
	// Здесь paymentCode резолвится сервером (normalizeProlongReferenceCodes), в отличие
	// от balance/add.
	if code := c.GetPaymentCode(); code != "" {
		data["paymentCode"] = code
	} else if id := c.GetPaymentID(); id != "" {
		data["paymentId"] = id
	}
	return data
}

// normalizeIDs приводит "a,b,c" к []string. Ветка []int убрана осознанно: во всех DTO,
// которые принимают ids (ProlongRequestClientDto.ids, ProxyReplaceRequestClientDto.ids,
// ProxyCommentSetRequestClientDto.ids), это Set<String> с ObjectId внутри. Числовые id
// остались только в v1, и превращение []int в ["1","2"] лишь маскировало ошибку вызывающего:
// сервер всё равно не нашёл бы такие IP. Значения не-string теперь уходят как есть.
func normalizeIDs(ids interface{}) interface{} {
	switch value := ids.(type) {
	case string:
		return splitIDs(value)
	default:
		return ids
	}
}

// ProlongCalc Calculate the renewal
// @param proxyType - ipv4 | ipv6 | mobile | isp | mix
// @param ids - ObjectId strings of IP addresses
// @param periodId - ObjectId string OR the period code ("1m"): prolong resolves a non-id value as a
// code exactly like order/* (ClientApiService.normalizeProlongReferenceCodes)
func ProlongCalc(proxyType string, ids interface{}, periodId string, coupon string) (map[string]interface{}, error) {
	return legacyClient().ProlongCalc(proxyType, ids, periodId, coupon)
}

func (c *Client) ProlongCalc(proxyType string, ids interface{}, periodId string, coupon string) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "prolong/calc/"+url.PathEscape(proxyType), c.prepareLegacyProlong(ids, periodId, coupon))
	return result.Map, err
}

// ProlongMake Create a renewal order. Attention! Deducts money from the balance.
// @param proxyType - ipv4 | ipv6 | mobile | isp | mix
// @param ids - ObjectId strings of IP addresses
// @param periodId - ObjectId string OR the period code ("1m")
func ProlongMake(proxyType string, ids interface{}, periodId string, coupon string) (map[string]interface{}, error) {
	return legacyClient().ProlongMake(proxyType, ids, periodId, coupon)
}

func (c *Client) ProlongMake(proxyType string, ids interface{}, periodId string, coupon string) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "prolong/make/"+url.PathEscape(proxyType), c.prepareLegacyProlong(ids, periodId, coupon))
	if err != nil {
		return result.Map, err
	}
	return assertProlongMade(result.Map)
}

// assertProlongMade отделяет состоявшееся продление от несостоявшегося.
//
// При нехватке средств prolong/make отдаёт конверт status="error" с ПУСТЫМ errors[] и
// calc-данными в data (ProlongMakeResponseClientDto.ofInsufficientFunds, ClientApiService:3185) —
// ровно ту же форму, что легитимный warning у prolong/calc. Общий разбор конверта поэтому
// возвращал такие данные как успех, и несписанное продление выглядело как списанное.
// Признак успеха — непустой orderId (ProlongMakeDataClientDto), признак провала — warning.
//
// order/make этим не страдает: у OrderMakeResponseClientDto есть только ofSuccess/ofError,
// и при ошибке errors[] всегда заполнен.
func assertProlongMade(data map[string]interface{}) (map[string]interface{}, error) {
	if data == nil {
		return data, nil
	}
	if id, _ := data["orderId"].(string); strings.TrimSpace(id) != "" {
		return data, nil
	}
	warning, _ := data["warning"].(string)
	warning = strings.TrimSpace(warning)
	if warning == "" {
		warning = "prolong/make did not create an order (insufficient funds)"
	}
	return data, &APIError{
		HTTPStatus: http.StatusOK,
		Status:     "error",
		Errors:     []APIErrorItem{{Message: warning, Code: "0"}},
		Data:       data,
	}
}

/////////////////////////////// Typed v2 API ///////////////////////////////

// OrderRequest mirrors the current order/calc and order/make payload.
//
// Every *Id field accepts an ObjectId string OR the matching stable code. When the value is not a
// known id and the paired *Code field is empty, the server resolves it as a code
// (ClientApiService.normalizeOrderReferenceCodes). The fallback exists for PaymentID, CountryID,
// PeriodID, OperatorID, MixID and TarifID, so a code-first request needs no *Code field at all:
//
//	OrderRequest{SectionCode: "mobile", CountryID: "USA", PeriodID: "1m", Quantity: 1,
//	    OperatorID: "68b1f0c4e13a4c0f1a2b3c4d", RotationID: "5"}
//
// The *Code fields remain for callers who prefer to be explicit; filling both is pointless, and
// prepareOrder drops the id when the code of the same pair is set.
//
// RotationID is the exception: it is neither an id nor a code, but the rotation interval in minutes.
type OrderRequest struct {
	// CountryID — ObjectId OR country code (alpha3, uppercased by the server: "USA").
	CountryID string `json:"countryId,omitempty"`
	// CountryCode — alpha3, available from reference/list → country[].alpha3.
	CountryCode string `json:"countryCode,omitempty"`
	// SectionCode — ipv4 | ipv6 | isp | mobile | mix | mix_isp | resident | scraper.
	SectionCode string `json:"sectionCode"`
	// PeriodID — ObjectId OR period code (lowercased by the server: "1m", "3m").
	PeriodID string `json:"periodId,omitempty"`
	// PeriodCode — period code. reference/list publishes only period[].id and period[].name, so the
	// code has to come from elsewhere; when in doubt send the id.
	PeriodCode string `json:"periodCode,omitempty"`
	Coupon     string `json:"coupon,omitempty"`
	// PaymentID — ObjectId from balance/payments/list OR a payment code (PaymentSystem.code, or a
	// PaymentSystemTypes name such as "balance"). Note that balance/add resolves neither — see AddBalance.
	PaymentID string `json:"paymentId,omitempty"`
	// PaymentCode — payment code. balance/payments/list returns only id and name.
	PaymentCode      string `json:"paymentCode,omitempty"`
	Quantity         int    `json:"quantity,omitempty"`
	Authorization    string `json:"authorization,omitempty"`
	CustomTargetName string `json:"customTargetName,omitempty"`
	// MixID — ObjectId of the MIX package OR its tag; both come from reference/list/mix
	// (quantities[].id and country[].tag).
	MixID string `json:"mixId,omitempty"`
	// MixCode — MIX package tag, available as country[].tag under mix / mix_isp.
	MixCode  string `json:"mixCode,omitempty"`
	Uptime   bool   `json:"uptime,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	// MobileServiceType — shared | dedicated. Required for mobile; prepareOrder defaults it to dedicated.
	MobileServiceType string `json:"mobileServiceType,omitempty"`
	// OperatorID — ObjectId OR operator tag (MobileOperator.tag). The id comes from
	// reference/list/mobile → country[].operators.dedicated[].id (or .shared[]).
	OperatorID string `json:"operatorId,omitempty"`
	// OperatorCode — operator tag. Not published by reference/list, which returns only id and name.
	OperatorCode string `json:"operatorCode,omitempty"`
	// RotationID — rotation interval in MINUTES as a numeric string: "5", "10", "0" = By Link.
	// Not an id and not a code: the values come from reference/list/mobile →
	// operators[].rotations[].id, which is the minute count itself. The server does
	// `requestDto.rotationId as int`, so a non-numeric value like "5m" breaks the request
	// ("Unknown error", code 35).
	RotationID string `json:"rotationId,omitempty"`
	// RotationCode — redundant: the server only checks isInteger() and copies the value into
	// rotationId, with no reference lookup. A non-numeric value is rejected with
	// "Set existed [rotationCode] from reference". Prefer RotationID.
	RotationCode string `json:"rotationCode,omitempty"`
	// TarifID — ObjectId of a resident tariff from reference/list/resident → tarifs[].id, OR the
	// tariff code (exact match on ResidentTariffPlan.code).
	TarifID string `json:"tarifId,omitempty"`
	// TarifCode — resident tariff code. Not published by reference/list: tarifs[] carries only
	// id, name and personal.
	TarifCode    string `json:"tarifCode,omitempty"`
	GenerateAuth string `json:"generateAuth,omitempty"`
}

func (c *Client) prepareOrder(order OrderRequest, makeOrder bool) OrderRequest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if order.PaymentID == "" && order.PaymentCode == "" {
		order.PaymentID, order.PaymentCode = c.paymentID, c.paymentCode
	}
	if order.SectionCode == "mobile" && order.MobileServiceType == "" {
		order.MobileServiceType = "dedicated"
	}
	if order.CountryCode != "" {
		order.CountryID = ""
	}
	if order.PeriodCode != "" {
		order.PeriodID = ""
	}
	if order.PaymentCode != "" {
		order.PaymentID = ""
	}
	if order.MixCode != "" {
		order.MixID = ""
	}
	if order.OperatorCode != "" {
		order.OperatorID = ""
	}
	if order.RotationCode != "" {
		order.RotationID = ""
	}
	if order.TarifCode != "" {
		order.TarifID = ""
	}
	if makeOrder && order.GenerateAuth == "" {
		order.GenerateAuth = c.generateAuth
	}
	return order
}

// requireOrderTarget — та же проверка цели, что и requireCustomTargetName, но для типизированного пути.
func requireOrderTarget(order OrderRequest) error {
	switch order.SectionCode {
	case "ipv4", "ipv6", "isp", "mix", "mix_isp":
	default:
		return nil
	}
	if isMixSection(order.SectionCode) &&
		mixResolvedLocally(order.MixID, order.MixCode, order.CountryID, int64(order.Quantity)) {
		return nil
	}
	if strings.TrimSpace(order.CustomTargetName) != "" {
		return nil
	}
	return fmt.Errorf("customTargetName is required for %s orders (client api returns \"Incorrect goal\", code 14)", order.SectionCode)
}

func (c *Client) CalculateOrder(order OrderRequest) (map[string]interface{}, error) {
	if err := requireOrderTarget(order); err != nil {
		return nil, err
	}
	result, err := c.Request(http.MethodPost, "order/calc", c.prepareOrder(order, false))
	return result.Map, err
}

func (c *Client) MakeOrder(order OrderRequest) (map[string]interface{}, error) {
	if err := requireOrderTarget(order); err != nil {
		return nil, err
	}
	result, err := c.Request(http.MethodPost, "order/make", c.prepareOrder(order, true))
	return result.Map, err
}

type ProxyListOptions struct {
	Latest string
	// OrderID — ObjectId string заказа, не число.
	OrderID string
	Country string
	Ends    string
	// Page и PerPage принимает только маршрут с типом — proxy/list/{type}
	// (ProxyController.getProxiesByType). На proxy/list без типа они игнорируются.
	Page    int
	PerPage int
}

func (o ProxyListOptions) values() url.Values {
	values := url.Values{}
	if o.Latest != "" {
		values.Set("latest", o.Latest)
	}
	if o.OrderID != "" {
		values.Set("orderId", o.OrderID)
	}
	if o.Country != "" {
		values.Set("country", o.Country)
	}
	if o.Ends != "" {
		values.Set("ends", o.Ends)
	}
	if o.Page > 0 {
		values.Set("page", fmt.Sprint(o.Page))
	}
	if o.PerPage > 0 {
		values.Set("per_page", fmt.Sprint(o.PerPage))
	}
	return values
}

func (c *Client) ListProxies(proxyType string, options ProxyListOptions) (map[string]interface{}, error) {
	path := "proxy/list"
	if proxyType != "" {
		path += "/" + url.PathEscape(proxyType)
	}
	if query := options.values().Encode(); query != "" {
		path += "?" + query
	}
	result, err := c.Request(http.MethodGet, path, nil)
	return result.Map, err
}

type ProxyDownloadOptions struct {
	// Ext — txt | csv | шаблон с %ip%/%port%/%login%/%user%/%password%/%protocol%/%rotation_link%.
	// Ограничения см. assertExt: >250 символов либо CR/LF/'/'/'\' — голый HTTP 400 мимо конверта.
	Ext   string
	Proto string
	// ListID — только для resident/subresident.
	ListID string
	// PackageKey работает ТОЛЬКО с proxyType == "subresident". На литеральном маршруте
	// proxy/download/resident он игнорируется, и выгружается родительский пакет.
	PackageKey string
	Country    string
	Ends       string
}

// DownloadProxies — выгрузка прокси файлом. Ответ — attachment, а не конверт
// {status,data,errors}; ошибки доступа при этом приходят конвертом и разбираются в APIError.
func (c *Client) DownloadProxies(proxyType string, options ProxyDownloadOptions) ([]byte, error) {
	if err := assertExt(options.Ext); err != nil {
		return nil, err
	}
	if options.PackageKey != "" && !strings.EqualFold(strings.TrimSpace(proxyType), "subresident") {
		return nil, fmt.Errorf("package_key is only honored by proxy/download/subresident; %q ignores it and exports the parent package", proxyType)
	}
	query := url.Values{}
	if options.Ext != "" {
		query.Set("ext", options.Ext)
	}
	if options.Proto != "" {
		query.Set("proto", options.Proto)
	}
	if options.ListID != "" {
		query.Set("listId", options.ListID)
	}
	if options.PackageKey != "" {
		query.Set("package_key", options.PackageKey)
	}
	if options.Country != "" {
		query.Set("country", options.Country)
	}
	if options.Ends != "" {
		query.Set("ends", options.Ends)
	}
	path := "proxy/download/" + url.PathEscape(proxyType)
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return c.RequestBinary(path)
}

func (c *Client) SetProxyComment(ids []string, comment string) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "proxy/comment/set", map[string]interface{}{"ids": ids, "comment": comment})
	return result.Map, err
}

// ProlongRequest mirrors prolong/calc and prolong/make. normalizeProlongReferenceCodes has the same
// *Id-or-code fallback as orders, but only for PeriodID and PaymentID — the two reference fields
// prolong accepts.
type ProlongRequest struct {
	IDs               []string `json:"ids,omitempty"`
	OrderSeparatorIDs []string `json:"orderSeparatorIds,omitempty"`
	OrderSeparatorID  string   `json:"orderSeparatorId,omitempty"`
	Coupon            string   `json:"coupon,omitempty"`
	// PeriodID — ObjectId OR period code ("1m", lowercased by the server).
	PeriodID   string `json:"periodId,omitempty"`
	PeriodCode string `json:"periodCode,omitempty"`
	// PaymentID — ObjectId from balance/payments/list OR a payment code.
	PaymentID   string `json:"paymentId,omitempty"`
	PaymentCode string `json:"paymentCode,omitempty"`
}

func (c *Client) prepareProlong(request ProlongRequest) ProlongRequest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if request.PaymentID == "" && request.PaymentCode == "" {
		request.PaymentID, request.PaymentCode = c.paymentID, c.paymentCode
	}
	if request.PeriodCode != "" {
		request.PeriodID = ""
	}
	if request.PaymentCode != "" {
		request.PaymentID = ""
	}
	return request
}

func (c *Client) CalculateProlong(proxyType string, request ProlongRequest) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "prolong/calc/"+url.PathEscape(proxyType), c.prepareProlong(request))
	return result.Map, err
}

func (c *Client) MakeProlong(proxyType string, request ProlongRequest) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "prolong/make/"+url.PathEscape(proxyType), c.prepareProlong(request))
	if err != nil {
		return result.Map, err
	}
	return assertProlongMade(result.Map)
}

type GeoFilter struct {
	Country string `json:"country,omitempty"`
	Region  string `json:"region,omitempty"`
	City    string `json:"city,omitempty"`
	ISP     string `json:"isp,omitempty"`
}

type ResidentExport struct {
	Ports *int   `json:"ports,omitempty"`
	Ext   string `json:"ext,omitempty"`
}

type ResidentListRequest struct {
	Title     string          `json:"title,omitempty"`
	Whitelist string          `json:"whitelist,omitempty"`
	Geo       GeoFilter       `json:"geo"`
	Export    *ResidentExport `json:"export,omitempty"`
	Rotation  *int            `json:"rotation,omitempty"`
}

func (c *Client) CreateResidentList(request ResidentListRequest) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "resident/list/add", request)
	return result.Map, err
}

// ResidentLists — листы резидентского пакета. data — голый массив, без обёртки items
// (как в client-api v1). Идентификатор листа числовой: ListItemResponseDto.id — Long.
func (c *Client) ResidentLists() ([]interface{}, error) {
	result, err := c.Request(http.MethodGet, "resident/lists", nil)
	if err != nil {
		return nil, err
	}
	// data — голый массив листов, без обёртки items (как в client-api v1).
	return result.Slice, nil
}

type ResidentSubuserCreateRequest struct {
	Rotation     *int   `json:"rotation,omitempty"`
	TrafficLimit string `json:"traffic_limit"`
	ExpiredAt    string `json:"expired_at,omitempty"`
	IsLinkDate   *bool  `json:"is_link_date,omitempty"`
}

type ResidentSubuserUpdateRequest struct {
	PackageKey   string `json:"package_key"`
	Rotation     *int   `json:"rotation,omitempty"`
	TrafficLimit string `json:"traffic_limit,omitempty"`
	ExpiredAt    string `json:"expired_at,omitempty"`
	Active       *bool  `json:"is_active,omitempty"`
	IsLinkDate   *bool  `json:"is_link_date,omitempty"`
}

func (c *Client) CreateResidentSubuser(request ResidentSubuserCreateRequest) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "residentsubuser/create", request)
	return result.Map, err
}

func (c *Client) UpdateResidentSubuser(request ResidentSubuserUpdateRequest) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "residentsubuser/update", request)
	return result.Map, err
}

func (c *Client) DeleteResidentSubuser(packageKey string) (ResultData, error) {
	return c.Request(http.MethodDelete, "residentsubuser/delete", map[string]string{"package_key": packageKey})
}

func (c *Client) ResidentSubuserLists(packageKey string) ([]interface{}, error) {
	query := url.Values{"package_key": {packageKey}}
	result, err := c.Request(http.MethodGet, "residentsubuser/lists?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return result.Slice, nil
}

type ResidentSubuserListRequest struct {
	PackageKey string          `json:"package_key"`
	Title      string          `json:"title,omitempty"`
	Whitelist  string          `json:"whitelist,omitempty"`
	Geo        GeoFilter       `json:"geo"`
	Export     *ResidentExport `json:"export,omitempty"`
	Rotation   *int            `json:"rotation,omitempty"`
}

func (c *Client) CreateResidentSubuserList(request ResidentSubuserListRequest) (map[string]interface{}, error) {
	result, err := c.Request(http.MethodPost, "residentsubuser/list/add", request)
	return result.Map, err
}
