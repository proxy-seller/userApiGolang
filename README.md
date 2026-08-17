# Proxy-Seller Client API SDK for Go

This package is a small client for Client API v2. It builds requests, adds the API key, parses the common `{status,data,errors}` envelope and keeps binary downloads as bytes.

Base URL: `https://proxy-seller.com/personal/api/v2/` — the API key is a **path segment**, not a header.

## Install

```sh
go get github.com/proxy-seller/userApiGolang/v2
```

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
        api.WithBaseURL("http://127.0.0.1:7995/personal/api/v2/"),
        api.WithTimeout(15*time.Second),
    )
    client.SetPaymentCode("balance") // stable across prod/dev

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

`WithHTTPClient` accepts an injected `*http.Client` (custom transport, TLS, tracing, local stubs). `WithTimeout` defaults to 30 seconds. `baseURL` must include `/personal/api/v2/`.

Every endpoint is available as a method on `*Client`. The old package-level API remains available and delegates to `DefaultClient`:

```go
api.SetApiKey("YOUR_API_KEY")
api.SetPaymentId("PAYMENT_SYSTEM_ID")
fmt.Println(api.Balance())
```

`URL` is retained for old callers, but new code should use `WithBaseURL`.

## All ids in v2 are strings

v1 exposed numeric ids. In v2 almost every id is a **MongoDB ObjectId string** — `orderId`, `paymentId`, `countryId`, `periodId`, IP address ids, auth ids, `basket_id`, `order_id`. Never parse them into `int`/`int64`, never format them with `%d`, and do not assume they are sortable or sequential.

```go
made, err := client.MakeOrder(api.OrderRequest{
    SectionCode: "ipv4", CountryCode: "USA", PeriodCode: "1m", Quantity: 1,
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
    // SubscriptionID / DailyCountCap / MonthlyAmountCap left untouched
})

if limits, ok := api.AutoTopupLimitsFromError(err); ok {
    fmt.Println(limits.MinAmount, limits.MinThreshold, limits.MinDailyCountCap)
}
```

`set` returns the state **after** saving, so no second request is needed. Auto top-up error codes: `49` feature disabled, `50` threshold too low, `51` amount too low, `52` amount below threshold, `53` no saved payment method, `54` daily cap too low, `55` monthly cap below a single top-up, `56` saved card invalid.

## Current order API

Use `OrderRequest` for stable codes and all current fields:

```go
calc, err := client.CalculateOrder(api.OrderRequest{
    SectionCode: "mobile",
    CountryCode: "USA",
    PeriodCode: "1m",
    Quantity: 1,
    MobileServiceType: "dedicated", // shared or dedicated; required for mobile
    OperatorCode: "operator-code",
    RotationCode: "5m",
})

mix, err := client.CalculateOrder(api.OrderRequest{
    SectionCode: "mix",
    MixCode: "mix-code",
    PeriodCode: "1m",
    Quantity: 10,
})

uptime, err := client.CalculateOrder(api.OrderRequest{
    SectionCode: "ipv4",
    CountryCode: "USA",
    PeriodCode: "1m",
    Quantity: 1,
    Uptime: true,
})
```

`customTargetName` is required for `ipv4`, `ipv6`, `isp` and for `mix`/`mix_isp` that the server cannot resolve to a MIX package (otherwise it falls back to ipv4 and answers `"Incorrect goal"`, code 14). The SDK checks this locally and mirrors `ClientApiService.parseMixSelection`: a MIX is considered resolved by `mixId`/`mixCode`, by `countryId` in `"packageId:quantity"` form, or by `countryId` together with `quantity > 0`.

The legacy `OrderCalcMobile`/`OrderMakeMobile` helpers send `mobileServiceType=dedicated`; `OrderCalcMobile` no longer builds an IPv6 request. For code-first, MIX, uptime, scraper/shared, or other new combinations, use `OrderRequest`.

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

## Prolong, proxy and resident options

- `CalculateProlong` / `MakeProlong` accept `ProlongRequest` with proxy IDs, MIX separator IDs, `periodCode`, and `paymentCode`.
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
| `errors[].code` read as a number from an `interface{}` field | typed `APIErrorCode`, use `CodeInt()` |

## Changelog

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

To exercise a local `client-api-service`, run it on port 7995 and construct the client with `WithBaseURL("http://127.0.0.1:7995/personal/api/v2/")`. Use a development API key and call read-only endpoints first (`Balance`, `AuthList`, `ResidentLists`).
