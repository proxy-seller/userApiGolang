package userApiGolang

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient поднимает локальный стенд и отдаёт клиент, созданный через NewClient —
// именно тот путь, на котором раньше падала половина SDK ("client api key is required").
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient("test-key", WithBaseURL(server.URL+"/personal/api/v2/"))
	// Темп запросов остаётся включённым, но идёт по фейковым часам (ratelimit_test.go): эти тесты
	// проверяют форму запросов, а серия write/money-вызовов на одном клиенте иначе ждала бы
	// реальные секунды.
	useFakeClock(client, newFakeClock())
	return client, server
}

func envelopeHandler(t *testing.T, body string, captured *string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("не удалось прочитать тело запроса: %v", err)
			}
			*captured = string(raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}

// Проверка локального гейта цели заказа.
//
// Сервер распознаёт mix не только по mixId/mixCode, но и через countryId: строкой
// "packageId:quantity" либо countryId=packageId вместе с quantity. Если mix распознан,
// customTargetName не нужен. Тест фиксирует именно это, потому что раньше гейт проверял только
// mixId/mixCode и блокировал legacy-путь OrderCalcMix/OrderMakeMix (пакет тогда уезжал в countryId).
func TestRequireCustomTargetNameMixViaCountryId(t *testing.T) {
	cases := []struct {
		name    string
		data    map[string]interface{}
		wantErr bool
	}{
		{
			name:    "mix через countryId+quantity — цель не нужна",
			data:    map[string]interface{}{"sectionCode": "mix", "countryId": "1370", "quantity": 100},
			wantErr: false,
		},
		{
			name:    "mix строкой packageId:quantity — цель не нужна",
			data:    map[string]interface{}{"sectionCode": "mix", "countryId": "1370:100"},
			wantErr: false,
		},
		{
			name:    "mix_isp через countryId+quantity — цель не нужна",
			data:    map[string]interface{}{"sectionCode": "mix_isp", "countryId": "1371", "quantity": 50},
			wantErr: false,
		},
		{
			name:    "mix по mixId — цель не нужна",
			data:    map[string]interface{}{"sectionCode": "mix", "mixId": "abc"},
			wantErr: false,
		},
		{
			name:    "quantity как float64 (из JSON) тоже освобождает mix",
			data:    map[string]interface{}{"sectionCode": "mix", "countryId": "1370", "quantity": float64(100)},
			wantErr: false,
		},
		{
			name:    "mix без пакета и без цели — сервер свалится в ipv4, цель обязательна",
			data:    map[string]interface{}{"sectionCode": "mix"},
			wantErr: true,
		},
		{
			name:    "ipv4 без цели — обязательна",
			data:    map[string]interface{}{"sectionCode": "ipv4", "countryId": "1", "quantity": 1},
			wantErr: true,
		},
		{
			name:    "ipv4 с целью — ок",
			data:    map[string]interface{}{"sectionCode": "ipv4", "countryId": "1", "quantity": 1, "customTargetName": "seo"},
			wantErr: false,
		},
		{
			name:    "mobile — цель не требуется вообще",
			data:    map[string]interface{}{"sectionCode": "mobile"},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireCustomTargetName(tc.data)
			if tc.wantErr && err == nil {
				t.Fatalf("ожидалась ошибка, получен nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ожидался nil, получена ошибка: %v", err)
			}
		})
	}
}

// Тот же инвариант на типизированном пути (OrderRequest).
func TestRequireOrderTargetMixViaCountryId(t *testing.T) {
	if err := requireOrderTarget(OrderRequest{SectionCode: "mix", CountryID: "1370", Quantity: 100}); err != nil {
		t.Fatalf("mix через countryId+quantity не должен требовать цель, получено: %v", err)
	}
	if err := requireOrderTarget(OrderRequest{SectionCode: "mix", CountryID: "1370:100"}); err != nil {
		t.Fatalf("mix строкой packageId:quantity не должен требовать цель, получено: %v", err)
	}
	if err := requireOrderTarget(OrderRequest{SectionCode: "mix_isp", CountryID: "1371", Quantity: 50}); err != nil {
		t.Fatalf("mix_isp через countryId+quantity не должен требовать цель, получено: %v", err)
	}
	if err := requireOrderTarget(OrderRequest{SectionCode: "ipv4", CountryID: "1", Quantity: 1}); err == nil {
		t.Fatal("ipv4 без цели обязан отбиваться локально")
	}
}

// data delete-эндпоинтов приходит СТРОКОЙ, а не объектом. Раньше обёртки возвращали
// result.Map (nil в этом случае), и неудавшееся удаление было неотличимо от успешного.
func TestDeleteResultMap(t *testing.T) {
	// resident/list/delete → "delete"
	got := deleteResultMap(ResultData{Value: "delete"})
	if got == nil || got["status"] != "delete" {
		t.Fatalf(`ожидалось {status:"delete"}, получено %#v`, got)
	}

	// residentsubuser/list/delete → JSON внутри строки, худший случай: not-found при status=success
	got = deleteResultMap(ResultData{Value: `{"status":"not-found"}`})
	if got == nil || got["status"] != "not-found" {
		t.Fatalf(`ожидалось {status:"not-found"}, получено %#v`, got)
	}

	// уже объект — отдаём как есть
	got = deleteResultMap(ResultData{Map: map[string]interface{}{"status": "delete"}})
	if got["status"] != "delete" {
		t.Fatalf("объект должен проходить без изменений, получено %#v", got)
	}

	// пусто — nil, а не паника
	if got = deleteResultMap(ResultData{}); got != nil {
		t.Fatalf("ожидался nil, получено %#v", got)
	}
}

// errors[].code на проводе — целое JSON-число. Пока поле лежало в interface{}, encoding/json
// давал float64 и любое ветвление по коду было тихо ложным. Тест фиксирует типизацию.
func TestAPIErrorCodeIsTyped(t *testing.T) {
	body := `{"status":"error","data":null,"errors":[{"message":"Set comment","code":503}]}`
	_, err := parseEnvelope([]byte(body), http.StatusOK)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("ожидался *APIError, получено %T (%v)", err, err)
	}
	if apiErr.Errors[0].Code != "503" {
		t.Fatalf(`ожидался код "503", получено %q`, apiErr.Errors[0].Code)
	}
	if apiErr.Errors[0].CodeInt() != 503 || apiErr.CodeInt() != 503 {
		t.Fatalf("ожидался числовой код 503, получено item=%d error=%d", apiErr.Errors[0].CodeInt(), apiErr.CodeInt())
	}
	if !apiErr.HasCode(503) {
		t.Fatal("HasCode(503) должен находить код")
	}
	if got := apiErr.Error(); got != "client api error 503: Set comment" {
		t.Fatalf("текст ошибки изменился: %q", got)
	}
	// код строкой тоже не должен ронять разбор
	_, err = parseEnvelope([]byte(`{"status":"error","errors":[{"message":"x","code":"22003"}]}`), http.StatusOK)
	apiErr, ok = err.(*APIError)
	if !ok {
		t.Fatalf("ожидался *APIError, получено %T", err)
	}
	if apiErr.CodeInt() != 22003 {
		t.Fatalf("строковый код должен разбираться в 22003, получено %d", apiErr.CodeInt())
	}
	// marshal обратно числом, как у сервера
	encoded, marshalErr := json.Marshal(APIErrorItem{Message: "x", Code: "503"})
	if marshalErr != nil {
		t.Fatalf("marshal: %v", marshalErr)
	}
	if !strings.Contains(string(encoded), `"code":503`) {
		t.Fatalf("код должен сериализоваться числом, получено %s", encoded)
	}
}

// Ошибки доступа (битый ключ / IP не разрешён / rate limit) приходят HTTP 200 одной и той же
// тройкой ошибок с code 503. Клиент обязан видеть весь массив.
func TestAccessErrorTripleIsFullyVisible(t *testing.T) {
	body := `{"status":"error","data":null,"errors":[` +
		`{"message":"Error api key","code":503},` +
		`{"message":"IP not allowed 10.0.0.1","code":503},` +
		`{"message":"Request limit reached","code":503}]}`
	_, err := parseEnvelope([]byte(body), http.StatusOK)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("ожидался *APIError, получено %T", err)
	}
	if len(apiErr.Errors) != 3 {
		t.Fatalf("ожидались все 3 ошибки, получено %d", len(apiErr.Errors))
	}
	if !apiErr.IsAccessError() {
		t.Fatal("тройка должна распознаваться как ошибка доступа")
	}
	if messages := apiErr.Messages(); len(messages) != 3 || messages[2] != "Request limit reached" {
		t.Fatalf("ожидались все сообщения тройки, получено %#v", messages)
	}
	// обычная ошибка доступом не является
	_, err = parseEnvelope([]byte(`{"status":"error","errors":[{"message":"Incorrect goal","code":14}]}`), http.StatusOK)
	if err.(*APIError).IsAccessError() {
		t.Fatal("Incorrect goal не ошибка доступа")
	}
}

// ext: длина <= 250, запрещены CR, LF, '/' и '\' — иначе сервер отвечает голым HTTP 400
// плайн-текстом мимо конверта.
func TestAssertExt(t *testing.T) {
	if err := assertExt(""); err != nil {
		t.Fatalf("пустой ext допустим, получено: %v", err)
	}
	if err := assertExt("csv"); err != nil {
		t.Fatalf("csv допустим, получено: %v", err)
	}
	if err := assertExt("%login%:%password%@%ip%:%port%"); err != nil {
		t.Fatalf("шаблон допустим, получено: %v", err)
	}
	if err := assertExt(strings.Repeat("a", MaxProxyDownloadExtLength)); err != nil {
		t.Fatalf("ровно 250 символов допустимы, получено: %v", err)
	}
	if err := assertExt(strings.Repeat("a", MaxProxyDownloadExtLength+1)); err == nil {
		t.Fatal("251 символ обязан отбиваться локально")
	}
	for _, bad := range []string{"a\rb", "a\nb", "a/b", `a\b`} {
		if err := assertExt(bad); err == nil {
			t.Fatalf("ext %q обязан отбиваться локально", bad)
		}
	}
}

