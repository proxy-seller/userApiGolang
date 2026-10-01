package userApiGolang

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client-side pacing of API requests — the README section "Rate limits and the request queue".
//
// Every Proxy-Seller SDK paces its requests by the same rules, so that a program stays under the
// API limits without doing anything itself:
//
//   - Global window. All requests of a client — reads, writes and money requests, retries
//     included — share a sliding window: at most requestsPerMinute request starts within any
//     60 seconds. It is a log of the recent start times, not a token bucket: a bucket lets a burst
//     exceed the limit within one 60-second span.
//   - Write lane. Write and money requests go through one serialized lane per client, one at a
//     time in arrival order. The next one starts only after the previous one has finished, no
//     earlier than writeInterval after the previous write or money request started and, for a
//     money request, no earlier than moneyInterval after the previous money request started. Reads
//     never wait for the lane, only for the window.
//   - HTTP 429. The edge rate limit answers 429 before the request reaches the API, so repeating it
//     is safe even for a money request: the client waits Retry-After and retries, up to maxRetries
//     times. A retried write or money request keeps its place in the lane, and every retry is a new
//     start in the global window. Nothing else is retried — in particular not the envelope errors
//     code 57 ("Prolong for this order is already in progress": a retry could renew an order twice)
//     and the access-denied triple (code 503), which cannot be told apart from a wrong key or IP.
//
// The state lives in the Client: several clients or processes sharing one key do not coordinate.

const (
	defaultRequestsPerMinute = 1000
	defaultWriteInterval     = time.Second
	defaultMoneyInterval     = 2 * time.Second
	defaultMaxRetries        = 3

	rateLimitWindow   = time.Minute     // the span of the global window
	defaultRetryAfter = 2 * time.Second // a 429 without a usable Retry-After
	maxRetryAfter     = time.Minute     // the longest single wait after a 429
)

// WithRateLimit turns the client-side pacing of requests on or off. It is on by default; false
// restores the previous behaviour exactly — no waiting and no retries. See the README section
// "Rate limits and the request queue".
func WithRateLimit(enabled bool) ClientOption {
	return func(c *Client) { c.requestLimiter().enabled = enabled }
}

// WithRequestsPerMinute sets the global window: at most perMinute request starts — reads, writes and
// money requests together, retries included — within any 60 seconds. Default 1000; a value below 1
// is ignored (use WithRateLimit(false) to switch the pacing off).
func WithRequestsPerMinute(perMinute int) ClientOption {
	return func(c *Client) {
		if perMinute > 0 {
			c.requestLimiter().requestsPerMinute = perMinute
		}
	}
}

// WithWriteInterval sets the shortest gap between the starts of two consecutive write or money
// requests. Default 1 s; 0 (or a negative value) means no gap, the lane still runs one request at
// a time.
func WithWriteInterval(interval time.Duration) ClientOption {
	return func(c *Client) { c.requestLimiter().writeInterval = nonNegativeDuration(interval) }
}

// WithMoneyInterval sets the shortest gap between the starts of two consecutive money requests —
// order/make, prolong/make/{type} and balance/add. Default 2 s; 0 (or a negative value) means no gap
// beyond WithWriteInterval.
func WithMoneyInterval(interval time.Duration) ClientOption {
	return func(c *Client) { c.requestLimiter().moneyInterval = nonNegativeDuration(interval) }
}

// WithMaxRetries sets how many times a request answered with HTTP 429 is retried before the call
// fails with an *APIError whose HTTPStatus is 429. Default 3; 0 (or a negative value) means no
// retries. No other failure is ever retried.
func WithMaxRetries(retries int) ClientOption {
	return func(c *Client) {
		if retries < 0 {
			retries = 0
		}
		c.requestLimiter().maxRetries = retries
	}
}

func nonNegativeDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}

