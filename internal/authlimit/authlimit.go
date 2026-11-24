// Package authlimit is a backend's auth rate limit as its clients read it:
// the HTTP 429 that the transport's per-peer budget answers on every /auth
// route, with its Retry-After. Both pairing clients (deviceclient and
// pairbootstrap) return it, so a person is told the same wait wherever the
// limit stops them.
package authlimit

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimited is the limit answering a request before any credential was
// judged. It is temporary, as every answer but an authentication verdict
// is, and it tells a person to wait.
type RateLimited struct {
	// Attempts names what the limit counts, as a person reads it.
	Attempts string
	// Then is what to do once the wait is over.
	Then string
	// RetryAfter is the backend's Retry-After, 0 when it named none.
	RetryAfter time.Duration
}

// By is the rate limit a 429 answer with header h names, counting
// attempts; trying again after the wait is the remedy unless the caller
// knows a retry needs more.
func By(h http.Header, attempts string) *RateLimited {
	return &RateLimited{Attempts: attempts, Then: "try again", RetryAfter: retryAfter(h)}
}

func (e *RateLimited) Error() string {
	wait := "wait a moment"
	switch seconds := int(e.RetryAfter / time.Second); {
	case seconds <= 0:
	case seconds == 1:
		wait = "wait 1 second"
	case seconds < 60:
		wait = fmt.Sprintf("wait %d seconds", seconds)
	default:
		minutes := (seconds + 59) / 60
		wait = fmt.Sprintf("wait %d minute", minutes)
		if minutes > 1 {
			wait += "s"
		}
	}
	return fmt.Sprintf("this computer is limiting %s; %s, then %s", e.Attempts, wait, e.Then)
}

// PublicDetails makes the sentence what a person is shown wherever the
// error reaches a caller (errorsx.Detailed).
func (e *RateLimited) PublicDetails() (code, message string) {
	return "rate_limited", e.Error()
}

// retryAfter reads a Retry-After header's whole seconds, the form the
// transport's rate limiter writes; 0 for anything else.
func retryAfter(h http.Header) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