// Бинарные выгрузки: старые подписи глотают ошибку и отдают пустую строку, *E-варианты
// обязаны её возвращать.
func TestBinaryDownloadsHaveErrorVariants(t *testing.T) {
	errorBody := `{"status":"error","data":null,"errors":[{"message":"Error api key","code":503}]}`
	client, server := newTestClient(t, envelopeHandler(t, errorBody, nil))

	if _, err := client.ProxyDownload("ipv4", "txt", "", ""); err == nil {
		t.Fatal("ProxyDownload обязан возвращать ошибку конверта")
	}
	if _, err := client.ProxyDownloadResident("1", "txt", ""); err == nil {
		t.Fatal("ProxyDownloadResident обязан возвращать ошибку конверта")
	}
	if _, err := client.ResidentGeo(); err == nil {
		t.Fatal("ResidentGeo обязан возвращать ошибку конверта")
	}
	if _, err := client.ResidentGeoIsp(); err == nil {
		t.Fatal("ResidentGeoIsp обязан возвращать ошибку конверта")
	}

	// локальная проверка ext срабатывает до сети
	if _, err := client.ProxyDownload("ipv4", "a/b", "", ""); err == nil {
		t.Fatal("ProxyDownload обязан отбивать запрещённый ext")
	}
	if _, err := client.DownloadProxies("ipv4", ProxyDownloadOptions{Ext: strings.Repeat("a", 251)}); err == nil {
		t.Fatal("DownloadProxies обязан отбивать слишком длинный ext")
	}
	// package_key игнорируется всеми маршрутами, кроме subresident
	if _, err := client.DownloadProxies("resident", ProxyDownloadOptions{PackageKey: "abc"}); err == nil {
		t.Fatal("package_key вне subresident обязан отбиваться локально")
	}

	// пакетные обёртки на DefaultClient: старая подпись — пустая строка, *E — ошибка
	oldURL := URL
	URL = server.URL + "/personal/api/v2/"
	SetApiKey("test-key")
	t.Cleanup(func() { URL = oldURL; SetApiKey("") })

	if value := ProxyDownload("ipv4", "txt", "", ""); value != "" {
		t.Fatalf("старая подпись обязана вернуть пустую строку, получено %q", value)
	}
	if _, err := ProxyDownloadE("ipv4", "txt", "", ""); err == nil {
		t.Fatal("ProxyDownloadE обязан вернуть ошибку")
	}
	if _, err := ResidentGeoE(); err == nil {
		t.Fatal("ResidentGeoE обязан вернуть ошибку")
	}
	if _, err := ResidentGeoIspE(); err == nil {
		t.Fatal("ResidentGeoIspE обязан вернуть ошибку")
	}
	if _, err := ProxyDownloadResidentE("1", "txt", ""); err == nil {
		t.Fatal("ProxyDownloadResidentE обязан вернуть ошибку")
	}
}

// Функции, которые раньше жили только как package-level на DefaultClient, обязаны работать
// у клиента из NewClient — иначе пользователь получал "client api key is required".
func TestClientHasMethodsForLegacyPackageFunctions(t *testing.T) {
	client, _ := newTestClient(t, envelopeHandler(t, `{"status":"success","data":{"ok":true},"errors":[]}`, nil))

	if _, err := client.ReferenceList("ipv4"); err != nil {
		t.Fatalf("ReferenceList: %v", err)
	}
	if _, err := client.ResidentPackage(); err != nil {
		t.Fatalf("ResidentPackage: %v", err)
	}
	if _, err := client.ProxyReplace([]string{"68b1f0c4e13a4c0f1a2b3c4d"}, ProxyReplaceReasonNotWork, ""); err != nil {
		t.Fatalf("ProxyReplace: %v", err)
	}
	if _, err := client.ResidentTrafficDetails(map[string]interface{}{"packageKey": "abc"}); err != nil {
		t.Fatalf("ResidentTrafficDetails: %v", err)
	}
	if _, err := client.ResidentListRotation(561, 60); err != nil {
		t.Fatalf("ResidentListRotation: %v", err)
	}
	if _, err := client.ResidentsubuserListTools("abc"); err != nil {
		t.Fatalf("ResidentsubuserListTools: %v", err)
	}
	if _, err := client.OrderCalcIpv4("1", "1", 1, "login", "", "seo"); err != nil {
		t.Fatalf("OrderCalcIpv4: %v", err)
	}
	if _, err := client.ProlongCalc("ipv4", "68b1f0c4e13a4c0f1a2b3c4d", "1", ""); err != nil {
		t.Fatalf("ProlongCalc: %v", err)
	}
	if _, err := client.BalancePaymentsList(); err == nil {
		t.Fatal("BalancePaymentsList обязан жаловаться на отсутствие data.items")
	}
}

// proxy/replace: type — ПРИЧИНА замены (одна из ProxyReplaceReasons), а не тип прокси;
// при CUSTOM обязателен непустой comment (иначе сервер отвечает "Set comment", code 503).
func TestProxyReplaceReasonValidation(t *testing.T) {
	if err := assertProxyReplaceReason("ipv4", ""); err == nil {
		t.Fatal("тип прокси не является причиной замены и должен отбиваться")
	}
	for _, reason := range ProxyReplaceReasons {
		comment := ""
		if reason == ProxyReplaceReasonCustom {
			comment = "своя причина"
		}
		if err := assertProxyReplaceReason(reason, comment); err != nil {
			t.Fatalf("причина %s должна проходить: %v", reason, err)
		}
	}
	// сервер принимает причину в любом регистре
	if err := assertProxyReplaceReason("not_work", ""); err != nil {
		t.Fatalf("нижний регистр допустим: %v", err)
	}
	if err := assertProxyReplaceReason(ProxyReplaceReasonCustom, "   "); err == nil {
		t.Fatal("CUSTOM без непустого comment обязан отбиваться")
	}
	client, _ := newTestClient(t, envelopeHandler(t, `{"status":"success","data":{"ok":{"ips":[]}},"errors":[]}`, nil))
	if _, err := client.ProxyReplace("id1", ProxyReplaceReasonCustom, ""); err == nil {
		t.Fatal("ProxyReplace обязан отбивать CUSTOM без comment до отправки")
	}
}

// balance/add — единственная точка, где paymentCode НЕ резолвится: сервер принимает там только
// paymentId. Раньше SDK молча уезжал с paymentId="".
func TestAddBalanceRejectsPaymentCodeOnly(t *testing.T) {
	client, _ := newTestClient(t, envelopeHandler(t, `{"status":"success","data":{"url":"https://pay"},"errors":[]}`, nil))
	client.SetPaymentCode("balance")

	_, err := client.AddBalance(10, "")
	if err == nil {
		t.Fatal("ожидалась локальная ошибка про нерезолвящийся paymentCode")
	}
	if !strings.Contains(err.Error(), "paymentCode") {
		t.Fatalf("ошибка должна объяснять проблему с paymentCode, получено: %v", err)
	}

	// с paymentId запрос уходит как раньше
	client.SetPaymentID("68b1f0c4e13a4c0f1a2b3c4d")
	if url, addErr := client.AddBalance(10, ""); addErr != nil || url != "https://pay" {
		t.Fatalf("ожидался url платежа, получено %q / %v", url, addErr)
	}
}

// balance/autotopup/set — PARTIAL UPDATE: опущенное поле не должно уезжать как null.
// Полей dailyCountCap и monthlyAmountCap в контракте больше нет (убраны 18.08.2026), их нет
// ни в запросе, ни в ответе — присланные сервер игнорирует.
func TestAutoTopupSetSendsOnlyProvidedFields(t *testing.T) {
	var body string
	state := `{"status":"success","data":{"configured":true,"enabled":true,"state":"ACTIVE",` +
		`"threshold":5,"amount":10,"subscriptionId":"sub_1","failCount":0,` +
		`"paymentMethod":{"id":"sub_1","status":"active","paymentMethod":"card","brand":"visa","last4":"4242","exp":"12/2030"},` +
		`"lastEvent":{"status":"SUCCEEDED","amount":10,"at":"2026-08-17T10:00:00.000+00:00"}},"errors":[]}`
	client, _ := newTestClient(t, envelopeHandler(t, state, &body))

	got, err := client.SetAutoTopup(AutoTopupSetRequest{Threshold: Float64(5)})
	if err != nil {
		t.Fatalf("SetAutoTopup: %v", err)
	}
	if body != `{"threshold":5}` {
		t.Fatalf(`ожидалось тело {"threshold":5} без остальных полей, получено %s`, body)
	}
	if got == nil || !got.Enabled || got.State != "ACTIVE" || got.SubscriptionID != "sub_1" {
		t.Fatalf("состояние после сохранения разобрано неверно: %#v", got)
	}
	if got.PaymentMethod == nil || got.PaymentMethod.Last4 != "4242" {
		t.Fatalf("paymentMethod разобран неверно: %#v", got.PaymentMethod)
	}
	if got.LastEvent == nil || got.LastEvent.Status != "SUCCEEDED" {
		t.Fatalf("lastEvent разобран неверно: %#v", got.LastEvent)
	}

	// выключение — тоже одно поле, false обязан доехать (не отброситься omitempty)
	if _, err = client.SetAutoTopup(AutoTopupSetRequest{Enabled: Bool(false)}); err != nil {
		t.Fatalf("SetAutoTopup(enabled=false): %v", err)
	}
	if body != `{"enabled":false}` {
		t.Fatalf(`ожидалось тело {"enabled":false}, получено %s`, body)
	}

	// пустой запрос не должен содержать ни одного null
	if _, err = client.SetAutoTopup(AutoTopupSetRequest{}); err != nil {
		t.Fatalf("SetAutoTopup({}): %v", err)
	}
	if body != `{}` {
		t.Fatalf("пустой partial-update обязан быть {}, получено %s", body)
	}
}

func TestAutoTopupGetParsesState(t *testing.T) {
	client, _ := newTestClient(t, envelopeHandler(t,
		`{"status":"success","data":{"configured":false,"enabled":false,"state":"NO_PAYMENT_METHOD","failCount":0},"errors":[]}`, nil))
	state, err := client.GetAutoTopup()
	if err != nil {
		t.Fatalf("GetAutoTopup: %v", err)
	}
	if state.Configured || state.Enabled || state.State != "NO_PAYMENT_METHOD" {
		t.Fatalf("состояние разобрано неверно: %#v", state)
	}
	if state.PaymentMethod != nil || state.Threshold != nil {
		t.Fatal("отсутствующие поля должны остаться nil, а не нулями")
	}
}

// Границы значений приходят в errors[0].customData, коды 49-53 и 56 (54 и 55 удалены вместе с дневным и месячным лимитами). Клиент обязан
// иметь к ним доступ.
func TestAutoTopupLimitsFromError(t *testing.T) {
	body := `{"status":"error","data":null,"errors":[{"message":"Top-up amount must be 5 or more","code":51,` +
		`"customData":{"minAmount":5,"minThreshold":1}}]}`
	_, err := parseEnvelope([]byte(body), http.StatusOK)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("ожидался *APIError, получено %T", err)
	}
	if !apiErr.HasCode(51) {
		t.Fatal("код 51 (сумма ниже минимума) должен быть доступен")
	}
	limits, found := AutoTopupLimitsFromError(err)
	if !found {
		t.Fatal("границы из customData не найдены")
	}
	if limits.MinAmount == nil || *limits.MinAmount != 5 {
		t.Fatalf("minAmount разобран неверно: %#v", limits.MinAmount)
	}
	if limits.MinThreshold == nil || *limits.MinThreshold != 1 {
		t.Fatalf("minThreshold разобран неверно: %#v", limits.MinThreshold)
	}
	// minDailyCountCap здесь больше не проверяется: ключ убран из контракта 18.08.2026
	// вместе с самим лимитом, и в customData сервер отдаёт только minAmount и minThreshold.
	if apiErr.FirstCustomData() == nil {
		t.Fatal("FirstCustomData обязан отдавать customData")
	}
	if _, found = AutoTopupLimitsFromError(nil); found {
		t.Fatal("nil-ошибка не содержит границ")
	}
}

