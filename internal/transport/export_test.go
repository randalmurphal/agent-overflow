package transport

import (
	"net/http"
	"time"
)

// AuthRateLimitedForTest is next behind the auth routes' budget
// (authRateLimit) on the clock now, so a test outside this package can drive
// a client through the production limiter.
func AuthRateLimitedForTest(now func() time.Time, next http.HandlerFunc) http.HandlerFunc {
	limiter := newRateLimiter("/auth", authRateLimit)
	limiter.now = now
	return rateLimited(limiter, next)
}
