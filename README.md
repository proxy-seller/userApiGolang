# Proxy-Seller Client API SDK for Go

This package is a small client for Client API v2. It builds requests, adds the API key, parses the common `{status,data,errors}` envelope and keeps binary downloads as bytes.

Base URL: `https://proxy-seller.com/personal/api/v2/` — the API key is a **path segment**, not a header.

## Install

```sh
go get github.com/proxy-seller/userApiGolang/v2
```

> ⚠️ **This does not resolve yet.** The module path declares `/v2`, but the repository carries no
> tags at all and the default branch (`main`) still declares the path without `/v2`, so the proxy
> answers `no matching versions`. Until a `v2.x.x` tag is pushed on a branch whose `go.mod` says
> `/v2`, depend on the branch explicitly — **keeping `/v2` in the path**:
>
> ```sh
> go get github.com/proxy-seller/userApiGolang/v2@feature/client-api-v2
> ```
>
> Dropping the `/v2` fails: the bare path and this branch's `go.mod` disagree, and the proxy
> refuses with `go.mod has post-v0 module path ".../v2" at revision …`. With `/v2` it resolves
> to the pseudo-version `v2.0.0-20260827082345-0949d38aa304`.

## Client configuration

Prefer an explicit `Client`. Each instance has its own key, payment settings, base URL and HTTP client.

```go
package main

import (
    "errors"
    "fmt"
    "time"

    api "github.com/proxy-seller/userApiGolang/v2"
)

func main() {
    client := api.NewClient(
        "YOUR_API_KEY",
        api.WithTimeout(15*time.Second),
    )

    balance, err := client.Balance()
    if err != nil {
        var apiErr *api.APIError
        if errors.As(err, &apiErr) {
            fmt.Println(apiErr.HTTPStatus, apiErr.Messages(), apiErr.CodeInt())
        }
        return
    }
    fmt.Println(balance)
}
```

Nothing else is required — the client talks to `https://proxy-seller.com/personal/api/v2/` by default. `WithTimeout` defaults to 30 seconds.

### Paying for orders

Every order and renewal needs a payment system. Take one from `BalancePaymentsList` and set it once:

```go
payments, _ := client.BalancePaymentsList()   // [{id: "69e7…", name: "PayPal"}, …]
client.SetPaymentID(payments[0]["id"].(string))
```

This is the one place where an id is unavoidable: several payment systems share the same internal code (a single `cryptomus` covers "USDT (TRC-20)", "All cryptocurrencies" and more), so the code cannot tell them apart. Everywhere else you use human-readable codes.

### Residential and scraper orders need a fingerprint

`order/make` carries an `X-Fingerprint` header. Most sections ignore it, but **residential and scraper orders are not created without it at all** — the order service answers `Header X-Fingerprint is required` and nothing is ordered.

```go
client := api.NewClient("YOUR_API_KEY", api.WithFingerprint("my-installation-id"))
// or later:
client.SetFingerprint("my-installation-id")
```

Any opaque string is accepted — the server does not validate its shape — but it must be a **stable identifier of your installation**. The SDK deliberately does not generate one: a value randomized per process would break the anti-fraud and affiliate attribution the header exists for.

Ordering resident or scraper without a fingerprint fails locally with an explanatory error, rather than spending a round trip on a request the server is certain to reject.

<details>
<summary>Pointing the client at another host (local testing)</summary>

```go
client := api.NewClient("YOUR_API_KEY", api.WithBaseURL("http://localhost:7995/personal/api/v2/"))
```

`WithHTTPClient` accepts an injected `*http.Client` (custom transport, TLS, tracing, local stubs). The base URL must include `/personal/api/v2/`.

</details>

Every endpoint is available as a method on `*Client`. The old package-level API remains available and delegates to `DefaultClient`:

```go
api.SetApiKey("YOUR_API_KEY")
api.SetPaymentId("PAYMENT_SYSTEM_ID")
fmt.Println(api.Balance())
```

`URL` is retained for old callers, but new code should use `WithBaseURL`.

## All ids in v2 are strings