// Типы listId — как на проводе: resident/list/* принимает JSON-число, а
// residentsubuser/list/delete — непустую строку.
func TestListIDTypesMatchServer(t *testing.T) {
	var body string
	client, _ := newTestClient(t, envelopeHandler(t, `{"status":"success","data":"delete","errors":[]}`, &body))

	got, err := client.ResidentListDelete(561)
	if err != nil {
		t.Fatalf("ResidentListDelete: %v", err)
	}
	if body != `{"id":561}` {
		t.Fatalf(`resident/list/delete обязан отправлять число, получено %s`, body)
	}
	if got["status"] != "delete" {
		t.Fatalf("data-строка delete-эндпоинта не разобрана: %#v", got)
	}

	if _, err = client.ResidentsubuserListDelete("f9ea7063d7a699b12b3e", "123"); err != nil {
		t.Fatalf("ResidentsubuserListDelete: %v", err)
	}
	if !strings.Contains(body, `"id":"123"`) {
		t.Fatalf(`residentsubuser/list/delete обязан отправлять строку, получено %s`, body)
	}

	if _, err = client.ResidentListRename(561, "new"); err != nil {
		t.Fatalf("ResidentListRename: %v", err)
	}
	if !strings.Contains(body, `"id":561`) {
		t.Fatalf("resident/list/rename обязан отправлять число, получено %s", body)
	}
}

// Списки id в запросах — строки ObjectId; ветки []int больше нет, значения не-string
// уходят как есть, а не превращаются в бессмысленные строковые id.
func TestNormalizeIDs(t *testing.T) {
	got, ok := normalizeIDs("a, b ,,c").([]string)
	if !ok || len(got) != 3 || got[2] != "c" {
		t.Fatalf("строка с запятыми должна разбиваться, получено %#v", got)
	}
	ids := []string{"a", "b"}
	if same, isSlice := normalizeIDs(ids).([]string); !isSlice || len(same) != 2 {
		t.Fatalf("[]string должен проходить как есть, получено %#v", same)
	}
	if _, isStrings := normalizeIDs([]int{1, 2}).([]string); isStrings {
		t.Fatal("[]int больше не должен молча превращаться в строковые id")
	}
}

// prolong/make при нехватке средств кладёт причину в errors[{code:16}], поэтому её разбирает
// общий parseEnvelope, и отдельной обёртки в SDK больше нет.
//
// Прежняя форма — status="error" с ПУСТЫМ errors[] — под которую эта обёртка писалась,
// сервером не отдаётся. После смены формы единственным её эффектом осталось превращать
// ЛЕГИТИМНЫЙ success с пустым orderId в фальшивую ошибку уже ПОСЛЕ списания денег.
func TestProlongMakeInsufficientFundsIsAnError(t *testing.T) {
	insufficient := `{"status":"error","data":null,"errors":[{"code":16,"message":"Insufficient funds on balance"}]}`

	client, _ := newTestClient(t, envelopeHandler(t, insufficient, nil))
	_, err := client.ProlongMake("ipv4", []string{"68b1f0c4e13a4c0f1a2b3c4d"}, "1m", "")
	if err == nil {
		t.Fatal("нехватка средств обязана возвращать ошибку, а не выглядеть успехом")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("ожидался *APIError, получено %T", err)
	}
	if !apiErr.HasCode(16) {
		t.Fatalf("код 16 (нехватка средств) должен быть доступен, получено: %v", err)
	}

	// типизированный путь ведёт себя так же
	client2, _ := newTestClient(t, envelopeHandler(t, insufficient, nil))
	if _, err = client2.MakeProlong("ipv4", ProlongRequest{IDs: []string{"x"}, PeriodID: "1m"}); err == nil {
		t.Fatal("MakeProlong тоже обязан вернуть ошибку при нехватке средств")
	}

	// Успешное продление по-прежнему проходит. orderIds — все продлённые заказы (одним запросом
	// их может быть несколько), orderId — первый из них.
	okBody := `{"status":"success","data":{"orderId":"68b1f0c4e13a4c0f1a2b3c4d",` +
		`"orderIds":["68b1f0c4e13a4c0f1a2b3c4d","68b1f0c4e13a4c0f1a2b3c4e"],"total":10,"balance":90,` +
		`"listBaseOrderNumbers":["LH-1","LH-2"]},"errors":[]}`
	client3, _ := newTestClient(t, envelopeHandler(t, okBody, nil))
	data, err := client3.ProlongMake("ipv6", []string{"68b1f0c4e13a4c0f1a2b3c4d", "68b1f0c4e13a4c0f1a2b3c4e"}, "1m", "")
	if err != nil {
		t.Fatalf("успешное продление не должно давать ошибку: %v", err)
	}
	if data["orderId"] != "68b1f0c4e13a4c0f1a2b3c4d" {
		t.Fatalf("orderId потерян: %#v", data)
	}
	if orderIDs, _ := data["orderIds"].([]interface{}); len(orderIDs) != 2 || orderIDs[1] != "68b1f0c4e13a4c0f1a2b3c4e" {
		t.Fatalf("orderIds потерян: %#v", data["orderIds"])
	}

	// Деньги уже списаны: пустой orderId в успешном конверте терять total/balance нельзя.
	emptyID := `{"status":"success","data":{"orderId":"","orderIds":[],"total":10,"balance":90,"listBaseOrderNumbers":["LH-1"]},"errors":[]}`
	client4, _ := newTestClient(t, envelopeHandler(t, emptyID, nil))
	data, err = client4.ProlongMake("ipv4", []string{"x"}, "1m", "")
	if err != nil {
		t.Fatalf("успех с пустым orderId не должен становиться ошибкой: %v", err)
	}
	if data["total"] == nil || data["listBaseOrderNumbers"] == nil {
		t.Fatalf("данные списания потеряны: %#v", data)
	}
}

// TestSplitProlongTargets — значения разводятся по форме: строка с "." или ":" — адрес, всё
// остальное — id; смешанный список разводится на обе части. В какое поле уйдут id (ids или
// orderIds) и допустим ли смешанный список, решает тип — см. TestPrepareLegacyProlongRoutesByType.
func TestSplitProlongTargets(t *testing.T) {
	cases := []struct {
		name    string
		input   interface{}
		wantIPs []string
		wantIDs []string
	}{
		{"ipv4", []string{"1.2.3.4", "5.6.7.8"}, []string{"1.2.3.4", "5.6.7.8"}, nil},
		// Любая строка с двоеточием — адрес: и mobile-тройка ip:port_http:port_socks, и литерал ipv6.
		{"colon address", []string{"2001:db8::1:8080"}, []string{"2001:db8::1:8080"}, nil},
		{"mobile triple", []string{"10.0.0.1:8000:9000"}, []string{"10.0.0.1:8000:9000"}, nil},
		{"objectids", []string{"68b1f0c4e13a4c0f1a2b3c4d"}, nil, []string{"68b1f0c4e13a4c0f1a2b3c4d"}},
		{"mixed", []string{"1.2.3.4", "68b1f0c4e13a4c0f1a2b3c4d"}, []string{"1.2.3.4"}, []string{"68b1f0c4e13a4c0f1a2b3c4d"}},
		{"comma string", "1.2.3.4, 5.6.7.8", []string{"1.2.3.4", "5.6.7.8"}, nil},
		{"blanks dropped", []string{"1.2.3.4", "   ", ""}, []string{"1.2.3.4"}, nil},
		{"interface slice, non-strings skipped", []interface{}{"1.2.3.4", "68b1f0c4e13a4c0f1a2b3c4d", 5},
			[]string{"1.2.3.4"}, []string{"68b1f0c4e13a4c0f1a2b3c4d"}},
		{"nil", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ips, ids := splitProlongTargets(tc.input)
			if !reflect.DeepEqual(ips, tc.wantIPs) {
				t.Fatalf("ips = %#v, want %#v", ips, tc.wantIPs)
			}
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("ids = %#v, want %#v", ids, tc.wantIDs)
			}
		})
	}
}

