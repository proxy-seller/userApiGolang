package userApiGolang

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// newTestClient поднимает локальный стенд и отдаёт клиент, созданный через NewClient —
// именно тот путь, на котором раньше падала половина SDK ("client api key is required").
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient("test-key", WithBaseURL(server.URL+"/personal/api/v2/")), server
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
// Бэкенд (ClientApiService.parseMixSelection) резолвит mix не только по mixId/mixCode,
// но и через countryId: строкой "packageId:quantity" либо countryId=packageId вместе с
// quantity. Если mix распознан, requiresClientApiGoal возвращает false и customTargetName
// не нужен. Тест фиксирует именно это, потому что раньше гейт проверял только mixId/mixCode
// и блокировал legacy-путь OrderCalcMix/OrderMakeMix (пакет уезжает в countryId).
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

// errors[].code на бэкенде — int (ProxySellerApiErrorItemDto.code / ClientApiErrorsDto.code /
// ApiErrorDto.code). Пока поле лежало в interface{}, encoding/json давал float64 и любое
// ветвление по коду было тихо ложным. Тест фиксирует типизацию.
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
// тройкой (LegacyClientApiErrorHelper.legacyAccessErrors). Клиент обязан видеть весь массив.
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
// плайн-текстом мимо конверта (ResidentUserApiService.downloadProxyList).
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

// proxy/replace: type — ПРИЧИНА замены (enum ProxyReplaceType), а не тип прокси;
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
	// сервер делает value.toUpperCase() — регистр не важен
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

