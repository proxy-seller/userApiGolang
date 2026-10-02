# Proxy-Seller Client API SDK for Go

This package is a small client for Client API v2. It builds requests, adds the API key, parses the common `{status,data,errors}` envelope and keeps binary downloads as bytes.

Base URL: `https://proxy-seller.com/personal/api/v2/` — the API key is a **path segment**, not a header.

## Install

```sh
go get github.com/proxy-seller/userApiGolang/v2@latest
```

```go
import api "github.com/proxy-seller/userApiGolang/v2"
```

**Keep `/v2` in the path.** The bare `github.com/proxy-seller/userApiGolang` is the v1 module,
the client for `/personal/api/v1/`, and none of this document applies to it.

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

Nothing else is required — the client talks to `https://proxy-seller.com/personal/api/v2/` by default. `WithTimeout` defaults to 30 seconds; money calls (`order/make`, `prolong/make/{type}`, `balance/add`) get 120 seconds — see [Timeouts and retries on payments](#timeouts-and-retries-on-payments). Requests are paced by default — see [Rate limits and the request queue](#rate-limits-and-the-request-queue).

### Paying for orders

Orders and renewals are paid **from the account balance or with the saved card** — nothing else. Set the payment code once:

```go
client.SetPaymentCode("balance")                // pay from the balance
// client.SetPaymentCode("paddle_subscription") // or with the saved card (needs an active card subscription)
```

`order/make` requires a payment system and accepts only these two: any other value is answered with `Set paymentId = <id>(inner balance) OR paymentId = <id>(subscribed card)`, and paying with the saved card without an active card subscription is refused. `order/calc` quotes without one, `prolong/*` falls back to the balance when none is set, and `autoprolong/*` requires it (see [Automatic renewal](#automatic-renewal)). `PaymentCode` / `PaymentID` on `OrderRequest`, `ProlongRequest` or `AutoProlongRequest` override the client's value for a single call.

`BalancePaymentsList` is **not** a list of ways to pay for orders: it lists the payment systems for topping up the balance with `AddBalance`, and it never contains the balance itself. Its `id` is the one place where an id is unavoidable — several payment systems share the same code (a single `cryptomus` covers "USDT (TRC-20)", "All cryptocurrencies" and more), so the code cannot tell them apart. Everywhere else you use human-readable codes.

### Optional installation fingerprint

`order/make` accepts an optional `X-Fingerprint` header — a stable, opaque identifier of your installation. When it is present the server uses it for anti-fraud checks and affiliate attribution; **no section refuses an order without it**.

```go
client := api.NewClient("YOUR_API_KEY", api.WithFingerprint("my-installation-id"))
// or later:
client.SetFingerprint("my-installation-id")
```

Any string is accepted — the server does not validate its shape — but keep it **stable** for your installation. The SDK sends the header on `order/make` whenever a value is set (`OrderRequest.Fingerprint` for a single call, otherwise the client's value) and never requires it: without a value no header is sent and the order goes through as usual. The SDK deliberately does not generate one — a value randomized per process would defeat the anti-fraud and attribution checks the header exists for.

<details>
<summary>Pointing the client at another host (local testing)</summary>

```go
client := api.NewClient("YOUR_API_KEY", api.WithBaseURL("http://localhost:7995/personal/api/v2/"))
```

`WithHTTPClient` accepts an injected `*http.Client` (custom transport, TLS, tracing, local stubs). The SDK never modifies it — money calls go through a copy with a longer timeout, see [Timeouts and retries on payments](#timeouts-and-retries-on-payments). The base URL must include `/personal/api/v2/`.

</details>

Every endpoint is available as a method on `*Client`. The old package-level API remains available and delegates to `DefaultClient`:

```go
api.SetApiKey("YOUR_API_KEY")
api.SetPaymentCode("balance") // orders and renewals: "balance" or "paddle_subscription"
balance, err := api.BalanceE()
```

`Balance()` and `BalanceAdd()` keep their v1 signatures and are deprecated: they have no error result, so a failed call comes back as `-1` / `""`. Use `BalanceE()` / `BalanceAddE()` or the `*Client` methods.

> ⚠️ **The package-level API keeps one state for the whole process.** `SetApiKey`, `SetPaymentId`, `SetPaymentCode`, `SetGenerateAuth` and `SetFingerprint` are deprecated: they change `DefaultClient`, which every goroutine and every package of the program shares. Two callers with different keys or payment codes overwrite each other, and an order goes out with the other caller's key or is paid the other caller's way. Use `NewClient` — one client per key — and pass per-call values (`OrderRequest.PaymentCode`, `ProlongRequest.PaymentCode`, `OrderRequest.Fingerprint`, `OrderRequest.GenerateAuth`) instead of changing a shared client between calls: its setters have the same effect on everyone who uses it.

`URL` is retained for old callers and deprecated as well — new code should use `WithBaseURL`. While `URL` differs from `DefaultBaseURL`, it overrides the base URL of `DefaultClient` for the package-level functions; at its default value it leaves `DefaultClient` alone, so a replacement such as `api.DefaultClient = api.NewClient(key, api.WithBaseURL("http://localhost:7995/personal/api/v2/"))` is honored. Set `URL` back to `DefaultBaseURL` and `DefaultClient` gets its own base URL back.

## All ids in v2 are strings

v1 exposed numeric ids. In v2 almost every id is an **ObjectId string (24 hex characters)** — `orderId`, `paymentId`, `countryId`, `periodId`, IP address ids, auth ids, `basket_id`, `order_id`. Never parse them into `int`/`int64`, never format them with `%d`, and do not assume they are sortable or sequential. On the request side the reference `*Id` fields also accept the stable code in place of the id (see [Current order API](#current-order-api)), which is one more reason to treat them as opaque strings.

```go
made, err := client.MakeOrder(api.OrderRequest{
    // *Id fields take an ObjectId or the stable code — both are opaque strings
    SectionCode: "ipv4", CountryID: "USA", PeriodID: "1m", Quantity: 1,
    CustomTargetName: "seo",
})
// made["orderId"] == "68b1f0c4e13a4c0f1a2b3c4d" — a string, not 1000000
```

Two documented exceptions:

| Id | Type in this SDK | On the wire |
|---|---|---|
| resident list id (`resident/lists`, `resident/list/rename`, `resident/list/delete`, `resident/list/rotation`) | numeric — `int64` | a JSON number (64-bit integer), both in the `resident/lists` response and in the request bodies |
| subpackage list id on `residentsubuser/list/delete` | **string** | a non-empty JSON string |

`residentsubuser/list/rename` and `residentsubuser/list/rotation` take a 32-bit integer id, so those helpers keep `int`.

## Error handling

The envelope is `{status, data, errors}` and the HTTP status is **almost always 200** — failures live inside `errors[]`. The API itself answers non-2xx only with the plain-text `ext` rejection described below. What sits in front of it can answer otherwise: the edge rate limit answers HTTP 429, which the client retries by itself (see [Rate limits and the request queue](#rate-limits-and-the-request-queue)), and a gateway or the front-end can answer HTTP 5xx or 404 with an HTML page. Such replies come back as an `*APIError` with `HTTPStatus` and the start of the page in `Body`; on a money call they leave the outcome unknown — see [Timeouts and retries on payments](#timeouts-and-retries-on-payments).

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
  `errors[0].message` is therefore always `"Error api key"` — it does **not** identify the actual cause. Inspect the whole slice. The API itself never answers HTTP 429: going over its limit of 1000 requests per calendar minute per key produces this triple too. The client paces its requests to stay under that limit and never retries the triple (see [Rate limits and the request queue](#rate-limits-and-the-request-queue)).
- **Reads** return a response with useful `data` and an empty `errors` array as success with `ResultData.Status == "error"` (server-side warnings: `order/calc`, `prolong/calc` and `autoprolong/calc` with insufficient funds).
- **Money and write calls** (the kinds in [Rate limits and the request queue](#rate-limits-and-the-request-queue)) succeed only on `status: "success"`. `status: "error"` is an `*APIError` there even with `data` and an empty `errors[]` — the data stays in `APIError.Data`, and its `warning` becomes the text of the error. A reply without the JSON envelope — an HTML page, an empty body, `204`, a non-object or truncated JSON with a 2xx status, or an HTTP 5xx page — is an `*APIError` for which `errors.Is(err, api.ErrUnexpectedResponse)` holds: the request may have been executed, so check before retrying (see [Timeouts and retries on payments](#timeouts-and-retries-on-payments)). Reads, the `*/calc` endpoints and downloads are parsed as before.
- **Errors never carry the API key.** The key sits in the URL path, and its echo comes back in transport errors (`*url.Error` holds the full URL), in error pages and bodies (a front-end 404 page repeats the path in lower case, a Spring error body has it in `path`) and in redirects. Everywhere the SDK hands an error over — `*url.Error.URL`, `APIError.Body`, the messages, `data` and `customData` — the key is replaced with `***`, in any letter case and URL-encoded too. A wrapped transport error whose text quotes the key is replaced with one that has the same text without the key and still reports `Timeout()` and matches `context.DeadlineExceeded` / `context.Canceled`; the raw error is not reachable through `errors.Unwrap`. `APIError.Body` of a reply without the envelope keeps at most 500 characters, and `fmt` prints a `*Client` with `apiKey: "***"`.
- Validation limits are carried in `errors[].customData`; `AutoTopupLimitsFromError` unpacks the auto top-up ones.

## Rate limits and the request queue

The client paces its own requests so that a program stays under the API limits without any code of its own. Every Proxy-Seller SDK does this by the same rules, with the same defaults:

- **One window for all requests.** Reads, writes and money requests — retries included — share a sliding window of **1000 request starts within any 60 seconds**. When it is full, the next request waits until the oldest start in it is 60 seconds old. It is a sliding window, not a token bucket, so no burst can go over 1000 within one minute.
- **One queue for writes and payments.** Write and money requests of a client go through a single queue, one at a time, in the order they were made. Each starts only after the previous one has finished, and additionally
  - no earlier than **1 s** after the previous write or money request started;
  - if it is a money request, no earlier than **2 s** after the previous money request started.
- **Reads never wait for the queue** — only for the window — and run concurrently.
- **HTTP 429 is retried.** A 429 comes from the edge rate limit in front of the API: the request never reached the API, so repeating it is safe even for `order/make`. The client waits for `Retry-After` and tries again, up to **3 times**. `Retry-After` is read as a whole number of seconds (`0` retries at once) or an HTTP date; a missing value or anything else (`1.5` included) means 2 s, and a single wait never exceeds 60 s. A write or money request keeps its place in the queue meanwhile, and every retry counts as a new start in the window. When the retries run out, the call fails with the usual `*APIError`, `HTTPStatus == 429`.
- **Nothing else is retried.** Errors in the envelope come back exactly as before — in particular code 57, `Prolong for this order is already in progress` (a retry could renew an order twice), and the access-denied triple with code 503 (see [Error handling](#error-handling)), which cannot be told apart from a wrong key or IP.

The kind of a request is decided by its path alone — also for calls made through `Client.Request`:

| Kind | Endpoints |
|---|---|
| money | `order/make`, `prolong/make/{type}`, `balance/add` |
| write | `autoprolong/enable/{type}`, `autoprolong/disable/{type}`, `auth/add`, `auth/add/ip`, `auth/change`, `auth/delete`, `proxy/replace`, `proxy/comment/set`, `balance/autotopup/set`, `resident/list` (the POST alias of `resident/list/add`), `resident/list/add`, `resident/list/delete`, `resident/list/rename`, `resident/list/rotation`, `resident/list/tools`, `residentsubuser/create`, `residentsubuser/update`, `residentsubuser/delete`, `residentsubuser/list/add`, `residentsubuser/list/delete`, `residentsubuser/list/rename`, `residentsubuser/list/rotation`, `residentsubuser/list/tools` |
| read | everything else: `*/list`, `*/get`, every `*/calc` (`order/calc`, `prolong/calc/{type}` and `autoprolong/calc/{type}` are POST but change nothing), `reference/*`, `proxy/download/*`, `resident/package`, `resident/lists`, `resident/geo*`, `resident/consumption`, `resident/traffic/details`, `residentsubuser/packages`, `residentsubuser/lists`, `balance/payments/list`, `balance/autotopup/get` |

A `Client` is safe for concurrent use. Waiting blocks the calling goroutine (it sleeps, it does not spin) and happens before the request is sent, so it does not count against `WithTimeout` or `WithMoneyTimeout`.

### Changing or turning it off

```go
client := api.NewClient("YOUR_API_KEY",
    api.WithRequestsPerMinute(600),               // the window; default 1000
    api.WithWriteInterval(1500*time.Millisecond), // between write/money starts; default 1 s
    api.WithMoneyInterval(3*time.Second),         // between money starts; default 2 s
    api.WithMaxRetries(5),                        // HTTP 429 retries; default 3
)

unpaced := api.NewClient("YOUR_API_KEY", api.WithRateLimit(false)) // the previous behaviour, exactly
```

| Option | Default | Meaning |
|---|---|---|
| `WithRateLimit(enabled)` | `true` | `false` restores the previous behaviour exactly: no waiting, no retries |
| `WithRequestsPerMinute(n)` | `1000` | request starts within any 60 s; values below 1 are ignored |
| `WithWriteInterval(d)` | `1s` | between the starts of two write or money requests |
| `WithMoneyInterval(d)` | `2s` | between the starts of two money requests |
| `WithMaxRetries(n)` | `3` | retries after HTTP 429; `0` turns them off |

The package-level functions use `DefaultClient`, which is paced with the defaults. To configure it, replace it before the first call: `api.DefaultClient = api.NewClient("YOUR_API_KEY", api.WithRateLimit(false))` — the replacement keeps its own base URL, see [Client configuration](#client-configuration).

### One client per key

The window and the queue live in the `Client` instance. Clients — or processes — that share an API key do not know about each other: each keeps its own window and its own queue, so together they can still go over the limits. Create one `Client` per key and share it between goroutines. When several processes or machines work with one key anyway, the server can still answer code 57 (two renewals of the same order at once) or the access-denied triple (the key went over its limit) — handle them as before.

## Timeouts and retries on payments

Money calls — `order/make` (`MakeOrder`, `OrderMake*`), `prolong/make/{type}` (`MakeProlong`, `ProlongMake`) and `balance/add` (`AddBalance`, `BalanceAdd*`) — have a timeout of their own, **120 s** by default; every other call keeps `WithTimeout` (30 s). The server processes an order synchronously — a large MIX order takes about a second per country — and a money call the client gave up on is still an order that was created and paid.

```go
client := api.NewClient("YOUR_API_KEY",
    api.WithTimeout(15*time.Second),     // reads and writes
    api.WithMoneyTimeout(3*time.Minute), // money calls; default 120 s
)
```

- A money call waits for the longer of `WithTimeout` and `WithMoneyTimeout`, so a longer `WithTimeout` is never shortened. `WithMoneyTimeout(0)` turns the separate timeout off: money calls then wait as long as any other call.
- With `WithHTTPClient` your `*http.Client` is never modified. A money call goes through a copy of it with the longer `Timeout` and the same `Transport` (and connection pool), `Jar` and `CheckRedirect`. The timeouts of the `Transport` itself (`ResponseHeaderTimeout` and the like) still apply, and a client with `Timeout: 0` (no timeout) stays without one.
- Time spent waiting in the queue does not count against either timeout: the request has not been sent yet.

**A failed money call does not mean that nothing happened.** After a timeout, a dropped connection, an HTTP 5xx or a reply without the JSON envelope, the outcome is unknown: the order may have been created and paid, the renewal may have gone through, a payment link may have been issued. The SDK never repeats these calls by itself — the one exception is HTTP 429 from the edge rate limit, which never reached the API. Look before you call again:

| Call | Check first |
|---|---|
| `order/make` | `ListOrders` — the newest orders (`SortBy: "date_insert", Order: "desc"`) |
| `prolong/make/{type}` | `ListProxies` (the `date_end` of the proxies) or `ListOrders` with `IsExtend: "Y"` |
| `balance/add` | `Balance` and your payment history |

What such a failure looks like:

| Failure | Error |
|---|---|
| timeout | `*url.Error`; `os.IsTimeout(err)` is true |
| dropped connection, DNS, TLS | `*url.Error` |
| a reply without the JSON envelope: an HTML page, an empty body or truncated JSON with a 2xx status, or an HTTP 5xx page | `*api.APIError` with `errors.Is(err, api.ErrUnexpectedResponse)`, the `HTTPStatus` and the start of the reply in `Body` |
| HTTP 5xx with the envelope | `*api.APIError` with `HTTPStatus >= 500` |

```go
_, err := client.MakeOrder(order)
var urlErr *url.Error
var apiErr *api.APIError
if errors.As(err, &urlErr) || errors.Is(err, api.ErrUnexpectedResponse) ||
    (errors.As(err, &apiErr) && apiErr.HTTPStatus >= 500) {
    // the outcome is unknown: look the order up in ListOrders before placing it again
}
```

None of these errors contains the API key — see [Error handling](#error-handling).

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

Every `*Id` field of an order takes **the readable code** — `USA`, `1m`, `europe-2-mix_IPv4`. If the value is not a known ObjectId and the paired `*Code` field is empty, the server resolves it as a code. This covers `countryId`, `periodId`, `operatorId`, `mixId`, `tarifId` and `paymentId`, so an order needs **no `*Code` field at all** — put the code straight into the id:

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

`RotationID` is the one field that is neither an id nor a code: it is the **rotation interval in minutes** as a numeric string — `"5"`, `"10"`, `"0"` for By Link. `RotationCode` is redundant, because the server only checks that it is an integer and copies the value into `rotationId` without any reference lookup — a non-numeric value is answered with `"Set existed [rotationCode] from reference"`, and a non-numeric `rotationId` (`"5m"`) cannot be read as a number and comes back as `"Unknown error"`, code 35.

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
| `paymentId` | the code `balance` (pay from the balance) or `paddle_subscription` (the saved card) — the only two systems orders accept | fixed codes, nothing to look up; `balance/payments/list` is for `balance/add` only (see [Paying for orders](#paying-for-orders)) |

ObjectIds are still accepted everywhere if you happen to have them; the reference simply no longer publishes them. Code resolution happens in `order/calc`, `order/make`, `prolong/calc` and `prolong/make`.

The paired `*Code` fields (`CountryCode`, `PeriodCode`, `OperatorCode`, `MixCode`, `TarifCode`, `PaymentCode`) are still accepted and do the same job explicitly. You do not need them: a code in the `*Id` field resolves the same way. `RotationCode` is the exception — the server only copies it into `rotationId` and rejects a non-integer, so set `RotationID` and ignore it.

`customTargetName` — what you use the proxies for. Required for `ipv4`, `ipv6` and `isp`. For `mix`/`mix_isp` it is only needed when the server cannot tell which MIX package you mean; naming the package (`MixID`, or the first argument of `OrderCalcMix`) removes the need for it. Order a mix without either and the server answers `"Incorrect goal"`, code 14.

The legacy positional helpers take the same values, so codes go in without any placeholder chain — only `authorization` and `coupon` are genuinely optional:

```go
calc, err := client.OrderCalcMobile("USA", "1m", 1, "", "", "68b1f0c4e13a4c0f1a2b3c4d", "5")
ipv4, err := client.OrderCalcIpv4("USA", "1m", 1, "", "", "seo")
```

`OrderCalcMobile`/`OrderMakeMobile` always send `mobileServiceType=dedicated`, and `OrderCalcMobile` no longer builds an IPv6 request. For shared mobile, MIX, uptime, scraper or other new combinations, use `OrderRequest`.

### Listing orders

```go
orders, err := client.ListOrders(api.OrderListOptions{
    Status: "PAYED",       // PAYED | NOT_PAYED | RETURN — the status_type of the response
    SortBy: "date_insert", // date_insert | summ | status
    Order:  "desc",
    Page:   1,
    Limit:  20,
})

all, err := client.OrderList() // the same call with no filters at all
```

Every filter is optional. Query filters and response fields use snake_case names — `order_id`, `start_date`, `end_date`, `status`, `is_extend`, `auto_order`, `page`, `limit`, `sort_by`, `order` — and `OrderListOptions` maps its fields onto them.

`data` is not a flat list but a `metadata` + `items` pair, and `metadata` is always there: without `Limit` it reports `total_pages: 1`, `current_limit: 0` and the whole list in `items`. `summ` and `items[].price` are **strings with the currency already in them** (`"$25.00"`), `auto_order` and `is_extend` are `"Y"`/`"N"` rather than booleans, and the dates are ISO 8601 with offset (`2026-09-01T14:15:26+00:00`). `id` is a numeric order ID sent as a string; the ObjectId is `order_id` — the same value `ListProxies` returns as `order_id`, and the one `OrderIDs` takes when you renew `ipv6`, `mix` or `mix_isp` (see [Renewing proxies](#renewing-proxies)).

## proxy/replace takes a reason, not a proxy type

The `type` field of `proxy/replace` is the **replacement reason**, not the proxy type — the proxy type is derived from the first id in `ids`. Allowed values, exported as constants:

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

What you renew by depends on the type. `ipv4`, `isp` and `mobile` are renewed **per proxy**; `ipv6`, `mix` and `mix_isp` are sold and renewed **only as whole orders**. Every field below is one `ListProxies` publishes:

| type | pass | taken from | example |
|---|---|---|---|
| `ipv4`, `isp` | the address, or the proxy id | `ip`, or `id` | `1.2.3.4` / `68b1f0c4e13a4c0f1a2b3c4d` |
| `mobile` | the address, or the proxy id | `ip` + `:` + `port_http` + `:` + `port_socks`, or `id` | `10.0.0.1:50100:50101` / `68b1f0c4e13a4c0f1a2b3c4d` |
| `ipv6`, `mix`, `mix_isp` | the order id | `order_id` (the same value `ListOrders` returns as `order_id`) | `6a248de4717805635cf6057d` |

For the per-proxy types you can renew by the addresses themselves — no ids to look up:

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

`ProlongCalc` / `ProlongMake` route every value by its shape and by the type: an address (it contains `.` or `:`) goes to `ips`; anything else goes to `ids` for `ipv4`/`isp`/`mobile` and to `orderIds` for `ipv6`/`mix`/`mix_isp`.

**Pass either proxy ids or addresses in one call, not both.** For `ipv4`/`isp`/`mobile` a list that mixes them is refused locally with `mixing proxy ids and addresses in one call is not supported: pass either ids or addresses`, and nothing is sent: given both `ids` and `ips`, the server renews by `ids` and ignores `ips`, so the addresses would silently drop out of a paid renewal. For `ipv6`/`mix`/`mix_isp` the list is still routed as is — ids to `orderIds`, addresses to `ips` — and the server rejects the `ips` part itself.

**`ipv6`, `mix` and `mix_isp` are renewed as whole orders by `OrderIDs`.** Every active proxy of that type in those orders is renewed — for `mix`/`mix_isp`, the mix packages of those orders — so pass the `order_id`, not addresses or proxy ids:

```go
list, _ := client.ListProxies("ipv6", api.ProxyListOptions{})
first := list["items"].([]interface{})[0].(map[string]interface{})

req := api.ProlongRequest{OrderIDs: []string{first["order_id"].(string)}, PeriodID: "1m"}
quote, _ := client.CalculateProlong("ipv6", req)
made, err := client.MakeProlong("ipv6", req)
```

In the struct form the per-proxy types take `req.IDs` (proxy `id` values, sent as `ids`) or `req.IPs` (addresses, sent as `ips`) — set one of them, since with both the server renews by `ids` and ignores `ips`:

```go
req := api.ProlongRequest{IDs: []string{"68b1f0c4e13a4c0f1a2b3c4d"}, PeriodID: "1m"}
quote, _ := client.CalculateProlong("mobile", req)
```

The server refuses the whole request, and renews nothing, when:

- a selection field of the other kind is sent — the error (code 0) names it: `[ids] is not applicable for ipv6: prolong by [orderIds]`, `[ips] is not applicable for mix: prolong by [orderIds]`, `[orderIds] is not applicable for ipv4: prolong by [ids]`;
- an order is not yours or has no active proxy of that type, or `OrderIDs` is empty — code 29, `Incorrect orderIds`.

`ProlongCalc` shows the price; `ProlongMake` charges the balance. Its `data` is `orderId`, `orderIds`, `total`, `listBaseOrderNumbers` and `balance`: `orderIds` lists every renewed order — one request can renew several — and `orderId` is its first element; `listBaseOrderNumbers` holds one base order number per renewed order (per package for `mix`/`mix_isp`), matching `base_order_number` in `ListOrders`. If the balance is short, `ProlongMake` returns an `*APIError` with the server's warning — it never reports a renewal that did not happen.

## Automatic renewal

`ProlongMake` charges you now. `autoprolong/*` only arms a charge that happens later, without you present — a separate branch of the API, not a flag on prolong.

```go
req := api.AutoProlongRequest{}
req.IPs = []string{"1.2.3.4"}                   // or req.IDs; req.OrderIDs for ipv6 / mix / mix_isp
req.PeriodID = "1m"
req.PaymentID = "balance"                       // mandatory here

client.CalculateAutoProlong("ipv4", req)        // what will be charged, and when
client.EnableAutoProlong("ipv4", req)           // arm it
client.DisableAutoProlong("ipv4", req)          // disarm it
```

The selection follows the same rules as [renewal](#renewing-proxies): `IDs` or `IPs` for `ipv4`/`isp`/`mobile`, `OrderIDs` for `ipv6`/`mix`/`mix_isp`, where every active proxy of those orders is switched.

`PaymentID` is **mandatory** for calc and enable — the charge happens while you are away, so the payment system cannot be guessed. Only `balance` and `paddle_subscription` are accepted: a one-off Paddle checkout needs a browser redirect a headless client cannot complete. `paddle_subscription` charges the card saved on the account: set `SubscriptionID` only when the account has several saved cards (the server answers `Set [subscriptionId]`), with one card it is picked automatically.

Residential packages renew as a package, not as addresses — send no selection, only the payment system and optionally `TarifID` to confirm the tariff already on the package. **Any selection there is an error**: with a non-empty `IDs`, `IPs` or `OrderIDs`, `CalculateAutoProlong`, `EnableAutoProlong` and `DisableAutoProlong` for `resident` fail locally with `resident auto-prolong applies to the whole package: do not pass proxy or order ids`, before anything is sent (the server refuses any of `ids`, `ips` or `orderIds` there too, with `[ids] is not applicable for resident: auto-prolong applies to the whole package`). The SDK never drops the selection silently — otherwise a disable meant for a few addresses would switch off the whole package:

```go
client.EnableAutoProlong("resident", api.AutoProlongRequest{
    ProlongRequest: api.ProlongRequest{PaymentID: "balance"},
    TarifID:        "trial",
})
```

Three things about the answers before you parse them:

* **`ids` is not an echo.** `enable` and `disable` answer `ids[]` — the ids of the proxies actually affected — and `orderIds[]`, their orders. For `ipv6`, `mix` and `mix_isp` whole orders are switched, so `quantity` and `ids` cover every active proxy of the orders you sent. For `resident` both are empty.
* **Not enough money is not an error return.** `calc` answers `status: "error"` with a *filled* `data` and an empty `errors[]` — the same shape `prolong/calc` uses. Read `warning`.
* **Residential fills different fields.** `days` and `chargeDate` are null there (a package renews on expiry *or* on traffic exhaustion, so no single date describes it); `tarifId` and `dateEnd` carry the meaning instead.

`scraper` has no auto-renewal: it is extended by buying traffic through `order/make`.

> Replaces `resident/autorenew/{enable,disable,calculate}`, **removed** from the server.

## Prolong, proxy and resident options

- `CalculateProlong` / `MakeProlong` are the struct-based form of the same two calls: `ProlongRequest` carries `IDs` or `IPs` (`ipv4`/`isp`/`mobile`) or `OrderIDs` (`ipv6`/`mix`/`mix_isp`), `PeriodID`/`PeriodCode` and `PaymentID`/`PaymentCode`. `prolong/*` has the same id-or-code fallback as orders, so `PeriodID: "1m"` is enough.
- `ListProxies` accepts all current filters through `ProxyListOptions`.
- `ListOrders` accepts all `order/list` filters through `OrderListOptions`; `OrderList` is the no-filter shortcut. See [Listing orders](#listing-orders).
- `CreateResidentList` and `CreateResidentSubuserList` accept geo, export and rotation options.
- `CreateResidentSubuser` / `UpdateResidentSubuser` include string traffic limits, expiration, rotation, active and link-date fields.
- `resident/lists` returns `data` as a **flat array** (no `items` wrapper), matching v1.
- `resident/traffic/details` names the package key `packageKey` **or** `key` — not `package_key` — and it is required.
- Delete endpoints (`resident/list/delete`, `residentsubuser/delete`, `residentsubuser/list/delete`) return `data` as a **string**, sometimes with JSON inside (`{"status":"not-found"}` under `status: "success"`). The helpers normalize it to a map so a failed delete is distinguishable from a successful one.
- Subpackage `expired_at` is an **object** — a date object `{date, timezone_type, timezone}`: `date` is UTC in `yyyy-MM-dd HH:mm:ss.SSSSSS`, `timezone_type` is always `3`, `timezone` is always `UTC` — while the parent package `expired_at` is a formatted string `"31.12.2025 23:59:59"`. Traffic values are strings in both.

Raw scalar `data` is available in `ResultData.Value`; object and array values remain available in `Map` and `Slice`; the untouched JSON is in `ResultData.Raw`.

## Migrating from v1

| v1 | v2 |
|---|---|
| `import "github.com/proxy-seller/userApiGolang"` | `import api "github.com/proxy-seller/userApiGolang/v2"` — a new module path, so the old one keeps resolving for v1 callers |
| numeric ids (`orderId => 1000000`) | ObjectId strings (`orderId => "68b1f0c4e13a4c0f1a2b3c4d"`); resident list ids stay numeric |
| `ProxyCheck`, `Ping` | removed, no v2 equivalent |
| `Balance() float64`, `BalanceAdd(...) string` | kept in the v1 shape, without an error result (`paymentId` is a string now), and deprecated — a failure comes back as `-1` / `""`; use `BalanceE` / `BalanceAddE` or the `*Client` methods |
| `targetId` + `targetSectionId` | `customTargetName` only |
| `resident/lists` wrapped in `items` | flat array in `data` |
| HTTP 429 on rate limit | HTTP 200 with the three-error access triple, `code: 503` |
| `type` of `proxy/replace` looked like a proxy type | it is the replacement reason enum |
| `package_key` on `proxy/download/resident` | only on `proxy/download/subresident` |
| numeric `paymentId` | orders and renewals: the code `balance` or `paddle_subscription`; `balance/add`: an ObjectId string from `balance/payments/list`, which does **not** resolve `paymentCode` |
| numeric proxy ids for renewal | `ipv4`/`isp`/`mobile`: the address or the proxy `id` from `ListProxies` (`IPs` / `IDs`); `ipv6`/`mix`/`mix_isp`: the `order_id` (`OrderIDs`) — they renew as whole orders |
| `errors[].code` read as a number from an `interface{}` field | typed `APIErrorCode`, use `CodeInt()` |

## Changelog

The tag `v2.0.0` (2026-10-01) is the first release of the `/v2` module and contains both
`v2.0.0` parts below. They were written while v2 was still in development, and part 2 was
planned as v2.0.1 at the time; nothing was tagged in between.

### v2.0.1 — 2026-10-02

- **Fixed (money): `OrderRequest.Quantity` is always sent.** The field had `omitempty`, so
  `Quantity: 0` — or a forgotten `Quantity` — was dropped, the server treated the missing
  quantity as 1, and `MakeOrder` bought one proxy instead of being refused (`CalculateOrder`
  quoted one). Now `0` reaches the server and is refused with `Set [quantity] more than 0`, like
  in every other SDK and the raw API. `resident` and `scraper` ignore the field. **Behaviour
  change:** a regular-section `MakeOrder` / `CalculateOrder` without `Quantity` used to buy or
  quote 1; set `Quantity` explicitly.
- `OrderRequest.SectionCode` is documented as required: it was always sent, and an empty value is
  refused with `Set existed [sectionCode] from reference` — on purpose, the server does not guess
  ipv4 for an empty section.
- **Deprecated:** the package-level `Balance()` and `BalanceAdd()`. They have no error result: a
  failed call comes back as `-1` / `""`. Use `BalanceE()` / `BalanceAddE()` or the `*Client`
  methods.
- README: the install section no longer says the module does not resolve — `v2.0.0` is tagged.

### v2.0.0, part 2 of 2 — catching up with the server

- **Errors no longer carry the API key.** A transport error (`*url.Error`: port closed, timeout, dropped connection, redirect) used to be returned as is, with the full URL — key included — in its text, and `APIError.Body` kept error pages that echo the path. The key is now replaced with `***`, in any letter case and URL-encoded too, in `*url.Error.URL`, `APIError.Body`, the messages, `data` and `customData`; a wrapped transport error that quotes the key is replaced by one without it that keeps `Timeout()` and matches the same context errors. `fmt` prints a `*Client` with `apiKey: "***"`. `APIError.Body` of a reply without the JSON envelope keeps at most 500 characters.
- **Behaviour change: money and write calls succeed only on `status: "success"`.** `status: "error"` with `data` and an empty `errors[]` used to pass as a success on every route; it stays one only on reads (the `*/calc` endpoints with insufficient funds) and is an `*APIError` on make and write calls, with `data.warning` as its text. A reply without the JSON envelope on these calls — HTML, an empty body, `204`, a non-object or truncated JSON with a 2xx status, or an HTTP 5xx page — used to surface as a bare `*json.SyntaxError` (or a plain `client api HTTP 502`); it is now an `*APIError` for which `errors.Is(err, ErrUnexpectedResponse)` holds, with the text `unexpected response (no JSON envelope): the request may have been executed, check before retrying`. Added `ErrUnexpectedResponse` and `APIError.Unwrap`. Reads, `*/calc` and downloads are unchanged.
- **Money calls get their own timeout, 120 s by default** (`WithMoneyTimeout`); every other call keeps 30 s. A money call waits for the longer of the two, and the `*http.Client` of `WithHTTPClient` is not modified — the money call goes through a copy with the longer `Timeout`. New section [Timeouts and retries on payments](#timeouts-and-retries-on-payments): a timeout, a dropped connection, a 5xx or a reply without the envelope on `order/make`, `prolong/make` or `balance/add` leaves the outcome unknown — check `ListOrders`, `ListProxies` or `Balance` before calling again.
- **Fixed: a replaced `DefaultClient` was sent to production.** Every package-level call reset the base URL of `DefaultClient` to `URL`, whose default value is the production URL, so `api.DefaultClient = api.NewClient(key, api.WithBaseURL(mock))` was ignored by the package-level functions, `OrderMake*` included. `URL` now overrides `DefaultClient` only while it differs from `DefaultBaseURL`, and setting it back restores the client's own base URL.
- **Deprecated:** the package-level setters `SetApiKey`, `SetPaymentId`, `SetPaymentCode`, `SetGenerateAuth`, `SetFingerprint` and the variable `URL`. The package-level API keeps one state for the whole process — use `NewClient`.
- README: corrected [Error handling](#error-handling), which claimed that non-2xx statuses come only from the `ext` rejection and the edge 429 — gateways and the front-end answer 5xx and 404 pages too.
- **Behaviour change (server side): `ListProxies` filters.** `Latest: "Y"` now means the latest order of the requested type (of the MIX orders for `mix` / `mix_isp`) instead of the latest order of the whole account, which left the list empty whenever another type had been bought last; without a type it is still one latest order for the whole response, and it no longer empties `resident` and `scraper`. `OrderID` also accepts the numeric `id` of a `ListOrders` row, `base_order_number` and an earlier `order_number` of a renewed order, not only `order_id` and the exact current `order_number`. No SDK code changed — both values go to the server as they are.
- **Behaviour change: requests are now paced by default** (see [Rate limits and the request queue](#rate-limits-and-the-request-queue)). All requests share a sliding window of 1000 starts within any 60 s; write and money requests run one at a time through a queue, at least 1 s apart (money requests at least 2 s apart); HTTP 429 from the edge rate limit is retried after `Retry-After`, up to 3 times. Code 57 and the access-denied triple are returned as before and never retried. New options `WithRateLimit`, `WithRequestsPerMinute`, `WithWriteInterval`, `WithMoneyInterval` and `WithMaxRetries`; `WithRateLimit(false)` restores the previous behaviour exactly.
- **Breaking — renewal selection follows the server.** `ProlongRequest.OrderSeparatorIDs` / `OrderSeparatorID` (`orderSeparatorIds` / `orderSeparatorId`) are removed — the server dropped both; use `OrderIDs`. `IDs` (`ids`) and `IPs` (`ips`) now apply to `ipv4`, `isp` and `mobile` only: for `ipv6`, `mix` and `mix_isp` the server refuses them. `AutoProlongRequest` embeds `ProlongRequest`, so the same applies to `autoprolong/*`.
- Added `ProlongRequest.OrderIDs` (`orderIds`). `ipv6`, `mix` and `mix_isp` are renewed only as whole orders, by the `order_id` from `ListProxies` / `ListOrders`, on `prolong/*` and `autoprolong/*`; `ipv4`, `isp` and `mobile` keep renewing per proxy by `IDs` (`ids`, the proxy `id`) or `IPs` (`ips`, the addresses) — with both set, the server renews by `ids` and ignores `ips`. A selection field of the other kind is rejected by the server with an error that names it (code 0), e.g. `[ids] is not applicable for ipv6: prolong by [orderIds]`.
- `ProlongCalc` / `ProlongMake` route by type: an address goes to `ips`, any other value to `ids` (`ipv4`/`isp`/`mobile`) or `orderIds` (`ipv6`/`mix`/`mix_isp`), and `resident` sends no selection. For `ipv6`/`mix`/`mix_isp` the fallback for values the SDK cannot split goes to `orderIds`, not `ids`.
- **Behaviour change:** for `ipv4`/`isp`/`mobile`, `ProlongCalc` / `ProlongMake` now refuse a list that mixes proxy ids and addresses (error, no request) — with both fields the server renews by `ids` and ignores `ips`, so the addresses would silently drop out of a paid renewal.
- **Behaviour change:** `CalculateAutoProlong` / `EnableAutoProlong` / `DisableAutoProlong` with `type = resident` now refuse any non-empty `IDs`, `IPs` or `OrderIDs` (error, no request): the package renews as a whole, and a selection is never dropped silently.
- **Response fields.** `autoprolong/enable` and `autoprolong/disable` answer the new `orderIds` — the orders of the affected proxies — next to `ids`; both are empty for `resident`. `prolong/make` answers `orderIds` — every renewed order, with `orderId` kept as its first element — and `listBaseOrderNumbers` holds one base order number per renewed order or mix package.
- Added `ListOrders` / `OrderList` (and the package-level `OrderList`) for `GET order/list`, with `OrderListOptions`. Query filters and response fields use snake_case names (`order_id`, `start_date`, `end_date`, `status`, `is_extend`, `auto_order`, `page`, `limit`, `sort_by`, `order`). `data` carries `metadata` + `items`, and `summ` / `items[].price` are currency strings, not numbers.
- Added `CalculateAutoProlong` / `EnableAutoProlong` / `DisableAutoProlong` (and their package-level counterparts) for `autoprolong/{calc,enable,disable}/{type}`, with `AutoProlongRequest`. `PaymentID` is required on calc and enable and restricted to `balance` / `paddle_subscription`; `scraper` is rejected locally. With `paddle_subscription`, `SubscriptionID` is needed only when the account has several saved cards: with one the server charges it, with none it answers `No saved card on the account: add a card in your account or use [paymentId] balance`. Earlier builds of this branch required `SubscriptionID` locally.
- The server **removed** `resident/autorenew/{enable,disable,calculate}` — `type: "resident"` on the three endpoints above replaces them.
- Added the optional `X-Fingerprint` header on `order/make`: `WithFingerprint(...)` / `SetFingerprint(...)`, or `OrderRequest.Fingerprint` for a single call. It is sent whenever a value is set and never required — the server does not refuse API-key orders without it, so residential and scraper orders no longer fail locally when it is unset.
- Removed `DailyCountCap` / `MonthlyAmountCap` from auto top-up (dropped from the contract 2026-08-18 and silently ignored), along with error codes `54`/`55` and `MinDailyCountCap`.
- `MaxLine` is now sent on `proxy/download/resident` — the only route that accepts it. `Proto`, `Country` and `Ends` are no longer sent there, since that literal route ignores them.
- `*Code` no longer overrides a paired `*Id` for `MixID`, `OperatorID`, `RotationID` and `TarifID`: the server applies the code on those four only while the id is empty, so the SDK was inverting the contract.
- Fixed two README examples that did not compile (`SetPaymentId` → `SetPaymentID`, `ListProxies` takes `ProxyListOptions`).
- Fixed the "Paying for orders" example: it took `payments[0]` from `BalancePaymentsList`, which lists top-up systems for `AddBalance` only and never the balance itself. Orders and renewals are paid with `SetPaymentCode("balance")` or `SetPaymentCode("paddle_subscription")` (the saved card) — the only two systems `order/make` accepts.
- Test suite repaired and extended. `go test ./...` had stopped compiling: `TestAutoTopupLimitsFromError` still referenced `MinDailyCountCap`, removed with the caps above. `TestProlongMakeInsufficientFundsIsAnError` still asserted the old insufficient-funds shape (`status:"error"` with an empty `errors[]`); the server now sends `errors[{code:16}]`, and the test covers that plus the case the removed guard used to break — a legitimate success with an empty `orderId` must keep `total` and `listBaseOrderNumbers`. Added `TestGenerateAuthPrecedence`: `OrderRequest.GenerateAuth` outranks `SetGenerateAuth()`, and neither is sent on `order/calc`, which drops the field.

### v2.0.0, part 1 of 2

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

To exercise a local or staging instance of the API, point the client at it with `WithBaseURL` and call read-only endpoints first (`Balance`, `AuthList`, `ResidentLists`).