// TestPrepareLegacyProlongRoutesByType — позиционные ProlongCalc/ProlongMake раскладывают выбор
// по типу. ipv4 / isp / mobile продлеваются по отдельным прокси: id уходит в ids, адрес — в ips,
// а список, где смешаны id и адреса, отбивается ошибкой (см. TestProlongRefusesMixedIDsAndAddresses).
// ipv6 / mix / mix_isp продлеваются только целым заказом: id уходит в orderIds, адрес — в ips, и
// смешанный список уходит как есть (часть ips сервер отбивает сам). resident выбора не шлёт вовсе.
// Поля ids у ipv6 / mix / mix_isp нет ни в одной ветке, включая сырой фолбэк для значения, которое
// не разобралось. Сверяется всё тело, кроме periodId и coupon, поэтому лишнее поле выбора тоже
// провалит тест.
func TestPrepareLegacyProlongRoutesByType(t *testing.T) {
	const (
		proxyID  = "68b1f0c4e13a4c0f1a2b3c4d"
		orderID  = "6a248de4717805635cf6057d"
		orderID2 = "6a248de4717805635cf6058a"
	)
	cases := []struct {
		name      string
		proxyType string
		input     interface{}
		want      map[string]interface{} // всё тело, кроме periodId и coupon, — то есть только выбор
	}{
		{"ipv4 address", "ipv4", []string{"1.2.3.4"}, map[string]interface{}{"ips": []string{"1.2.3.4"}}},
		{"ipv4 id", "ipv4", []string{proxyID}, map[string]interface{}{"ids": []string{proxyID}}},
		{"isp id from a string", "isp", proxyID, map[string]interface{}{"ids": []string{proxyID}}},
		{"mobile id", "mobile", []string{proxyID}, map[string]interface{}{"ids": []string{proxyID}}},
		{"mobile address", "mobile", []string{"10.0.0.1:50100:50101"}, map[string]interface{}{"ips": []string{"10.0.0.1:50100:50101"}}},
		{"ipv6 order id", "ipv6", []string{orderID}, map[string]interface{}{"orderIds": []string{orderID}}},
		{"mix order ids from a comma string", "mix", orderID + ", " + orderID2,
			map[string]interface{}{"orderIds": []string{orderID, orderID2}}},
		{"mix_isp order id", "mix_isp", []string{orderID}, map[string]interface{}{"orderIds": []string{orderID}}},
		{"type is normalized", " Mix-ISP ", []string{orderID}, map[string]interface{}{"orderIds": []string{orderID}}},
		// Адрес для заказного типа не превращается в заказ: он уходит в ips, и сервер называет
		// причину отказа ("[ips] is not applicable for ipv6: prolong by [orderIds]").
		{"ipv6 address stays in ips", "ipv6", []string{"1.2.3.4:26000"}, map[string]interface{}{"ips": []string{"1.2.3.4:26000"}}},
		{"ipv6 mixed list keeps both parts", "ipv6", []string{orderID, "1.2.3.4:26000"},
			map[string]interface{}{"orderIds": []string{orderID}, "ips": []string{"1.2.3.4:26000"}}},
		{"mix mixed list keeps both parts", "mix", []string{"1.2.3.4", orderID},
			map[string]interface{}{"orderIds": []string{orderID}, "ips": []string{"1.2.3.4"}}},
		{"resident sends no selection", "resident", []string{proxyID}, map[string]interface{}{}},
		{"raw fallback goes to ids", "ipv4", []int{1, 2}, map[string]interface{}{"ids": []int{1, 2}}},
		{"raw fallback goes to orderIds", "mix", []int{1}, map[string]interface{}{"orderIds": []int{1}}},
		{"nil sends no selection", "ipv4", nil, map[string]interface{}{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := NewClient("test-key").prepareLegacyProlong(tc.proxyType, tc.input, "1m", "")
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			got := map[string]interface{}{}
			for field, value := range data {
				if field != "periodId" && field != "coupon" {
					got[field] = value
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("selection = %#v, want %#v (whole body: %#v)", got, tc.want, data)
			}
			if data["periodId"] != "1m" {
				t.Fatalf("periodId потерян: %#v", data)
			}
		})
	}
}

// ipv4 / isp / mobile: список, где смешаны id прокси и адреса, отбивается до запроса — получив оба
// поля, сервер продлевает по ids и молча игнорирует ips, и адреса выпали бы из оплаченного
// продления. Ни calc, ни make с таким списком на сервер не уходят.
func TestProlongRefusesMixedIDsAndAddresses(t *testing.T) {
	var requests int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"total":1},"errors":[]}`))
	})
	mixed := map[string][]string{
		"ipv4":   {"1.2.3.4", "68b1f0c4e13a4c0f1a2b3c4d"},
		"isp":    {"68b1f0c4e13a4c0f1a2b3c4d", "5.6.7.8"},
		"mobile": {"10.0.0.1:50100:50101", "68b1f0c4e13a4c0f1a2b3c4d"},
	}
	for proxyType, list := range mixed {
		if _, err := client.ProlongCalc(proxyType, list, "1m", ""); err == nil ||
			!strings.Contains(err.Error(), "mixing proxy ids and addresses") ||
			!strings.Contains(err.Error(), "the server renews by ids and silently ignores ips") {
			t.Fatalf("ProlongCalc(%s, смешанный список): ожидалась локальная ошибка, получено %v", proxyType, err)
		}
		if _, err := client.ProlongMake(proxyType, strings.Join(list, ","), "1m", ""); err == nil ||
			!strings.Contains(err.Error(), "mixing proxy ids and addresses") {
			t.Fatalf("ProlongMake(%s, смешанная строка): ожидалась локальная ошибка, получено %v", proxyType, err)
		}
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("смешанный список не должен уходить на сервер, ушло запросов: %d", got)
	}

	// Однородные списки уходят как раньше.
	if _, err := client.ProlongCalc("ipv4", []string{"1.2.3.4", "5.6.7.8"}, "1m", ""); err != nil {
		t.Fatalf("ProlongCalc по адресам: %v", err)
	}
	if _, err := client.ProlongCalc("ipv4", []string{"68b1f0c4e13a4c0f1a2b3c4d"}, "1m", ""); err != nil {
		t.Fatalf("ProlongCalc по id: %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Fatalf("ожидалось 2 запроса с однородными списками, ушло %d", got)
	}
}

// Резидентский пакет продлевается целиком: любой непустой выбор (IDs, IPs, OrderIDs) при
// type = resident отбивается до запроса — для calc, enable и disable. Его нельзя ни отправить, ни
// молча выбросить: тогда disable, адресованный паре прокси, выключил бы автопродление всего пакета.
func TestAutoProlongResidentRefusesSelection(t *testing.T) {
	var requests int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"autoProlong":false,"quantity":1,"ids":[],"orderIds":[]},"errors":[]}`))
	})
	selections := map[string]ProlongRequest{
		"IDs":      {IDs: []string{"68b1f0c4e13a4c0f1a2b3c4d"}, PaymentID: "balance"},
		"IPs":      {IPs: []string{"1.2.3.4"}, PaymentID: "balance"},
		"OrderIDs": {OrderIDs: []string{"6a248de4717805635cf6057d"}, PaymentID: "balance"},
	}
	calls := map[string]func(string, AutoProlongRequest) (map[string]interface{}, error){
		"calc":    client.CalculateAutoProlong,
		"enable":  client.EnableAutoProlong,
		"disable": client.DisableAutoProlong,
	}
	for field, selection := range selections {
		for name, call := range calls {
			for _, proxyType := range []string{"resident", " Resident "} {
				_, err := call(proxyType, AutoProlongRequest{ProlongRequest: selection})
				if err == nil || !strings.Contains(err.Error(), "applies to the whole package") {
					t.Fatalf("%s(%q) с %s: ожидалась локальная ошибка, получено %v", name, proxyType, field, err)
				}
			}
		}
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("выбор для resident не должен уходить на сервер, ушло запросов: %d", got)
	}

	// Без выбора резидентские вызовы уходят как раньше, а у обычных типов выбор, конечно, законен.
	if _, err := client.EnableAutoProlong("resident", AutoProlongRequest{ProlongRequest: ProlongRequest{PaymentID: "balance"}}); err != nil {
		t.Fatalf("EnableAutoProlong(resident) без выбора: %v", err)
	}
	if _, err := client.DisableAutoProlong("resident", AutoProlongRequest{}); err != nil {
		t.Fatalf("DisableAutoProlong(resident) без выбора: %v", err)
	}
	if _, err := client.DisableAutoProlong("ipv4", AutoProlongRequest{ProlongRequest: ProlongRequest{IDs: []string{"68b1f0c4e13a4c0f1a2b3c4d"}}}); err != nil {
		t.Fatalf("DisableAutoProlong(ipv4) с IDs: %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Fatalf("ожидалось 3 запроса, ушло %d", got)
	}
}

// paddle_subscription без SubscriptionID уходит на сервер: с одной привязанной картой он берёт её
// сам, а сколько карт на аккаунте, знает только он. Заданный SubscriptionID уходит как есть, а без
// платёжки calc и enable по-прежнему отбиваются до запроса.
func TestAutoProlongPaddleSubscriptionNeedsNoSubscriptionID(t *testing.T) {
	var body string
	client, _ := newTestClient(t, envelopeHandler(t, `{"status":"success","data":{"autoProlong":true},"errors":[]}`, &body))

	request := AutoProlongRequest{ProlongRequest: ProlongRequest{IDs: []string{"68b1f0c4e13a4c0f1a2b3c4d"}, PeriodID: "1m", PaymentID: AutoProlongPaymentPaddleSubscription}}
	if _, err := client.EnableAutoProlong("ipv4", request); err != nil {
		t.Fatalf("EnableAutoProlong(paddle_subscription) без SubscriptionID: %v", err)
	}
	if want := `{"ids":["68b1f0c4e13a4c0f1a2b3c4d"],"periodId":"1m","paymentId":"paddle_subscription"}`; body != want {
		t.Fatalf("тело = %s, ожидалось %s", body, want)
	}

	client.SetPaymentCode(AutoProlongPaymentPaddleSubscription)
	if _, err := client.CalculateAutoProlong("resident", AutoProlongRequest{}); err != nil {
		t.Fatalf("CalculateAutoProlong(resident) с кодом paddle_subscription: %v", err)
	}
	if want := `{"paymentCode":"paddle_subscription"}`; body != want {
		t.Fatalf("тело = %s, ожидалось %s", body, want)
	}

	request.SubscriptionID = "sub_1"
	if _, err := client.EnableAutoProlong("ipv4", request); err != nil {
		t.Fatalf("EnableAutoProlong с SubscriptionID: %v", err)
	}
	if !strings.Contains(body, `"subscriptionId":"sub_1"`) {
		t.Fatalf("SubscriptionID потерялся: %s", body)
	}

	var requests int32
	unpaid, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{},"errors":[]}`))
	})
	_, err := unpaid.EnableAutoProlong("ipv4", AutoProlongRequest{ProlongRequest: ProlongRequest{IDs: []string{"68b1f0c4e13a4c0f1a2b3c4d"}, PeriodID: "1m"}})
	if err == nil || !strings.Contains(err.Error(), "paymentId is required") {
		t.Fatalf("без платёжки ожидалась локальная ошибка, получено %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("без платёжки запрос не должен уходить, ушло %d", got)
	}
}

// Тот же выбор на проводе: тело prolong/calc/ipv6 несёт orderIds, а не ids.
func TestProlongCalcSendsOrderIDsForOrderTypes(t *testing.T) {
	var body string
	client, _ := newTestClient(t, envelopeHandler(t, `{"status":"success","data":{"total":1},"errors":[]}`, &body))
	if _, err := client.ProlongCalc("ipv6", []string{"6a248de4717805635cf6057d"}, "1m", ""); err != nil {
		t.Fatalf("ProlongCalc: %v", err)
	}
	if want := `{"coupon":"","orderIds":["6a248de4717805635cf6057d"],"periodId":"1m"}`; body != want {
		t.Fatalf("тело = %s, ожидалось %s", body, want)
	}
}

// И обратная сторона: у ipv4 / isp / mobile id прокси уходит в ids, адрес — в ips.
func TestProlongCalcSendsIDsForPerProxyTypes(t *testing.T) {
	var body string
	client, _ := newTestClient(t, envelopeHandler(t, `{"status":"success","data":{"total":1},"errors":[]}`, &body))
	cases := []struct{ proxyType, value, want string }{
		{"ipv4", "68b1f0c4e13a4c0f1a2b3c4d", `{"coupon":"","ids":["68b1f0c4e13a4c0f1a2b3c4d"],"periodId":"1m"}`},
		{"mobile", "10.0.0.1:50100:50101", `{"coupon":"","ips":["10.0.0.1:50100:50101"],"periodId":"1m"}`},
	}
	for _, tc := range cases {
		if _, err := client.ProlongCalc(tc.proxyType, []string{tc.value}, "1m", ""); err != nil {
			t.Fatalf("ProlongCalc(%s): %v", tc.proxyType, err)
		}
		if body != tc.want {
			t.Fatalf("ProlongCalc(%s): тело = %s, ожидалось %s", tc.proxyType, body, tc.want)
		}
	}
}

// Поля ProlongRequest уходят под именами контракта: ids, ips, orderIds. Удалённых из контракта
// orderSeparatorIds и orderSeparatorId в теле нет; AutoProlongRequest встраивает те же поля.
func TestProlongRequestWireNames(t *testing.T) {
	encoded, err := json.Marshal(ProlongRequest{
		IDs: []string{"68b1f0c4e13a4c0f1a2b3c4d"}, IPs: []string{"1.2.3.4"},
		OrderIDs: []string{"6a248de4717805635cf6057d"}, PeriodID: "1m",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"ids":["68b1f0c4e13a4c0f1a2b3c4d"],"ips":["1.2.3.4"],"orderIds":["6a248de4717805635cf6057d"],"periodId":"1m"}`; string(encoded) != want {
		t.Fatalf("ProlongRequest = %s, ожидалось %s", encoded, want)
	}

	encoded, err = json.Marshal(AutoProlongRequest{
		ProlongRequest: ProlongRequest{OrderIDs: []string{"6a248de4717805635cf6057d"}, PaymentID: "balance"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"orderIds":["6a248de4717805635cf6057d"],"paymentId":"balance"}`; string(encoded) != want {
		t.Fatalf("AutoProlongRequest = %s, ожидалось %s", encoded, want)
	}
}

// TestOrderMixSendsMixId — идентификатор MIX-пакета должен уходить в mixId: сервер ищет пакет по
// mixId, а countryId для него — не то поле.
func TestOrderMixSendsMixId(t *testing.T) {
	c := NewClient("test-key")
	data := c.prepareMix("mix", "europe-2-mix_IPv4", "1m", 10, "", "", "")
	if got := data["mixId"]; got != "europe-2-mix_IPv4" {
		t.Fatalf("mixId = %#v, want the package code", got)
	}
	if got, present := data["countryId"]; present {
		t.Fatalf("countryId must not be sent for a mix order, got %#v", got)
	}
}

// generateAuth принимается ТОЛЬКО order/make: order/calc его молча отбрасывает, поэтому
// подмешивать его в расчёт нельзя — иначе тело расчёта отличалось бы от тела заказа.
//
// Поле OrderRequest.GenerateAuth старше клиентского SetGenerateAuth: клиентское значение —
// это умолчание на все заказы, а поле — решение для конкретного вызова. Остальные четыре
// SDK ведут себя так же, и расхождение здесь означало бы, что один и тот же код на разных
// языках создаёт разные заказы.
func TestGenerateAuthPrecedence(t *testing.T) {
	c := NewClient("test-key")
	c.SetGenerateAuth("Y")
	order := OrderRequest{SectionCode: "ipv4", CountryID: "USA", PeriodID: "1m", Quantity: 1}

	if got := c.prepareOrder(order, false).GenerateAuth; got != "" {
		t.Fatalf("order/calc: generateAuth = %q, не должен подмешиваться вовсе", got)
	}
	if got := c.prepareOrder(order, true).GenerateAuth; got != "Y" {
		t.Fatalf("order/make без поля: generateAuth = %q, ожидалось значение клиента Y", got)
	}

	explicit := order
	explicit.GenerateAuth = "N"
	if got := c.prepareOrder(explicit, true).GenerateAuth; got != "N" {
		t.Fatalf("явное поле = %q, оно обязано быть старше клиентского Y", got)
	}

	fresh := NewClient("test-key")
	if got := fresh.prepareOrder(order, true).GenerateAuth; got != "N" {
		t.Fatalf("умолчание клиента = %q, ожидалось N", got)
	}
}

// X-Fingerprint необязателен: без отпечатка резидентский и скраперный order/make уходят на сервер
// (без заголовка вовсе, а не с пустым), и SDK никогда не отбивает заказ из-за его отсутствия.
// Заданный отпечаток уходит на любую секцию, значение на вызов (OrderRequest.Fingerprint) старше
// клиентского, а order/calc заголовок не несёт.
func TestOrderMakeFingerprintIsOptional(t *testing.T) {
	var mu sync.Mutex
	var seen []string // "order/make <значение>" по запросам; <none> — заголовка не было
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		value := "<none>"
		if values := r.Header.Values(FingerprintHeader); len(values) > 0 {
			value = strings.Join(values, ",")
		}
		mu.Lock()
		seen = append(seen, r.URL.Path[strings.LastIndex(r.URL.Path, "/order/")+1:]+" "+value)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"orderId":"68b1f0c4e13a4c0f1a2b3c4d","total":1,"balance":9},"errors":[]}`))
	})
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return ""
		}
		return seen[len(seen)-1]
	}

	if _, err := client.OrderMakeResident("1-gb", ""); err != nil {
		t.Fatalf("OrderMakeResident без отпечатка не должен падать локально: %v", err)
	}
	if got := last(); got != "order/make <none>" {
		t.Fatalf("резидентский заказ без отпечатка: %q, ожидалось order/make без заголовка", got)
	}
	if _, err := client.MakeOrder(OrderRequest{SectionCode: "scraper", TarifID: "basic"}); err != nil {
		t.Fatalf("MakeOrder(scraper) без отпечатка не должен падать локально: %v", err)
	}
	if got := last(); got != "order/make <none>" {
		t.Fatalf("скраперный заказ без отпечатка: %q, ожидалось order/make без заголовка", got)
	}

	// Пробелы — это не значение: заголовок по-прежнему не уходит.
	client.SetFingerprint("   ")
	if _, err := client.OrderMakeResident("1-gb", ""); err != nil {
		t.Fatalf("OrderMakeResident с пустым отпечатком: %v", err)
	}
	if got := last(); got != "order/make <none>" {
		t.Fatalf("пустой отпечаток: %q, заголовок уходить не должен", got)
	}

	client.SetFingerprint("install-1")
	if _, err := client.OrderMakeIpv4("USA", "1m", 1, "", "", "seo"); err != nil {
		t.Fatalf("OrderMakeIpv4: %v", err)
	}
	if got := last(); got != "order/make install-1" {
		t.Fatalf("отпечаток клиента: %q, ожидалось order/make install-1", got)
	}
	if _, err := client.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", Fingerprint: "per-call"}); err != nil {
		t.Fatalf("MakeOrder(resident): %v", err)
	}
	if got := last(); got != "order/make per-call" {
		t.Fatalf("значение на вызов: %q, ожидалось order/make per-call", got)
	}

	if _, err := client.CalculateOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb"}); err != nil {
		t.Fatalf("CalculateOrder: %v", err)
	}
	if got := last(); got != "order/calc <none>" {
		t.Fatalf("order/calc: %q, заголовок уходить не должен", got)
	}
}