v1 exposed numeric ids. In v2 almost every id is a **MongoDB ObjectId string** — `orderId`, `paymentId`, `countryId`, `periodId`, IP address ids, auth ids, `basket_id`, `order_id`. Never parse them into `int`/`int64`, never format them with `%d`, and do not assume they are sortable or sequential. On the request side the reference `*Id` fields also accept the stable code in place of the id (see [Current order API](#current-order-api)), which is one more reason to treat them as opaque strings.

```go
made, err := client.MakeOrder(api.OrderRequest{
    // *Id fields take an ObjectId or the stable code — both are opaque strings
    SectionCode: "ipv4", CountryID: "USA", PeriodID: "1m", Quantity: 1,
    CustomTargetName: "seo",
})
// made["orderId"] == "68b1f0c4e13a4c0f1a2b3c4d" — a string, not 1000000
```

Two documented exceptions:

| Id | Type | Server field |
|---|---|---|
| resident list id (`resident/lists`, `resident/list/rename`, `resident/list/delete`, `resident/list/rotation`) | numeric — `int64` in this SDK | `ListItemResponseDto.id`, `ListRenameRequestDto.id`, `ListDeleteRequestDto.id`, `ListRotationRequestDto.id` are `Long` |
| subpackage list id on `residentsubuser/list/delete` | **string** | `DeleteSubPackageListRequestDto.id` is `String` with `@NotBlank` |

`residentsubuser/list/rename` and `residentsubuser/list/rotation` take an `Integer` id, so those helpers keep `int`.

## Error handling

The envelope is `{status, data, errors}` and the HTTP status is **almost always 200** — failures live inside `errors[]`. Non-2xx statuses exist only for the plain-text `ext` rejection described below.

```go
_, err := client.ProxyReplace(ids, api.ProxyReplaceReasonCustom, "manual reason")
var apiErr *api.APIError
if errors.As(err, &apiErr) {
    for _, item := range apiErr.Errors {   // read the WHOLE slice, not just [0]
        fmt.Println(item.CodeInt(), item.Message, item.CustomData)
    }
    if apiErr.HasCode(503) { /* ... */ }
    if apiErr.IsAccessError() { /* key / IP / rate limit */ }
}
```

- `APIErrorItem.Code` is typed (`APIErrorCode`), so `item.CodeInt() == 503` works. Before, `Code` was `interface{}` and `encoding/json` produced `float64`, which made every comparison against an int literal silently false.
- **Access errors always arrive as the same triple.** A broken key, an IP outside the allowlist and an exceeded rate limit all return HTTP 200 with three errors, all `code: 503`:
  `"Error api key"`, `"IP not allowed <ip>"`, `"Request limit reached"`.
  `errors[0].message` is therefore always `"Error api key"` — it does **not** identify the actual cause. Inspect the whole slice, and do not expect HTTP 429: it does not exist in v2. The rate limit itself is 1000 requests per calendar minute per key.
- A response with useful `data` and an empty `errors` array is returned as success with `ResultData.Status == "error"` (server-side warnings, e.g. `order/calc` with insufficient funds).
- Validation limits are carried in `errors[].customData`; `AutoTopupLimitsFromError` unpacks the auto top-up ones.

## Balance and auto top-up

`balance/add` accepts **only `paymentId`** (an id from `balance/payments/list`). Unlike `order/*` and `prolong/*`, it does not resolve `paymentCode` — the SDK now fails locally with an explanatory error instead of sending an empty `paymentId`.

```go
url, err := client.AddBalance(25, "68b1f0c4e13a4c0f1a2b3c4d")
```

`balance/autotopup/get` and `balance/autotopup/set` manage automatic replenishment. `set` is a **partial update**: only the fields you provide are changed, everything else keeps its stored value. That is why the request struct uses pointers — an omitted field is not serialized at all, so it can never arrive as `null`.

```go
state, err := client.GetAutoTopup()
// state.State: NO_PAYMENT_METHOD | DISABLED | ACTIVE | PAYMENT_INVALID | PAUSED_FAILURES

state, err = client.SetAutoTopup(api.AutoTopupSetRequest{
    Enabled:   api.Bool(true),
    Threshold: api.Float64(5),
    Amount:    api.Float64(25),
    // SubscriptionID left untouched
})

if limits, ok := api.AutoTopupLimitsFromError(err); ok {
    fmt.Println(limits.MinAmount, limits.MinThreshold)
}
```

`set` returns the state **after** saving, so no second request is needed. Auto top-up error codes: `49` feature disabled, `50` threshold too low, `51` amount too low, `52` amount below threshold, `53` no saved payment method, `56` saved card invalid.

> **`DailyCountCap` and `MonthlyAmountCap` are gone.** Removed from the contract on 2026-08-18: the server silently ignores them and they are absent from the response, so sending them made a call that reported success and changed nothing. Error codes `54` and `55` went with them and are not reused, and `customData` no longer carries `minDailyCountCap`.

## Current order API

Every `*Id` field of an order takes **the readable code** — `USA`, `1m`, `europe-2-mix_IPv4`. If the value is not a known ObjectId and the paired `*Code` field is empty, the server resolves it as a code (`ClientApiService.normalizeOrderReferenceCodes`). This covers `countryId`, `periodId`, `operatorId`, `mixId`, `tarifId` and `paymentId`, so an order needs **no `*Code` field at all** — put the code straight into the id:

```go
calc, err := client.CalculateOrder(api.OrderRequest{
    SectionCode: "mobile",
    CountryID: "USA",  // country[].id from the reference, uppercased by the server
    PeriodID: "1m",    // period code, lowercased by the server
    Quantity: 1,
    MobileServiceType: "dedicated", // shared or dedicated; required for mobile
    OperatorID: "68b1f0c4e13a4c0f1a2b3c4d", // operator id, or its tag
    RotationID: "5",   // MINUTES, "0" = By Link — not a code
})

mix, err := client.CalculateOrder(api.OrderRequest{
    SectionCode: "mix",
    MixID: "68b1f0c4e13a4c0f1a2b3c9a", // mix package id, or its tag
    PeriodID: "1m",
    Quantity: 10,
})

uptime, err := client.CalculateOrder(api.OrderRequest{
    SectionCode: "ipv4",
    CountryID: "USA",
    PeriodID: "1m",
    Quantity: 1,
    Uptime: true,
    CustomTargetName: "seo", // required for ipv4
})
```

The `*Code` fields (`CountryCode`, `PeriodCode`, `OperatorCode`, `MixCode`, `TarifCode`, `PaymentCode`) still work and are handy when you want to be explicit; `prepareOrder` clears the id of a pair when its code is set. Filling both is pointless.

`RotationID` is the one field that is neither an id nor a code: it is the **rotation interval in minutes** as a numeric string — `"5"`, `"10"`, `"0"` for By Link. `RotationCode` is redundant, because the server only checks `isInteger()` and copies the value into `rotationId` without any reference lookup — a non-numeric value is answered with `"Set existed [rotationCode] from reference"`, and a non-numeric `rotationId` (`"5m"`) dies in `requestDto.rotationId as int` and comes back as `"Unknown error"`, code 35.

### What you can pass, and where to get it

Every field in the reference is called `id`, and its value is a readable code — not an ObjectId. Read `id`, put it in the matching `*Id` argument. That is the whole rule:

| Request field | Pass this | Read it from |
|---|---|---|
| `countryId` | alpha-3 country code, e.g. `USA` (upper-cased server-side, so `usa` works) | `reference/list` → `country[].id` |
| `periodId` | period code, e.g. `1m` (lower-cased server-side) | `reference/list` → `period[].id` |
| `operatorId` | mobile operator code — exact match, case-sensitive | `reference/list/mobile` → `country[].operators.dedicated[]` / `.shared[]` → `id` |
| `rotationId` | **minutes as an integer**, `0` = By Link — the one `id` that is a number, not a code | `reference/list/mobile` → `country[].operators.*[].rotations[].id` — that value *is* the minute count (`name` is `"5 minutes"` / `"By Link"`) |
| `mixId` | mix package code — exact match | `reference/list/mix` → `quantities[].id`, e.g. `europe-2-mix_IPv4`. `OrderCalcMix`/`OrderMakeMix` take it as the first argument |
| `tarifId` | resident tariff code — exact match, e.g. `1-gb` | `reference/list/resident` → `tarifs[].id` |
| `paymentId` | payment-system ObjectId — the one unavoidable id | `balance/payments/list` → `id` (see [Paying for orders](#paying-for-orders)) |

ObjectIds are still accepted everywhere if you happen to have them; the reference simply no longer publishes them. Code resolution happens in `order/calc`, `order/make`, `prolong/calc` and `prolong/make`.

The paired `*Code` fields (`CountryCode`, `PeriodCode`, `OperatorCode`, `MixCode`, `TarifCode`, `PaymentCode`) are still accepted and do the same job explicitly. You do not need them: a code in the `*Id` field resolves the same way. `RotationCode` is the exception — the server only copies it into `rotationId` and rejects a non-integer, so set `RotationID` and ignore it.

`customTargetName` — what you use the proxies for. Required for `ipv4`, `ipv6` and `isp`. For `mix`/`mix_isp` it is only needed when the server cannot tell which MIX package you mean; naming the package (`MixID`, or the first argument of `OrderCalcMix`) removes the need for it. Order a mix without either and the server answers `"Incorrect goal"`, code 14.

The legacy positional helpers take the same values, so codes go in without any placeholder chain — only `authorization` and `coupon` are genuinely optional:

```go
calc, err := client.OrderCalcMobile("USA", "1m", 1, "", "", "68b1f0c4e13a4c0f1a2b3c4d", "5")
ipv4, err := client.OrderCalcIpv4("USA", "1m", 1, "", "", "seo")
```

`OrderCalcMobile`/`OrderMakeMobile` always send `mobileServiceType=dedicated`, and `OrderCalcMobile` no longer builds an IPv6 request. For shared mobile, MIX, uptime, scraper or other new combinations, use `OrderRequest`.

## proxy/replace takes a reason, not a proxy type

The `type` field of `proxy/replace` is the **replacement reason** (`ProxyReplaceType` on the server), not the proxy type — the proxy type is derived from the first id in `ids`. Allowed values, exported as constants:

`NOT_WORK` · `INCORRECT_LOCATION` · `CANT_CHANGE_NETWORK` · `LOW_SPEED` · `CUSTOM`

`CUSTOM` requires a non-empty `comment` (the server answers `"Set comment"`, code 503 otherwise). Both rules are enforced locally before the request leaves.

```go
_, err := client.ProxyReplace(
    []string{"68b1f0c4e13a4c0f1a2b3c4d"},
    api.ProxyReplaceReasonLowSpeed,
    "",
)
```

## Downloads return files, not envelopes

`proxy/download/*`, `resident/geo` and `resident/geo/isp` answer with `Content-Disposition: attachment`.

- `resident/geo` is a **JSON file** (`geo.json`) — not a zip archive, despite what older SDK docs claimed. `resident/geo/isp` is `isp.json`.
- `ext` must be at most 250 characters and must not contain CR, LF, `/` or `\`. Violations get a **bare HTTP 400 with a plain-text body**, outside the envelope. `assertExt` catches this locally.
- `package_key` is honored **only** by `proxy/download/subresident`. The literal `proxy/download/resident` route ignores it and exports the parent package, so `DownloadProxies` refuses that combination locally.

```go
file, err := client.DownloadProxies("subresident", api.ProxyDownloadOptions{
    Ext: "txt", ListID: "561", PackageKey: "f9ea7063d7a699b12b3e",
})
```

The legacy download helpers return a bare `string`/`[]byte` and swallow errors. Use the `*E` variants when you need the reason: `ProxyDownloadE`, `ProxyDownloadResidentE`, `ResidentGeoE`, `ResidentGeoIspE` (or the `*Client` methods, which all return an error).

## Renewing proxies

Renew by the addresses themselves — the same strings `ListProxies` gives you. No ids to look up:

```go
list, _ := client.ListProxies("ipv4", api.ProxyListOptions{})
items := list["items"].([]interface{})

ips := make([]string, 0, len(items))
for _, item := range items {
    ips = append(ips, item.(map[string]interface{})["ip"].(string))
}

quote, _ := client.ProlongCalc("ipv4", ips, "1m", "")   // price first
order, err := client.ProlongMake("ipv4", ips, "1m", "") // deducts money
```

`ProlongCalc` shows the price; `ProlongMake` charges the balance. If the balance is short, `ProlongMake` returns an `*APIError` with the server's warning — it never reports a renewal that did not happen.

What you pass follows the proxy type, and every field below is one `ListProxies` publishes:

| type | pass | built from | example |
|---|---|---|---|
| `ipv4`, `isp`, `mix`, `mix_isp` | the address | `ip` | `1.2.3.4` |
| `ipv6` | the address | `ip` | `1.2.3.4:26000` |
| `mobile` | the address | `ip` + `:` + `port_http` + `:` + `port_socks` | `10.0.0.1:50100:50101` |

For `ipv6` the `ip` field already contains the gateway together with the port (`1.2.3.4:26000`),
while `ip_only` holds the gateway alone — so pass `ip` as it comes, exactly like every other type.

ObjectId strings work for every type, and a mixed slice works too — each value is routed by its shape.

## Automatic renewal

`ProlongMake` charges you now. `autoprolong/*` only arms a charge that happens later, without you present — a separate branch of the API, not a flag on prolong.

```go
req := api.AutoProlongRequest{}
req.IPs = []string{"1.2.3.4"}
req.PeriodID = "1m"
req.PaymentID = "balance"                       // mandatory here

client.CalculateAutoProlong("ipv4", req)        // what will be charged, and when
client.EnableAutoProlong("ipv4", req)           // arm it
client.DisableAutoProlong("ipv4", req)          // disarm it
```

`PaymentID` is **mandatory** for calc and enable — the charge happens while you are away, so the payment system cannot be guessed. Only `balance` and `paddle_subscription` are accepted: a one-off Paddle checkout needs a browser redirect a headless client cannot complete. With `paddle_subscription` also set `SubscriptionID`.

Residential packages renew as a package, not as addresses — send no selection, only the payment system and optionally `TarifID` to confirm the tariff already on the package:

```go
client.EnableAutoProlong("resident", api.AutoProlongRequest{
    ProlongRequest: api.ProlongRequest{PaymentID: "balance"},
    TarifID:        "trial",
})
```

Three things about the answers before you parse them:

* **`ids` is not an echo.** For `ipv6` the whole order is switched at once, so `quantity` and `ids` can cover more proxies than you sent.
* **Not enough money is not an error return.** `calc` answers `status: "error"` with a *filled* `data` and an empty `errors[]` — the same shape `prolong/calc` uses. Read `warning`.
* **Residential fills different fields.** `days` and `chargeDate` are null there (a package renews on expiry *or* on traffic exhaustion, so no single date describes it); `tarifId` and `dateEnd` carry the meaning instead.

`scraper` has no auto-renewal: it is extended by buying traffic through `order/make`.

> Replaces `resident/autorenew/{enable,disable,calculate}`, **removed** from the server.

## Prolong, proxy and resident options

- `CalculateProlong` / `MakeProlong` are the struct-based form of the same two calls: `ProlongRequest` carries `IPs` or `IDs`, `PeriodID`/`PeriodCode` and `PaymentID`/`PaymentCode`. `normalizeProlongReferenceCodes` has the same id-or-code fallback as orders, so `PeriodID: "1m"` is enough.
- `ListProxies` accepts all current filters through `ProxyListOptions`.
- `CreateResidentList` and `CreateResidentSubuserList` accept geo, export and rotation options.
- `CreateResidentSubuser` / `UpdateResidentSubuser` include string traffic limits, expiration, rotation, active and link-date fields.
- `resident/lists` returns `data` as a **flat array** (no `items` wrapper), matching v1.
- `resident/traffic/details` names the package key `packageKey` **or** `key` — not `package_key` — and it is required.
- Delete endpoints (`resident/list/delete`, `residentsubuser/delete`, `residentsubuser/list/delete`) return `data` as a **string**, sometimes with JSON inside (`{"status":"not-found"}` under `status: "success"`). The helpers normalize it to a map so a failed delete is distinguishable from a successful one.
- Subpackage `expired_at` is an **object** (PHP date: `{date, timezone_type, timezone}`), while the parent package `expired_at` is a formatted string `"31.12.2025 23:59:59"`. Traffic values are strings in both.

Raw scalar `data` is available in `ResultData.Value`; object and array values remain available in `Map` and `Slice`; the untouched JSON is in `ResultData.Raw`.

## Migrating from v1

| v1 | v2 |
|---|---|
| numeric ids (`orderId => 1000000`) | ObjectId strings (`orderId => "68b1f0c4e13a4c0f1a2b3c4d"`); resident list ids stay numeric |
| `targetId` + `targetSectionId` | `customTargetName` only |
| `resident/lists` wrapped in `items` | flat array in `data` |
| HTTP 429 on rate limit | HTTP 200 with the three-error access triple, `code: 503` |
| `type` of `proxy/replace` looked like a proxy type | it is the replacement reason enum |
| `package_key` on `proxy/download/resident` | only on `proxy/download/subresident` |
| numeric `paymentId` | ObjectId string from `balance/payments/list`; `paymentCode` works for orders and prolong but **not** for `balance/add` |
| numeric proxy ids for renewal | pass the addresses instead — `ProlongCalc`/`ProlongMake` accept what `ListProxies` returns |
| `errors[].code` read as a number from an `interface{}` field | typed `APIErrorCode`, use `CodeInt()` |

## Changelog

### v2.0.1 — catching up with the server

- Added `CalculateAutoProlong` / `EnableAutoProlong` / `DisableAutoProlong` (and their package-level counterparts) for `autoprolong/{calc,enable,disable}/{type}`, with `AutoProlongRequest`. `PaymentID` is required on calc and enable and restricted to `balance` / `paddle_subscription`; `scraper` is rejected locally.
- The server **removed** `resident/autorenew/{enable,disable,calculate}` — `type: "resident"` on the three endpoints above replaces them.
- Added `X-Fingerprint` on `order/make`: `WithFingerprint(...)` / `SetFingerprint(...)`. Residential and scraper orders now fail locally when it is unset instead of being rejected by the server.
- Removed `DailyCountCap` / `MonthlyAmountCap` from auto top-up (dropped from the contract 2026-08-18 and silently ignored), along with error codes `54`/`55` and `MinDailyCountCap`.
- `MaxLine` is now sent on `proxy/download/resident` — the only route that accepts it. `Proto`, `Country` and `Ends` are no longer sent there, since that literal route ignores them.
- `*Code` no longer overrides a paired `*Id` for `MixID`, `OperatorID`, `RotationID` and `TarifID`: the server applies the code on those four only while the id is empty, so the SDK was inverting the contract.
- Fixed two README examples that did not compile (`SetPaymentId` → `SetPaymentID`, `ListProxies` takes `ProxyListOptions`).
- Test suite repaired and extended. `go test ./...` had stopped compiling: `TestAutoTopupLimitsFromError` still referenced `MinDailyCountCap`, removed with the caps above. `TestProlongMakeInsufficientFundsIsAnError` still asserted the old insufficient-funds shape (`status:"error"` with an empty `errors[]`); the server now sends `errors[{code:16}]`, and the test covers that plus the case the removed guard used to break — a legitimate success with an empty `orderId` must keep `total` and `listBaseOrderNumbers`. Added `TestGenerateAuthPrecedence`: `OrderRequest.GenerateAuth` outranks `SetGenerateAuth()`, and neither is sent on `order/calc`, which drops the field.

### v2 (current)

- Added `balance/autotopup/get` and `balance/autotopup/set` with partial-update semantics (`AutoTopupSetRequest`, `AutoTopupState`, `AutoTopupLimitsFromError`).
- `APIErrorItem.Code` is now the typed `APIErrorCode` instead of `interface{}`; added `CodeInt`, `HasCode`, `Messages`, `FirstCustomData`, `IsAccessError`.
- Every endpoint that previously existed only as a package-level function now has a `*Client` method, so clients built with `NewClient` no longer fail with `client api key is required`. Package-level functions still work through `DefaultClient`.
- Added error-returning download variants: `ProxyDownloadE`, `ProxyDownloadResidentE`, `ResidentGeoE`, `ResidentGeoIspE`.
- Added local `ext` validation (`assertExt`) and a guard against `package_key` outside `proxy/download/subresident`.
- `proxy/replace` validates the replacement-reason enum and requires a comment for `CUSTOM`.
- `balance/add` fails locally when only `paymentCode` is set.
- List id types now match the server: `int64` for `resident/list/*`, `string` for `residentsubuser/list/delete`.
- Removed the `[]int` branch of `normalizeIDs` (all `ids` fields are `Set<String>` of ObjectIds on the server).

Breaking changes in this release: `APIErrorItem.Code` type, `ResidentListRename`/`ResidentListDelete`/`ResidentListRotation` id parameters, `ResidentsubuserListDelete` id parameter.

## Local verification

```powershell
gofmt -l .
go build ./...
go vet ./...
go test ./...
```

To exercise a `client-api-service` you run yourself, point the client at it with `WithBaseURL` and call read-only endpoints first (`Balance`, `AuthList`, `ResidentLists`).