// requestLimiter returns the pacing state of the client. NewClient creates it; a Client that was not
// made by NewClient gets the default one on first use.
func (c *Client) requestLimiter() *rateLimiter {
	c.mu.RLock()
	limiter := c.limiter
	c.mu.RUnlock()
	if limiter != nil {
		return limiter
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limiter == nil {
		c.limiter = newRateLimiter()
	}
	return c.limiter
}

// requestCategory — how a request is paced.
type requestCategory int

const (
	// categoryRead waits only for the global window.
	categoryRead requestCategory = iota
	// categoryWrite also goes through the write lane (writeInterval).
	categoryWrite
	// categoryMoney also goes through the write lane (writeInterval and moneyInterval).
	categoryMoney
)

// requestCategories is the one table that decides how a request is paced. It is keyed by the path,
// not by the HTTP method: order/calc, prolong/calc/{type} and autoprolong/calc/{type} are POST but
// read only. "{type}" stands for any single last segment. A path that is not listed is a read.
var requestCategories = map[string]requestCategory{
	// money: the request charges the account
	"order/make":          categoryMoney,
	"prolong/make/{type}": categoryMoney,
	"balance/add":         categoryMoney,

	// write: the request changes something
	"autoprolong/enable/{type}":     categoryWrite,
	"autoprolong/disable/{type}":    categoryWrite,
	"auth/add":                      categoryWrite,
	"auth/add/ip":                   categoryWrite,
	"auth/change":                   categoryWrite,
	"auth/delete":                   categoryWrite,
	"proxy/replace":                 categoryWrite,
	"proxy/comment/set":             categoryWrite,
	"balance/autotopup/set":         categoryWrite,
	"resident/list":                 categoryWrite, // the POST alias of resident/list/add
	"resident/list/add":             categoryWrite,
	"resident/list/delete":          categoryWrite,
	"resident/list/rename":          categoryWrite,
	"resident/list/rotation":        categoryWrite,
	"resident/list/tools":           categoryWrite,
	"residentsubuser/create":        categoryWrite,
	"residentsubuser/update":        categoryWrite,
	"residentsubuser/delete":        categoryWrite,
	"residentsubuser/list/add":      categoryWrite,
	"residentsubuser/list/delete":   categoryWrite,
	"residentsubuser/list/rename":   categoryWrite,
	"residentsubuser/list/rotation": categoryWrite,
	"residentsubuser/list/tools":    categoryWrite,
}

// classifyRequest finds the category of a request by its path relative to the API root (the part
// after the key, as sent). The query string, the letter case and extra slashes do not matter.
func classifyRequest(path string) requestCategory {
	if end := strings.IndexAny(path, "?#"); end >= 0 {
		path = path[:end]
	}
	segments := strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' })
	if len(segments) == 0 {
		return categoryRead
	}
	if category, listed := requestCategories[strings.Join(segments, "/")]; listed {
		return category
	}
	segments[len(segments)-1] = "{type}"
	if category, listed := requestCategories[strings.Join(segments, "/")]; listed {
		return category
	}
	return categoryRead
}

// rateLimiter paces the requests of one Client. The settings are written by the ClientOptions before
// the client is used and never change afterwards; the lane is a channel, everything else is guarded
// by mu.
type rateLimiter struct {
	enabled           bool
	requestsPerMinute int
	writeInterval     time.Duration
	moneyInterval     time.Duration
	maxRetries        int

	// now and sleep are time.Now and time.Sleep; the tests put a fake clock here.
	now   func() time.Time
	sleep func(time.Duration)

	// lane is the write lane: a token with room for one. A write or money request holds it from the
	// moment it is admitted until its response has been read, retries included. Blocked senders are
	// admitted one by one in the order they arrived.
	lane chan struct{}

	mu sync.Mutex
	// starts is the window log: the start times of the requests of the last 60 seconds in ascending
	// order, plus the starts already booked by requests that are still waiting for them — which is
	// what makes the window first come, first served.
	starts []time.Time
	// lastWriteStart is the start of the last write or money request, lastMoneyStart that of the
	// last money request.
	lastWriteStart time.Time
	lastMoneyStart time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		enabled:           true,
		requestsPerMinute: defaultRequestsPerMinute,
		writeInterval:     defaultWriteInterval,
		moneyInterval:     defaultMoneyInterval,
		maxRetries:        defaultMaxRetries,
		now:               time.Now,
		sleep:             time.Sleep,
		lane:              make(chan struct{}, 1),
	}
}