// Имена фильтров order/list — snake_case (order_id, start_date, is_extend, sort_by, …), а не
// camelCase, как у proxy/list. Переименование сломало бы запрос молча: он бы ушёл, а фильтр бы
// не применился.
func TestOrderListSendsSnakeCaseFilterNames(t *testing.T) {
	var captured string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		captured = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"metadata":{},"items":[]},"errors":[]}`))
	})

	if _, err := client.ListOrders(OrderListOptions{
		OrderID:   "68b1f0c4e13a4c0f1a2b3c11",
		StartDate: "01.06.2023",
		EndDate:   "30.06.2023",
		Status:    "PAYED",
		IsExtend:  "Y",
		AutoOrder: "N",
		Page:      1,
		Limit:     20,
		SortBy:    "date_insert",
		Order:     "desc",
	}); err != nil {
		t.Fatalf("ListOrders: %v", err)
	}

	query, err := url.ParseQuery(strings.SplitN(captured, "?", 2)[1])
	if err != nil {
		t.Fatalf("не разобрался query %q: %v", captured, err)
	}
	want := map[string]string{
		"order_id": "68b1f0c4e13a4c0f1a2b3c11", "start_date": "01.06.2023",
		"end_date": "30.06.2023", "status": "PAYED", "is_extend": "Y",
		"auto_order": "N", "page": "1", "limit": "20",
		"sort_by": "date_insert", "order": "desc",
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Fatalf("%s = %q, ожидалось %q (весь запрос: %s)", key, got, value, captured)
		}
	}
	if len(query) != len(want) {
		t.Fatalf("лишние параметры в %s", captured)
	}
}

// Все фильтры опциональны: без них уходит голый order/list, а не "?" с пустыми значениями.
func TestOrderListWithoutFiltersSendsBarePath(t *testing.T) {
	var captured string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		captured = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"metadata":{},"items":[]},"errors":[]}`))
	})

	if _, err := client.OrderList(); err != nil {
		t.Fatalf("OrderList: %v", err)
	}
	if strings.Contains(captured, "?") {
		t.Fatalf("без фильтров query быть не должно, получено %s", captured)
	}
	if !strings.HasSuffix(captured, "/order/list") {
		t.Fatalf("путь = %s, ожидался .../order/list", captured)
	}
}

/////////////////////////////// Ключ в ошибках (F3) ///////////////////////////////

// keyForms — формы ключа, которых не должно быть ни в одной ошибке: сам ключ, он же в нижнем и
// верхнем регистре (фронт стейджа отдаёт путь в нижнем регистре) и URL-кодированные формы.
func keyForms(key string) []string {
	return []string{key, strings.ToLower(key), strings.ToUpper(key),
		url.PathEscape(key), strings.ToLower(url.PathEscape(key)),
		url.QueryEscape(key), strings.ToLower(url.QueryEscape(key))}
}

// assertNoKey проверяет текст ошибки, её представления для fmt и JSON — у неё самой и у каждой
// ошибки по цепочке Unwrap, — а у *APIError ещё Body, сообщения и Data.
func assertNoKey(t *testing.T, what string, err error, key string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: ожидалась ошибка", what)
	}
	var views []string
	for e, depth := err, 0; e != nil && depth < 32; e, depth = errors.Unwrap(e), depth+1 {
		encoded, _ := json.Marshal(e)
		views = append(views, e.Error(), fmt.Sprintf("%v", e), fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e),
			fmt.Sprintf("%s", e), string(encoded))
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		encoded, _ := json.Marshal(apiErr)
		views = append(views, apiErr.Body, apiErr.Status, fmt.Sprintf("%#v", *apiErr), string(encoded),
			strings.Join(apiErr.Messages(), " | "), fmt.Sprintf("%#v", apiErr.Data), fmt.Sprintf("%#v", apiErr.FirstCustomData()))
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		views = append(views, urlErr.URL, fmt.Sprintf("%#v", *urlErr))
	}
	for _, view := range views {
		for _, form := range keyForms(key) {
			if strings.Contains(view, form) {
				t.Fatalf("%s: ключ (%q) виден в представлении ошибки: %s", what, form, view)
			}
		}
	}
}

// leakyTimeoutError — ошибка транспорта, которая цитирует URL запроса (так делают обёртки с
// повторами) и при этом сообщает о таймауте.
type leakyTimeoutError struct{ url string }

func (e *leakyTimeoutError) Error() string        { return "giving up on " + e.url + " after 3 attempts" }
func (e *leakyTimeoutError) Timeout() bool        { return true }
func (e *leakyTimeoutError) Is(target error) bool { return target == context.DeadlineExceeded }

