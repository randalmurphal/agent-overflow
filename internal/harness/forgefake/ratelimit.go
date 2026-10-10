package forgefake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// rateLimitQuota is the Limit every faked pool reports: GitHub's
// authenticated REST and GraphQL hourly quota. The reserve the app keeps
// for the user's own actions is a tenth of it.
const rateLimitQuota = 5000

// RateLimit puts one quota pool of a forge under a limit until Reset (unix
// seconds). Pool is the pool a request draws on: GitHub's "core" (REST)
// or "graphql", GitLab's "throttle_authenticated_api". With Remaining 0
// every request to the pool is refused the way the forge refuses it:
// GitHub REST with 403, GitHub GraphQL with a RATE_LIMITED error, GitLab
// with 429 and Retry-After. With Remaining above 0 requests are answered
// as usual and carry the pool's quota headers, so the app's transport
// sees the pool running low. Remaining does not count down. The limit
// lifts once Reset passes.
type RateLimit struct {
	Forge     string `json:"forge"`
	Pool      string `json:"pool"`
	Remaining int    `json:"remaining"`
	Reset     int64  `json:"reset"`
}

type rateLimitKey struct{ forge, pool string }

// ratePools are the pools each forge's requests draw on.
var ratePools = map[string][]string{
	"github": {"core", "graphql"},
	"gitlab": {"throttle_authenticated_api"},
}

// SetRateLimit puts a pool under limit, replacing any limit it had.
// Reset clears every limit.
func (e *Engine) SetRateLimit(limit RateLimit) error {
	pools, ok := ratePools[limit.Forge]
	if !ok {
		return fmt.Errorf("forge rate limit: unknown forge %q", limit.Forge)
	}
	known := false
	for _, pool := range pools {
		known = known || pool == limit.Pool
	}
	switch {
	case !known:
		return fmt.Errorf("forge rate limit: %s has no pool %q (pools: %v)", limit.Forge, limit.Pool, pools)
	case limit.Remaining < 0 || limit.Remaining > rateLimitQuota:
		return fmt.Errorf("forge rate limit: remaining %d is outside 0..%d", limit.Remaining, rateLimitQuota)
	case limit.Reset <= time.Now().Unix():
		return fmt.Errorf("forge rate limit: reset %d is not in the future", limit.Reset)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rateLimits == nil {
		e.rateLimits = make(map[rateLimitKey]RateLimit)
	}
	e.rateLimits[rateLimitKey{limit.Forge, limit.Pool}] = limit
	return nil
}

// rateLimitLocked returns the limit on a request's pool, dropping one
// whose reset has passed. Callers hold mu.
func (e *Engine) rateLimitLocked(c *call, now time.Time) (RateLimit, bool) {
	key := rateLimitKey{forge: "github", pool: "core"}
	switch {
	case c.cli == "glab":
		key = rateLimitKey{forge: "gitlab", pool: "throttle_authenticated_api"}
	case c.graphQL != nil:
		key.pool = "graphql"
	}
	limit, ok := e.rateLimits[key]
	if !ok {
		return RateLimit{}, false
	}
	if now.Unix() >= limit.Reset {
		delete(e.rateLimits, key)
		return RateLimit{}, false
	}
	return limit, true
}

// headers are the pool's quota headers in the forge's own names.
func (l RateLimit) headers() http.Header {
	reset := strconv.FormatInt(l.Reset, 10)
	if l.Forge == "gitlab" {
		return http.Header{
			"Ratelimit-Limit":     {strconv.Itoa(rateLimitQuota)},
			"Ratelimit-Remaining": {strconv.Itoa(l.Remaining)},
			"Ratelimit-Reset":     {reset},
			"Ratelimit-Name":      {l.Pool},
		}
	}
	return http.Header{
		"X-Ratelimit-Limit":     {strconv.Itoa(rateLimitQuota)},
		"X-Ratelimit-Remaining": {strconv.Itoa(l.Remaining)},
		"X-Ratelimit-Reset":     {reset},
		"X-Ratelimit-Used":      {strconv.Itoa(rateLimitQuota - l.Remaining)},
		"X-Ratelimit-Resource":  {l.Pool},
	}
}

// decorate adds the pool's quota headers to an answer.
func (l RateLimit) decorate(resp response) response {
	header := resp.header.Clone()
	if header == nil {
		header = http.Header{}
	}
	for name, values := range l.headers() {
		header[name] = values
	}
	resp.header = header
	return resp
}

// refusal is the forge's answer to a request on an exhausted pool.
func (l RateLimit) refusal(now time.Time) response {
	resp := response{route: "rate limited", header: l.headers()}
	switch {
	case l.Forge == "gitlab":
		resp.status = http.StatusTooManyRequests
		resp.stdout = []byte("Retry later\n")
		resp.stderr = "429 Retry later"
		resp.header.Set("Content-Type", "text/plain")
		resp.header.Set("Retry-After", strconv.FormatInt(max(l.Reset-now.Unix(), 1), 10))
	case l.Pool == "graphql":
		out, _ := json.Marshal(map[string]any{"errors": []map[string]any{{
			"type":    "RATE_LIMITED",
			"message": "API rate limit already exceeded for user ID 1.",
		}}})
		resp.stdout = out
		resp.header.Set("Content-Type", "application/json; charset=utf-8")
	default:
		resp.status = http.StatusForbidden
		resp.stdout = []byte(`{"message":"API rate limit exceeded for user ID 1.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api","status":"403"}`)
		resp.stderr = "403 API rate limit exceeded"
		resp.header.Set("Content-Type", "application/json; charset=utf-8")
	}
	return resp
}
