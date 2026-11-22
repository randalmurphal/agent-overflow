package linear

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Usage is what one answer reported about Linear's rate limits: the
// cost of that request and the budgets left. Linear limits a user per
// hour by request count and by complexity points, refilled continuously;
// a single query may cost at most 10,000 points.
type Usage struct {
	// Complexity is the request's cost in points (x-complexity); zero
	// when the answer did not say.
	Complexity int
	// Requests is the request-count budget (x-ratelimit-requests-*).
	Requests Budget
	// Points is the complexity budget (x-ratelimit-complexity-*).
	Points Budget
}

// Budget is one rate limit's state as of an answer.
type Budget struct {
	// Known is false when the answer carried none of this limit's headers.
	Known     bool
	Limit     int
	Remaining int
	// Reset is when the window is full again; zero when not sent.
	Reset time.Time
}

// Allows reports whether the budget, as last reported, still has n
// units left. An unknown budget allows anything.
func (b Budget) Allows(n int) bool { return !b.Known || b.Remaining >= n }

// usageHeaders are the answer headers usageOf reads, which the transport
// reports for every answer.
var usageHeaders = []string{
	"X-Complexity",
	"X-Ratelimit-Requests-Limit", "X-Ratelimit-Requests-Remaining", "X-Ratelimit-Requests-Reset",
	"X-Ratelimit-Complexity-Limit", "X-Ratelimit-Complexity-Remaining", "X-Ratelimit-Complexity-Reset",
}

func usageOf(h http.Header) Usage {
	u := Usage{
		Requests: budgetOf(h, "X-Ratelimit-Requests-"),
		Points:   budgetOf(h, "X-Ratelimit-Complexity-"),
	}
	if n, ok := headerInt(h, "X-Complexity"); ok {
		u.Complexity = n
	}
	return u
}

func budgetOf(h http.Header, prefix string) Budget {
	var b Budget
	if n, ok := headerInt(h, prefix+"Limit"); ok {
		b.Known, b.Limit = true, n
	}
	if n, ok := headerInt(h, prefix+"Remaining"); ok {
		b.Known, b.Remaining = true, n
	}
	if t, ok := headerMillis(h, prefix+"Reset"); ok {
		b.Known, b.Reset = true, t
	}
	return b
}

func headerInt(h http.Header, name string) (int, bool) {
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return 0, false
	}
	// Complexity can be sent with a fraction; whole points are reported.
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 1<<53 {
		return 0, false
	}
	return int(f), true
}

// headerMillis reads a Unix time in milliseconds, the unit Linear's reset
// headers use.
func headerMillis(h http.Header, name string) (time.Time, bool) {
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}