// Ключ стоит в пути URL, и его эхо приходит в ошибки отовсюду: *url.Error с полным URL (порт закрыт,
// редирект, ошибка транспорта, цитирующая URL), страница 404 фронта с путём в нижнем регистре, тело
// ошибки Spring с полем path, конверт, чьё сообщение цитирует путь. Ни в тексте ошибки, ни в её
// представлениях для fmt и JSON, ни в Body, ни глубже по цепочке ключа быть не должно — ни как есть,
// ни в другом регистре, ни URL-кодированным. Второй ключ — с символами, которые кодируются.
func TestErrorsNeverCarryTheAPIKey(t *testing.T) {
	for _, key := range []string{"FakeKeyABCdef0123456789", "Fake Key+/0123abcDEF"} {
		key := key
		t.Run(key, func(t *testing.T) {
			// Порт закрыт.
			closed := httptest.NewServer(http.NotFoundHandler())
			closedURL := closed.URL
			closed.Close()
			offline := NewClient(key, WithBaseURL(closedURL+"/personal/api/v2/"))
			useFakeClock(offline, newFakeClock())
			_, err := offline.Balance()
			assertNoKey(t, "порт закрыт, balance/get", err, key)
			var urlErr *url.Error
			if !errors.As(err, &urlErr) || !strings.Contains(urlErr.URL, "/personal/api/v2/***/balance/get") {
				t.Fatalf("ожидалась *url.Error с вычищенным URL, получено %T: %v", err, err)
			}
			var opErr *net.OpError
			if !errors.As(err, &opErr) {
				t.Fatalf("вложенная ошибка без ключа должна остаться в цепочке как есть, получено %#v", err)
			}
			_, err = offline.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", PaymentCode: "balance"})
			assertNoKey(t, "порт закрыт, order/make", err, key)

			// Ошибка транспорта сама цитирует URL с ключом: она заменяется целиком, но признаки
			// таймаута и дедлайна остаются, а сырая ошибка не достаётся даже через errors.As.
			leaky := NewClient(key, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return nil, &leakyTimeoutError{url: r.URL.String()}
			})}))
			useFakeClock(leaky, newFakeClock())
			_, err = leaky.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", PaymentCode: "balance"})
			assertNoKey(t, "транспорт цитирует URL", err, key)
			if !os.IsTimeout(err) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("признаки таймаута потеряны: %v", err)
			}
			var raw *leakyTimeoutError
			if errors.As(err, &raw) {
				t.Fatal("сырая ошибка с ключом осталась в цепочке")
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				switch {
				case strings.HasSuffix(path, "/balance/get"):
					// страница 404 фронта: канонический URL в нижнем регистре
					w.Header().Set("Content-Type", "text/html")
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprintf(w, `<html><head><link rel="canonical" href="https://front.example%s/"/></head><body>Not found</body></html>`, strings.ToLower(path))
				case strings.HasSuffix(path, "/order/make"), strings.HasSuffix(path, "/order/list"):
					// Spring: путь в поле path
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprintf(w, `{"timestamp":"2026-10-01T12:00:00.000+00:00","status":500,"error":"Internal Server Error","path":%q,"raw":%q}`, path, r.URL.EscapedPath())
				case strings.HasSuffix(path, "/proxy/replace"):
					// конверт, сообщение, customData и data которого цитируют путь
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"status":"error","data":{"path":%q,"list":[%q]},"errors":[{"message":"Unknown path %s","code":0,"customData":{"path":%q}}]}`,
						r.URL.EscapedPath(), strings.ToLower(path), strings.ToUpper(path), path)
				case strings.HasSuffix(path, "/auth/add/ip"):
					// 200 HTML без конверта на запись
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprintf(w, "<html><body>%s %s</body></html>", r.URL.EscapedPath(), strings.ToLower(path))
				default:
					// бесконечный редирект на тот же путь в нижнем регистре
					http.Redirect(w, r, strings.ToLower(r.URL.EscapedPath())+"/", http.StatusFound)
				}
			}))
			t.Cleanup(server.Close)
			client := NewClient(key, WithBaseURL(server.URL+"/personal/api/v2/"))
			useFakeClock(client, newFakeClock())

			_, err = client.Balance()
			assertNoKey(t, "404 фронта", err, key)
			expectHTTPStatus(t, err, http.StatusNotFound)

			_, err = client.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", PaymentCode: "balance"})
			assertNoKey(t, "500 Spring на order/make", err, key)
			if !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("500 без конверта на order/make: ожидалась ErrUnexpectedResponse, получено %v", err)
			}

			_, err = client.ListOrders(OrderListOptions{})
			assertNoKey(t, "500 Spring на order/list", err, key)
			if apiErr := expectHTTPStatus(t, err, http.StatusInternalServerError); !strings.Contains(apiErr.Body, `"path":"/personal/api/v2/***/order/list"`) {
				t.Fatalf("путь в теле должен остаться, а ключ — смениться на ***: %s", apiErr.Body)
			}

			_, err = client.ProxyReplace([]string{testProxyID}, ProxyReplaceReasonNotWork, "")
			assertNoKey(t, "конверт цитирует путь", err, key)
			if messages := expectHTTPStatus(t, err, http.StatusOK).Messages(); len(messages) != 1 || !strings.Contains(messages[0], "/***/") {
				t.Fatalf("сообщение должно сохраниться без ключа: %#v", messages)
			}

			_, err = client.AuthAddIp("LH-1", "1.2.3.4")
			assertNoKey(t, "200 HTML на запись", err, key)

			_, err = client.ResidentPackage()
			assertNoKey(t, "редирект в нижний регистр", err, key)
			if !errors.As(err, &urlErr) || !strings.Contains(urlErr.URL, "***") {
				t.Fatalf("ожидалась *url.Error редиректа с вычищенным URL, получено %T: %v", err, err)
			}

			// Сам клиент тоже печатается без ключа — и напрямую, и полем чужой структуры.
			for _, view := range []string{fmt.Sprintf("%v", client), fmt.Sprintf("%+v", client), fmt.Sprintf("%#v", client),
				fmt.Sprintf("%s", client), fmt.Sprintf("%+v", struct{ API *Client }{client})} {
				for _, form := range keyForms(key) {
					if strings.Contains(view, form) {
						t.Fatalf("ключ (%q) виден в представлении клиента: %s", form, view)
					}
				}
				if !strings.Contains(view, `"***"`) {
					t.Fatalf("представление клиента должно показывать ключ как ***: %s", view)
				}
			}
		})
	}
}

// keyRedactor ловит ключ в любом регистре и в URL-кодированных формах, пустой ключ ничего не трогает.
func TestKeyRedactorForms(t *testing.T) {
	redact := keyRedactor("Fake Key+/0123abcDEF")
	cases := map[string]string{
		"/v2/Fake Key+/0123abcDEF/balance/get":              "/v2/***/balance/get",
		"/v2/fake key+/0123abcdef/balance/get":              "/v2/***/balance/get",
		"/v2/FAKE KEY+/0123ABCDEF/balance/get":              "/v2/***/balance/get",
		"/v2/Fake%20Key+%2F0123abcDEF/balance/get":          "/v2/***/balance/get",
		"/v2/fake%20key+%2f0123abcdef/balance/get":          "/v2/***/balance/get",
		"?key=Fake+Key%2B%2F0123abcDEF&x=1":                 "?key=***&x=1",
		"twice: Fake Key+/0123abcDEF, fake key+/0123abcdef": "twice: ***, ***",
		"no key here": "no key here",
		"":            "",
	}
	for input, want := range cases {
		if got := redact(input); got != want {
			t.Errorf("redact(%q) = %q, want %q", input, got, want)
		}
	}
	if got := keyRedactor("")("FakeKey"); got != "FakeKey" {
		t.Fatalf("пустой ключ не должен ничего вычищать, получено %q", got)
	}
	// Тело без конверта обрезается после вычистки: ключ на границе не остаётся наполовину.
	body := strings.Repeat("x", maxErrorBodyLength-3) + "Fake Key+/0123abcDEF" + strings.Repeat("y", 100)
	got := errorBody([]byte(body), false, redact)
	if strings.Contains(got, "Fake") || !strings.HasPrefix(got, strings.Repeat("x", maxErrorBodyLength-3)+"***") ||
		!strings.Contains(got, fmt.Sprintf("[truncated, %d bytes in total]", len(body))) {
		t.Fatalf("тело обрезано неверно: %q", got)
	}
	if got := errorBody([]byte(`{"status":"error","path":"/Fake Key+/0123abcDEF/`+strings.Repeat("z", 600)+`"}`), true, redact); strings.Contains(got, "truncated") || strings.Contains(got, "Fake") {
		t.Fatalf("тело конверта не обрезается, но ключ из него вычищается: %q", got)
	}
}

/////////////////////////////// Строгий успех money и write (F4) ///////////////////////////////

// stubStatus отвечает на каждый запрос одним и тем же статусом и телом.
func stubStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// strictCalls — денежные и записывающие вызовы, на которых успехом считается только
// status == "success".
func strictCalls() map[string]func(*Client) error {
	return map[string]func(*Client) error{
		"MakeOrder (money)": func(c *Client) error {
			_, err := c.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", PaymentCode: "balance"})
			return err
		},
		"OrderMakeIpv4 (money)": func(c *Client) error {
			_, err := c.OrderMakeIpv4("USA", "1m", 1, "", "", "seo")
			return err
		},
		"MakeProlong (money)": func(c *Client) error {
			_, err := c.MakeProlong("ipv4", ProlongRequest{IDs: []string{testProxyID}, PeriodID: "1m"})
			return err
		},
		"AddBalance (money)": func(c *Client) error { _, err := c.AddBalance(10, testProxyID); return err },
		"EnableAutoProlong (write)": func(c *Client) error {
			_, err := c.EnableAutoProlong("ipv4", AutoProlongRequest{ProlongRequest: ProlongRequest{
				IDs: []string{testProxyID}, PeriodID: "1m", PaymentID: "balance"}})
			return err
		},
		"ProxyReplace (write)": func(c *Client) error {
			_, err := c.ProxyReplace([]string{testProxyID}, ProxyReplaceReasonNotWork, "")
			return err
		},
		"ResidentsubuserListDelete (write, DELETE)": func(c *Client) error {
			_, err := c.ResidentsubuserListDelete("f9ea7063d7a699b12b3e", "123")
			return err
		},
	}
}

// На money и write успех — только status == "success". status="error" — ошибка, даже с data и
// пустым errors[]. Ответ без JSON-конверта (HTML, пустое тело, 204, не-объект, обрезанный JSON,
// неизвестный status) со статусом 2xx или 5xx — ошибка с причиной ErrUnexpectedResponse и понятным
// текстом: запрос мог выполниться. Раньше status="error" с data проходил успехом на любом маршруте,
// а HTML 200 давал голую *json.SyntaxError.
func TestMoneyAndWriteCallsSucceedOnlyOnStatusSuccess(t *testing.T) {
	gatewayPage := "<!DOCTYPE html><html><body>" + strings.Repeat("<p>Bad gateway</p>", 60) + "</body></html>"
	cases := []struct {
		name       string
		status     int
		body       string
		unexpected bool // ожидается ErrUnexpectedResponse
		code       int  // код, который должен быть в errors[]; 0 — проверять не нужно
	}{
		{"error with errors[]", 200, `{"status":"error","data":null,"errors":[{"code":16,"message":"Insufficient funds on balance"}]}`, false, 16},
		{"error with data and an empty errors[]", 200, `{"status":"error","data":{"warning":"Insufficient funds. Total $2. Not enough $33.10","total":35.1},"errors":[]}`, false, 0},
		{"HTML 200", 200, gatewayPage, true, 0},
		{"empty body", 200, "", true, 0},
		{"204 No Content", 204, "", true, 0},
		{"JSON null", 200, "null", true, 0},
		{"array", 200, `[1,2]`, true, 0},
		{"string", 200, `"ok"`, true, 0},
		{"truncated JSON", 200, `{"status":"success","data":{"orderId":"68b1f0c4`, true, 0},
		{"object without status", 200, `{}`, true, 0},
		{"unknown status", 200, `{"status":"pending","data":{"orderId":"68b1f0c4e13a4c0f1a2b3c4d"}}`, true, 0},
		{"HTTP 502 gateway page", 502, gatewayPage, true, 0},
		{"HTTP 504 empty", 504, "", true, 0},
		{"HTTP 500 Spring body", 500, `{"timestamp":"2026-10-01T12:00:00.000+00:00","status":500,"error":"Internal Server Error","path":"/x"}`, true, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestClient(t, stubStatus(tc.status, tc.body))
			for name, call := range strictCalls() {
				err := call(client)
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("%s: ожидалась *APIError, получено %T: %v", name, err, err)
				}
				if apiErr.HTTPStatus != tc.status {
					t.Fatalf("%s: HTTPStatus = %d, ожидался %d", name, apiErr.HTTPStatus, tc.status)
				}
				if got := errors.Is(err, ErrUnexpectedResponse); got != tc.unexpected {
					t.Fatalf("%s: errors.Is(err, ErrUnexpectedResponse) = %v, ожидалось %v (%v)", name, got, tc.unexpected, err)
				}
				if tc.code != 0 && !apiErr.HasCode(tc.code) {
					t.Fatalf("%s: код %d потерян: %v", name, tc.code, err)
				}
				if !tc.unexpected {
					continue
				}
				if text := err.Error(); !strings.Contains(text, fmt.Sprintf("HTTP %d", tc.status)) ||
					!strings.Contains(text, "no JSON envelope") || !strings.Contains(text, "may have been executed") {
					t.Fatalf("%s: текст ошибки должен назвать статус и предупредить о возможном выполнении: %q", name, text)
				}
				if len(apiErr.Errors) != 0 {
					t.Fatalf("%s: сервер ошибок не присылал, а в Errors что-то есть: %#v", name, apiErr.Errors)
				}
				if tc.body == gatewayPage && (!strings.HasPrefix(apiErr.Body, gatewayPage[:maxErrorBodyLength]) ||
					!strings.Contains(apiErr.Body, fmt.Sprintf("[truncated, %d bytes in total]", len(gatewayPage)))) {
					t.Fatalf("%s: тело без конверта должно быть обрезано до %d символов: %q", name, maxErrorBodyLength, apiErr.Body)
				}
			}
		})
	}

	t.Run("status error keeps its data", func(t *testing.T) {
		client, _ := newTestClient(t, stubStatus(200, `{"status":"error","data":{"warning":"Insufficient funds. Total $2. Not enough $33.10"},"errors":[]}`))
		_, err := client.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", PaymentCode: "balance"})
		apiErr := expectHTTPStatus(t, err, http.StatusOK)
		if data, _ := apiErr.Data.(map[string]interface{}); apiErr.Status != "error" || data["warning"] == nil {
			t.Fatalf("status и data ответа должны остаться в ошибке: %#v", apiErr)
		}
		if got := err.Error(); got != "client api error: Insufficient funds. Total $2. Not enough $33.10" {
			t.Fatalf("текст ошибки должен назвать причину из data.warning, получено %q", got)
		}
	})

	t.Run("4xx and the edge 429 stay plain errors", func(t *testing.T) {
		for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests} {
			client, _ := newTestClient(t, stubStatus(status, "<html>nope</html>"))
			client.requestLimiter().maxRetries = 0
			for name, call := range strictCalls() {
				err := call(client)
				expectHTTPStatus(t, err, status)
				if errors.Is(err, ErrUnexpectedResponse) {
					t.Fatalf("%s, HTTP %d: до API запрос не дошёл, ErrUnexpectedResponse тут лишняя: %v", name, status, err)
				}
			}
		}
	})

	t.Run("success is a success", func(t *testing.T) {
		client, _ := newTestClient(t, stubStatus(200, okEnvelope))
		for name, call := range strictCalls() {
			if err := call(client); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
}

// Чтения, calc и выгрузки разбираются как раньше: «нехватка средств» у calc — status="error" с
// data и пустым errors[] — по-прежнему отдаёт data без ошибки, HTML 200 на чтении — прежняя ошибка
// разбора, а выгрузка отдаёт тело как есть. Delete с not-found внутри успешного конверта — тоже как
// раньше.
func TestReadsAndCalcKeepTheLenientParsing(t *testing.T) {
	warning := `{"status":"error","data":{"warning":"Insufficient funds. Total $2. Not enough $33.10","total":35.1},"errors":[]}`
	client, _ := newTestClient(t, stubStatus(200, warning))
	calcs := map[string]func() (map[string]interface{}, error){
		"CalculateOrder": func() (map[string]interface{}, error) {
			return client.CalculateOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb"})
		},
		"OrderCalcIpv4": func() (map[string]interface{}, error) { return client.OrderCalcIpv4("USA", "1m", 1, "", "", "seo") },
		"CalculateProlong": func() (map[string]interface{}, error) {
			return client.CalculateProlong("ipv4", ProlongRequest{IDs: []string{testProxyID}, PeriodID: "1m"})
		},
		"ProlongCalc": func() (map[string]interface{}, error) {
			return client.ProlongCalc("ipv4", []string{testProxyID}, "1m", "")
		},
		"CalculateAutoProlong": func() (map[string]interface{}, error) {
			return client.CalculateAutoProlong("ipv4", AutoProlongRequest{ProlongRequest: ProlongRequest{
				IDs: []string{testProxyID}, PeriodID: "1m", PaymentID: "balance"}})
		},
		"ListOrders (a read)": func() (map[string]interface{}, error) { return client.ListOrders(OrderListOptions{}) },
	}
	for name, calc := range calcs {
		data, err := calc()
		if err != nil || data["warning"] != "Insufficient funds. Total $2. Not enough $33.10" {
			t.Fatalf("%s: нехватка средств обязана отдавать data без ошибки, получено %#v / %v", name, data, err)
		}
	}

	html := "<!DOCTYPE html><html><body>SPA</body></html>"
	client, _ = newTestClient(t, stubStatus(200, html))
	_, err := client.Balance()
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) || errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("HTML 200 на чтении — прежняя ошибка разбора, получено %T: %v", err, err)
	}
	if file, err := client.DownloadProxies("ipv4", ProxyDownloadOptions{Ext: "txt"}); err != nil || string(file) != html {
		t.Fatalf("выгрузка отдаёт тело как есть, получено %q / %v", file, err)
	}

	client, _ = newTestClient(t, stubStatus(200, `{"status":"success","data":"{\"status\":\"not-found\"}","errors":[]}`))
	if got, err := client.ResidentsubuserListDelete("f9ea7063d7a699b12b3e", "123"); err != nil || got["status"] != "not-found" {
		t.Fatalf("not-found внутри успешного конверта разбирается как раньше, получено %#v / %v", got, err)
	}
}