// balance/add — единственная точка, где paymentCode НЕ резолвится (normalizeOrderReferenceCodes
// там не вызывается). Раньше SDK молча уезжал с paymentId="".
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
func TestAutoTopupSetSendsOnlyProvidedFields(t *testing.T) {
	var body string
	state := `{"status":"success","data":{"configured":true,"enabled":true,"state":"ACTIVE",` +
		`"threshold":5,"amount":10,"subscriptionId":"sub_1","dailyCountCap":3,` +
		`"monthlyAmountCap":100,"failCount":0,` +
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
	if got.DailyCountCap == nil || *got.DailyCountCap != 3 {
		t.Fatalf("dailyCountCap разобран неверно: %#v", got.DailyCountCap)
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

// Границы значений приходят в errors[0].customData (ClientApiService.setAutoTopup),
// коды 49-56. Клиент обязан иметь к ним доступ.
func TestAutoTopupLimitsFromError(t *testing.T) {
	body := `{"status":"error","data":null,"errors":[{"message":"Top-up amount must be 5 or more","code":51,` +
		`"customData":{"minAmount":5,"minThreshold":1,"minDailyCountCap":2}}]}`
	_, err := parseEnvelope([]byte(body), http.StatusOK)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("ожидался *APIError, получено %T", err)
	}
	if !apiErr.HasCode(51) {
		t.Fatal("код 51 (AUTO_TOPUP_MIN_AMOUNT) должен быть доступен")
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
	if limits.MinDailyCountCap == nil || *limits.MinDailyCountCap != 2 {
		t.Fatalf("minDailyCountCap разобран неверно: %#v", limits.MinDailyCountCap)
	}
	if apiErr.FirstCustomData() == nil {
		t.Fatal("FirstCustomData обязан отдавать customData")
	}
	if _, found = AutoTopupLimitsFromError(nil); found {
		t.Fatal("nil-ошибка не содержит границ")
	}
}

// Типы listId — по серверным DTO: resident/list/* принимает Long, а
// residentsubuser/list/delete — String (@NotBlank).
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

// ids во всех DTO — Set<String> с ObjectId; ветки []int больше нет, значения не-string
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

// prolong/make при нехватке средств отдаёт status="error" с ПУСТЫМ errors[] и calc-данными
// в data (ProlongMakeResponseClientDto.ofInsufficientFunds, ClientApiService.groovy:3185) —
// ту же форму, что легитимный warning у prolong/calc. Раньше parseEnvelope отдавал это как
// успех, и несостоявшееся продление было неотличимо от состоявшегося.
func TestProlongMakeInsufficientFundsIsAnError(t *testing.T) {
	insufficient := `{"status":"error","data":{"warning":"Insufficient funds. Total 3.0000. Not enough $7.00","balance":3,"total":10},"errors":[]}`

	client, _ := newTestClient(t, envelopeHandler(t, insufficient, nil))
	data, err := client.ProlongMake("ipv4", []string{"68b1f0c4e13a4c0f1a2b3c4d"}, "1m", "")
	if err == nil {
		t.Fatal("нехватка средств обязана возвращать ошибку, а не выглядеть успехом")
	}
	if !strings.Contains(err.Error(), "Not enough") {
		t.Fatalf("текст warning должен попадать в ошибку, получено: %v", err)
	}
	// данные конверта остаются доступны для разбора
	if data == nil || data["balance"] == nil {
		t.Fatalf("calc-данные должны возвращаться вместе с ошибкой, получено %#v", data)
	}

	// типизированный путь ведёт себя так же
	client2, _ := newTestClient(t, envelopeHandler(t, insufficient, nil))
	if _, err = client2.MakeProlong("ipv4", ProlongRequest{IDs: []string{"x"}, PeriodID: "1m"}); err == nil {
		t.Fatal("MakeProlong тоже обязан вернуть ошибку при нехватке средств")
	}

	// успешное продление по-прежнему проходит
	ok := `{"status":"success","data":{"orderId":"68b1f0c4e13a4c0f1a2b3c4d","total":10,"balance":90,"listBaseOrderNumbers":[]},"errors":[]}`
	client3, _ := newTestClient(t, envelopeHandler(t, ok, nil))
	data, err = client3.ProlongMake("ipv4", []string{"x"}, "1m", "")
	if err != nil {
		t.Fatalf("успешное продление не должно давать ошибку: %v", err)
	}
	if data["orderId"] != "68b1f0c4e13a4c0f1a2b3c4d" {
		t.Fatalf("orderId потерян: %#v", data)
	}

	// у prolong/calc тот же конверт — ЛЕГИТИМНЫЙ warning, ошибки быть не должно
	client4, _ := newTestClient(t, envelopeHandler(t, insufficient, nil))
	if _, err = client4.CalculateProlong("ipv4", ProlongRequest{IDs: []string{"x"}, PeriodID: "1m"}); err != nil {
		t.Fatalf("prolong/calc warning не должен становиться ошибкой: %v", err)
	}
}

// TestSplitProlongTargets — клиент продлевает по адресам, которые видит в proxy/list, а не по
// ObjectId. Каждое значение маршрутизируется по форме, так что смешанный список тоже работает.
func TestSplitProlongTargets(t *testing.T) {
	cases := []struct {
		name    string
		input   interface{}
		wantIPs []string
		wantIDs []string
	}{
		{"ipv4", []string{"1.2.3.4", "5.6.7.8"}, []string{"1.2.3.4", "5.6.7.8"}, nil},
		// Любая строка с двоеточием уходит в ips — и адрес ipv6 (в поле "ip" уже шлюз с портом,
		// "1.2.3.4:26000"), и mobile-тройка.
		{"colon address", []string{"2001:db8::1:8080"}, []string{"2001:db8::1:8080"}, nil},
		{"mobile triple", []string{"10.0.0.1:8000:9000"}, []string{"10.0.0.1:8000:9000"}, nil},
		{"objectids", []string{"68b1f0c4e13a4c0f1a2b3c4d"}, nil, []string{"68b1f0c4e13a4c0f1a2b3c4d"}},
		{"mixed", []string{"1.2.3.4", "68b1f0c4e13a4c0f1a2b3c4d"}, []string{"1.2.3.4"}, []string{"68b1f0c4e13a4c0f1a2b3c4d"}},
		{"comma string", "1.2.3.4, 5.6.7.8", []string{"1.2.3.4", "5.6.7.8"}, nil},
		{"blanks dropped", []string{"1.2.3.4", "   ", ""}, []string{"1.2.3.4"}, nil},
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

// TestPrepareLegacyProlongSendsIPs — адреса уезжают в ips, а поле ids при этом не появляется
// пустым: сервер отдаёт приоритет ids, и пустой список молча отменил бы продление по адресам.
func TestPrepareLegacyProlongSendsIPs(t *testing.T) {
	c := NewClient("test-key")
	data := c.prepareLegacyProlong([]string{"1.2.3.4"}, "1m", "")
	if _, present := data["ids"]; present {
		t.Fatalf("ids must be absent when prolonging by address, got %#v", data)
	}
	ips, ok := data["ips"].([]string)
	if !ok || len(ips) != 1 || ips[0] != "1.2.3.4" {
		t.Fatalf("ips = %#v, want [1.2.3.4]", data["ips"])
	}
}

// TestOrderMixSendsMixId — идентификатор MIX-пакета должен уходить в mixId: parseMixSelection
// ищет пакет через findById(mixId), а countryId для него — не то поле.
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
