package trackerapi

import (
	"net/http"
	"time"
)

// Dialect is a tracker's conventions the transport cannot guess. The
// client calls it for every answer, from many goroutines at once.
type Dialect interface {
	// RejectedLogin reports an answer that marks refused credentials
	// whatever its status. Jira sets a header on a 200.
	RejectedLogin(h http.Header) bool
	// Problem reads an error body: the tracker's message and its per-field
	// messages, by field ID. body holds at most the first 64 KiB.
	Problem(status int, body []byte) Problem
	// RateLimited reports an answer that is a rate limit without being a
	// 429, and when it lifts; a zero time takes Retry-After or the
	// backoff. body is nil for a Stream answer below 400.
	RateLimited(status int, h http.Header, body []byte) (until time.Time, limited bool)
}

// Problem is what a dialect read from an error body.
type Problem struct {
	Message string
	Fields  map[string]string // field ID to message
}

// PlainDialect reads no tracker conventions: no login marker, no message
// (a StatusError shows the body), and no limit but a 429.
type PlainDialect struct{}

func (PlainDialect) RejectedLogin(http.Header) bool { return false }
func (PlainDialect) Problem(int, []byte) Problem    { return Problem{} }
func (PlainDialect) RateLimited(int, http.Header, []byte) (time.Time, bool) {
	return time.Time{}, false
}