/////////////////////////////// Таймаут денежных вызовов (F5) ///////////////////////////////

// timeoutsOf — таймаут обычного и денежного запроса клиента, как их применит doWithHeaders.
func timeoutsOf(t *testing.T, c *Client) (general, money time.Duration) {
	t.Helper()
	_, _, httpClient, err := c.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return httpClient.Timeout, c.moneyHTTPClient(httpClient).Timeout
}

// countingTransport считает запросы и отдаёт их http.DefaultTransport.
type countingTransport struct{ calls int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	atomic.AddInt32(&c.calls, 1)
	return http.DefaultTransport.RoundTrip(r)
}

// Денежный запрос ждёт max(общий таймаут, money-таймаут), по умолчанию 30 и 120 с. WithMoneyTimeout(0)
// отключает отдельный таймаут. Пользовательский *http.Client не меняется: для денежного запроса
// берётся его копия с тем же Transport, Jar и CheckRedirect, а клиент без таймаута так и остаётся без него.
func TestMoneyTimeoutDefaultsAndOptions(t *testing.T) {
	zero := &Client{}
	zero.SetAPIKey("k")
	cases := []struct {
		name           string
		client         *Client
		general, money time.Duration
	}{
		{"NewClient", NewClient("k"), 30 * time.Second, 120 * time.Second},
		{"a Client not made by NewClient", zero, 30 * time.Second, 120 * time.Second},
		{"a shorter WithTimeout", NewClient("k", WithTimeout(15*time.Second)), 15 * time.Second, 120 * time.Second},
		{"a longer WithTimeout wins", NewClient("k", WithTimeout(5*time.Minute)), 5 * time.Minute, 5 * time.Minute},
		{"WithMoneyTimeout", NewClient("k", WithMoneyTimeout(time.Minute)), 30 * time.Second, time.Minute},
		{"WithMoneyTimeout below WithTimeout", NewClient("k", WithMoneyTimeout(10*time.Second)), 30 * time.Second, 30 * time.Second},
		{"WithMoneyTimeout(0) turns it off", NewClient("k", WithMoneyTimeout(0)), 30 * time.Second, 30 * time.Second},
		{"a negative WithMoneyTimeout too", NewClient("k", WithMoneyTimeout(-time.Second)), 30 * time.Second, 30 * time.Second},
		{"WithHTTPClient", NewClient("k", WithHTTPClient(&http.Client{Timeout: 45 * time.Second})), 45 * time.Second, 120 * time.Second},
		{"WithHTTPClient without a timeout", NewClient("k", WithHTTPClient(&http.Client{})), 0, 0},
	}
	for _, tc := range cases {
		if general, money := timeoutsOf(t, tc.client); general != tc.general || money != tc.money {
			t.Errorf("%s: timeouts %v / %v, want %v / %v", tc.name, general, money, tc.general, tc.money)
		}
	}
	if DefaultClient.moneyTimeout != defaultMoneyTimeout {
		t.Fatalf("DefaultClient: money timeout %v, want %v", DefaultClient.moneyTimeout, defaultMoneyTimeout)
	}

	transport := &countingTransport{}
	jar, _ := cookiejar.New(nil)
	checkRedirect := func(*http.Request, []*http.Request) error { return nil }
	user := &http.Client{Timeout: 10 * time.Second, Transport: transport, Jar: jar, CheckRedirect: checkRedirect}
	client := NewClient("k", WithHTTPClient(user))
	_, _, used, _ := client.snapshot()
	money := client.moneyHTTPClient(used)
	if used != user || user.Timeout != 10*time.Second {
		t.Fatalf("пользовательский клиент изменён: %#v", user)
	}
	if money == user || money.Timeout != 120*time.Second || money.Transport != transport || money.Jar != jar ||
		reflect.ValueOf(money.CheckRedirect).Pointer() != reflect.ValueOf(checkRedirect).Pointer() {
		t.Fatalf("денежный запрос должен идти через копию с тем же Transport, Jar и CheckRedirect: %#v", money)
	}
}