// exchangeResult is the outcome of one HTTP round trip: the status and the headers of the response
// and its body, or the error that stopped it.
type exchangeResult struct {
	status int
	header http.Header
	body   []byte
	err    error
}

// run performs one API call of the given category. exchange is one HTTP round trip; it is called
// again when the response is HTTP 429 and retries are left.
func (l *rateLimiter) run(category requestCategory, exchange func() exchangeResult) exchangeResult {
	if !l.enabled {
		return exchange()
	}
	inLane := category != categoryRead
	if inLane {
		l.lane <- struct{}{}
		defer func() { <-l.lane }()
		l.waitForTurn(category == categoryMoney)
	}
	for retry := 0; ; retry++ {
		l.waitForWindow()
		if inLane {
			l.markStarted(category == categoryMoney)
		}
		result := exchange()
		if result.status != http.StatusTooManyRequests || retry >= l.maxRetries {
			return result
		}
		l.pause(retryAfterDelay(result.header.Get("Retry-After"), l.now()))
	}
}

// waitForTurn runs inside the lane: it waits writeInterval after the previous write or money start
// and, for a money request, moneyInterval after the previous money start — whichever ends later.
func (l *rateLimiter) waitForTurn(money bool) {
	l.mu.Lock()
	now := l.now()
	ready := now
	if !l.lastWriteStart.IsZero() {
		if at := l.lastWriteStart.Add(l.writeInterval); at.After(ready) {
			ready = at
		}
	}
	if money && !l.lastMoneyStart.IsZero() {
		if at := l.lastMoneyStart.Add(l.moneyInterval); at.After(ready) {
			ready = at
		}
	}
	l.mu.Unlock()
	l.pause(ready.Sub(now))
}

// markStarted records the start of a write or money request for the intervals of the next one. A
// retry records its own start: the attempt that gets through is the one the API sees.
func (l *rateLimiter) markStarted(money bool) {
	l.mu.Lock()
	now := l.now()
	l.lastWriteStart = now
	if money {
		l.lastMoneyStart = now
	}
	l.mu.Unlock()
}

// waitForWindow books the earliest start the global window allows and waits for it: a new start is
// allowed once the requestsPerMinute-th most recent one is 60 seconds old.
func (l *rateLimiter) waitForWindow() {
	l.mu.Lock()
	now := l.now()
	expired := 0
	for expired < len(l.starts) && !now.Before(l.starts[expired].Add(rateLimitWindow)) {
		expired++
	}
	l.starts = l.starts[expired:]
	start := now
	if len(l.starts) >= l.requestsPerMinute {
		if free := l.starts[len(l.starts)-l.requestsPerMinute].Add(rateLimitWindow); free.After(start) {
			start = free
		}
	}
	l.starts = append(l.starts, start)
	l.mu.Unlock()
	l.pause(start.Sub(now))
}

func (l *rateLimiter) pause(wait time.Duration) {
	if wait > 0 {
		l.sleep(wait)
	}
}

// retryAfterDelay reads the Retry-After header of an HTTP 429 as RFC 9110 defines it: delay-seconds
// (a non-negative whole number, digits only — "0" means retry at once) or an HTTP date (one in the
// past means no wait). Anything else — missing, "1.5", "-1", "NaN", garbage — means 2 s, and a single
// wait never exceeds 60 s.
func retryAfterDelay(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if isDelaySeconds(value) {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds >= int64(maxRetryAfter/time.Second) {
			return maxRetryAfter // digits only, so the one possible error is an overflow
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		delay := at.Sub(now)
		if delay < 0 {
			return 0
		}
		if delay > maxRetryAfter {
			return maxRetryAfter
		}
		return delay
	}
	return defaultRetryAfter
}

// isDelaySeconds — the delay-seconds form of Retry-After: one or more ASCII digits and nothing else.
func isDelaySeconds(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