// Денежный запрос не обрезается общим таймаутом, остальные — обрезаются, как раньше. Сервер
// отвечает за 400 мс, общий таймаут — 150 мс, денежный — 5 с.
func TestMoneyCallsOutliveTheClientTimeout(t *testing.T) {
	const delay, short = 400 * time.Millisecond, 150 * time.Millisecond
	slow := func(w http.ResponseWriter, r *http.Request, n int) {
		time.Sleep(delay)
		writeStubResponse(w, http.StatusOK, okEnvelope)
	}
	expectTimeout := func(t *testing.T, what string, err error) {
		t.Helper()
		if !os.IsTimeout(err) {
			t.Fatalf("%s: ожидался таймаут, получено %v", what, err)
		}
		if strings.Contains(err.Error(), "test-key") {
			t.Fatalf("%s: ключ в тексте ошибки: %v", what, err)
		}
	}

	client, _, _ := newPacedClient(t, slow, WithTimeout(short), WithMoneyTimeout(5*time.Second))
	ok := noErr(t)
	ok(client.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", PaymentCode: "balance"}))
	ok(client.AddBalance(10, testProxyID))
	ok(client.ProlongMake("ipv4", []string{testProxyID}, "1m", ""))
	_, err := client.Balance()
	expectTimeout(t, "чтение", err)
	_, err = client.SetProxyComment([]string{testProxyID}, "note")
	expectTimeout(t, "запись", err)

	// Пользовательский клиент: денежный запрос идёт через его Transport, сам клиент не меняется.
	transport := &countingTransport{}
	user := &http.Client{Timeout: short, Transport: transport}
	client, _, _ = newPacedClient(t, slow, WithHTTPClient(user), WithMoneyTimeout(5*time.Second))
	ok(client.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", PaymentCode: "balance"}))
	_, err = client.Balance()
	expectTimeout(t, "чтение через пользовательский клиент", err)
	if got := atomic.LoadInt32(&transport.calls); got != 2 || user.Timeout != short {
		t.Fatalf("оба запроса должны пройти через Transport пользователя (%d), его Timeout — остаться %v (%v)", got, short, user.Timeout)
	}

	// WithMoneyTimeout(0): денежный запрос ждёт столько же, сколько любой другой.
	client, _, _ = newPacedClient(t, slow, WithTimeout(short), WithMoneyTimeout(0))
	_, err = client.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", PaymentCode: "balance"})
	expectTimeout(t, "order/make с WithMoneyTimeout(0)", err)
}

/////////////////////////////// DefaultClient и пакетные функции (F6) ///////////////////////////////

// loopbackOnly пускает запросы только на локальный стенд: тест подменяет DefaultClient и URL, и
// ошибка в этой логике не должна увести запрос на прод даже с ключом-заглушкой.
type loopbackOnly struct{}

func (loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if host := r.URL.Hostname(); host != "127.0.0.1" && host != "::1" && host != "localhost" {
		return nil, fmt.Errorf("test transport refuses a request to %s", host)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// DefaultClient, заменённый на NewClient(key, WithBaseURL(mock)), как советует README, обслуживает
// пакетные функции по своему адресу: раньше legacyClient на каждом вызове сбрасывал его на URL, а
// URL по умолчанию — прод. URL подменяет адрес, только пока отличается от DefaultBaseURL; вернули
// её к умолчанию (или очистили) — клиент снова ходит по своему адресу, в том числе по заданному
// SetBaseURL во время подмены.
func TestPackageFunctionsHonorAReplacedDefaultClient(t *testing.T) {
	oldDefault, oldURL := DefaultClient, URL
	t.Cleanup(func() { DefaultClient, URL = oldDefault, oldURL })
	stub := func() (string, *int32) {
		hits := new(int32)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(hits, 1)
			writeStubResponse(w, http.StatusOK, okEnvelope)
		}))
		t.Cleanup(server.Close)
		return server.URL + "/personal/api/v2/", hits
	}
	mockURL, mockHits := stub()
	otherURL, otherHits := stub()
	thirdURL, thirdHits := stub()
	expectHits := func(what string, want ...int32) {
		t.Helper()
		got := []int32{atomic.LoadInt32(mockHits), atomic.LoadInt32(otherHits), atomic.LoadInt32(thirdHits)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: mock/other/third получили %v, ожидалось %v", what, got, want)
		}
	}
	balance := func() {
		t.Helper()
		if _, err := BalanceE(); err != nil {
			t.Fatalf("BalanceE: %v", err)
		}
	}

	URL = DefaultBaseURL
	DefaultClient = NewClient("test-key", WithBaseURL(mockURL), WithHTTPClient(&http.Client{Transport: loopbackOnly{}}))
	useFakeClock(DefaultClient, newFakeClock())
	SetPaymentCode("balance")
	balance()
	if _, err := OrderMakeIpv4("USA", "1m", 1, "", "", "seo"); err != nil {
		t.Fatalf("OrderMakeIpv4: %v", err)
	}
	expectHits("заменённый DefaultClient", 2, 0, 0)

	URL = otherURL
	balance()
	expectHits("URL изменён", 2, 1, 0)
	URL = DefaultBaseURL
	balance()
	expectHits("URL вернули к умолчанию", 3, 1, 0)
	URL = otherURL
	balance()
	URL = ""
	balance()
	expectHits("URL очищен", 4, 2, 0)

	URL = otherURL
	balance() // подмена действует
	DefaultClient.SetBaseURL(thirdURL)
	balance()
	expectHits("SetBaseURL во время подмены: URL всё ещё главнее", 4, 4, 0)
	URL = DefaultBaseURL
	balance()
	expectHits("после подмены — адрес из SetBaseURL, а не прежний", 4, 4, 1)
}

// Пакетные сеттеры состояния и URL помечены Deprecated со ссылкой на NewClient — все пакетные Set*,
// включая будущие.
func TestPackageLevelSettersAreDeprecated(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "sdk.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse sdk.go: %v", err)
	}
	docs := map[string]string{}
	var setters []string
	for _, decl := range file.Decls {
		switch declaration := decl.(type) {
		case *ast.FuncDecl:
			if declaration.Recv != nil {
				continue
			}
			if strings.HasPrefix(declaration.Name.Name, "Set") {
				setters = append(setters, declaration.Name.Name)
			}
			if declaration.Doc != nil {
				docs[declaration.Name.Name] = declaration.Doc.Text()
			}
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				doc := value.Doc
				if doc == nil {
					doc = declaration.Doc
				}
				for _, name := range value.Names {
					if doc != nil {
						docs[name.Name] = doc.Text()
					}
				}
			}
		}
	}
	want := []string{"SetApiKey", "SetPaymentId", "SetPaymentCode", "SetGenerateAuth", "SetFingerprint"}
	if !reflect.DeepEqual(setters, want) {
		t.Fatalf("пакетные Set* = %v, ожидались %v: новый сеттер тоже должен получить Deprecated", setters, want)
	}
	for _, name := range append(want, "URL") {
		doc := docs[name]
		at := strings.Index("\n"+doc, "\nDeprecated: ")
		if at < 0 || !strings.Contains(doc[at:], "NewClient") {
			t.Errorf("%s: нет абзаца \"Deprecated: ...\" со ссылкой на NewClient:\n%s", name, doc)
		}
	}
}

/////////////////////////////// F1 и F2 не сломаны ///////////////////////////////

// Транспорт Go не переотправляет POST без Idempotency-Key, и копия http-клиента для денежного
// запроса (F5) этого не меняет: соединение из пула оборвалось после того, как сервер прочитал
// order/make, — запрос пришёл ровно один раз, а вызов вернул ошибку без ключа.
func TestMoneyCallIsNotReplayedAfterADroppedConnection(t *testing.T) {
	var orders int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/order/make") {
			writeStubResponse(w, http.StatusOK, okEnvelope)
			return
		}
		atomic.AddInt32(&orders, 1)
		_, _ = io.ReadAll(r.Body)
		hijacker, isHijacker := w.(http.Hijacker)
		if !isHijacker {
			t.Error("the stub cannot hijack the connection")
			return
		}
		if conn, _, err := hijacker.Hijack(); err == nil {
			_ = conn.Close()
		}
	})
	noErr(t)(client.Balance()) // соединение остаётся в пуле keep-alive
	_, err := client.MakeOrder(OrderRequest{SectionCode: "resident", TarifID: "1-gb", PaymentCode: "balance"})
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || strings.Contains(err.Error(), "test-key") {
		t.Fatalf("ожидалась *url.Error без ключа, получено %T: %v", err, err)
	}
	if got := atomic.LoadInt32(&orders); got != 1 {
		t.Fatalf("order/make пришёл %d раз, ожидался ровно один", got)
	}
}

// Платёжка из вызова главнее платёжки клиента: клиентская пара (SetPaymentCode / SetPaymentID)
// подмешивается, только когда в вызове нет ни id, ни кода.
func TestCallPaymentOutranksClientPayment(t *testing.T) {
	var body string
	client, _ := newTestClient(t, envelopeHandler(t, `{"status":"success","data":{"ok":true,"url":"https://pay"},"errors":[]}`, &body))
	order := OrderRequest{SectionCode: "resident", TarifID: "1-gb"}
	prolong := ProlongRequest{IDs: []string{testProxyID}, PeriodID: "1m"}
	calls := map[string]func(id, code string) error{
		"CalculateOrder": func(id, code string) error {
			request := order
			request.PaymentID, request.PaymentCode = id, code
			_, err := client.CalculateOrder(request)
			return err
		},
		"MakeOrder": func(id, code string) error {
			request := order
			request.PaymentID, request.PaymentCode = id, code
			_, err := client.MakeOrder(request)
			return err
		},
		"CalculateProlong": func(id, code string) error {
			request := prolong
			request.PaymentID, request.PaymentCode = id, code
			_, err := client.CalculateProlong("ipv4", request)
			return err
		},
		"MakeProlong": func(id, code string) error {
			request := prolong
			request.PaymentID, request.PaymentCode = id, code
			_, err := client.MakeProlong("ipv4", request)
			return err
		},
		"CalculateAutoProlong": func(id, code string) error {
			request := AutoProlongRequest{ProlongRequest: prolong}
			request.PaymentID, request.PaymentCode = id, code
			_, err := client.CalculateAutoProlong("ipv4", request)
			return err
		},
		"EnableAutoProlong": func(id, code string) error {
			request := AutoProlongRequest{ProlongRequest: prolong}
			request.PaymentID, request.PaymentCode = id, code
			_, err := client.EnableAutoProlong("ipv4", request)
			return err
		},
	}
	payment := func() (interface{}, interface{}) {
		var sent map[string]interface{}
		if err := json.Unmarshal([]byte(body), &sent); err != nil {
			t.Fatalf("тело %q: %v", body, err)
		}
		return sent["paymentId"], sent["paymentCode"]
	}
	for name, call := range calls {
		client.SetPaymentCode("paddle_subscription")
		if err := call("balance", ""); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if id, code := payment(); id != "balance" || code != nil {
			t.Fatalf("%s: клиентский код + paymentId вызова: ушло paymentId=%v paymentCode=%v", name, id, code)
		}
		client.SetPaymentID("balance")
		if err := call("", "paddle_subscription"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if id, code := payment(); id != nil || code != "paddle_subscription" {
			t.Fatalf("%s: клиентский id + paymentCode вызова: ушло paymentId=%v paymentCode=%v", name, id, code)
		}
	}

	client.SetPaymentID("68b1f0c4e13a4c0f1a2b3c4d")
	if _, err := client.AddBalance(10, "68b1f0c4e13a4c0f1a2b3c51"); err != nil {
		t.Fatalf("AddBalance: %v", err)
	}
	if id, code := payment(); id != "68b1f0c4e13a4c0f1a2b3c51" || code != nil {
		t.Fatalf("AddBalance: paymentId вызова должен быть главнее клиентского, ушло %v / %v", id, code)
	}
}
