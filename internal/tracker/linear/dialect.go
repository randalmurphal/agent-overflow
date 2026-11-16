package linear

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// Error codes Linear sets in a GraphQL error's extensions.code.
const (
	CodeAuthentication = "AUTHENTICATION_ERROR"
	// CodeInput is a refused input. A key sent with a "Bearer " prefix
	// is also answered with it, with HTTP 400, so it is not proof that
	// the document or its variables are wrong.
	CodeInput       = "INPUT_ERROR"
	CodeRateLimited = "RATELIMITED"
)

// dialect reads Linear's conventions for the transport. Linear answers a
// rate limit with HTTP 429, a RATELIMITED error and Retry-After
// (https://linear.app/developers/rate-limiting); a RATELIMITED error under
// any other status is a limit too.
type dialect struct{}

func (dialect) RejectedLogin(http.Header) bool { return false }

// Problem joins the GraphQL errors of an error body into one message.
func (dialect) Problem(_ int, body []byte) trackerapi.Problem {
	errs, ok := parseErrors(body)
	if !ok || len(errs) == 0 {
		return trackerapi.Problem{}
	}
	return trackerapi.Problem{Message: joinErrors(typedErrors(errs, unscrubbed))}
}

// RateLimited reports a RATELIMITED answer. When the answer names no
// Retry-After (which the transport reads itself), the limit lifts at the
// reset of the endpoint limit, else at the latest reset of an exhausted
// budget; with neither, the transport backs off.
func (dialect) RateLimited(status int, h http.Header, body []byte) (time.Time, bool) {
	limited := status == http.StatusTooManyRequests
	if !limited && bytes.Contains(body, []byte(CodeRateLimited)) {
		errs, _ := parseErrors(body)
		for _, e := range errs {
			if e.Extensions.Code == CodeRateLimited {
				limited = true
				break
			}
		}
	}
	if !limited || strings.TrimSpace(h.Get("Retry-After")) != "" {
		return time.Time{}, limited
	}
	if t, ok := headerMillis(h, "X-Ratelimit-Endpoint-Requests-Reset"); ok {
		return t, true
	}
	var until time.Time
	for _, b := range []Budget{budgetOf(h, "X-Ratelimit-Requests-"), budgetOf(h, "X-Ratelimit-Complexity-")} {
		if b.Known && b.Remaining <= 0 && b.Reset.After(until) {
			until = b.Reset
		}
	}
	return until, true
}

// parseErrors reads the errors member of a GraphQL answer; false when
// body is not a GraphQL answer.
func parseErrors(body []byte) ([]wireError, bool) {
	var env struct {
		Errors []wireError `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false
	}
	return env.Errors, true
}

// unscrubbed passes text through: Problem's message goes into the
// transport's errors, which the transport scrubs itself.
func unscrubbed(text string) string { return text }

func typedErrors(wire []wireError, scrub func(string) string) []GraphQLError {
	out := make([]GraphQLError, 0, len(wire))
	for _, w := range wire {
		out = append(out, w.typed(scrub))
	}
	return out
}

func joinErrors(errs []GraphQLError) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.String())
	}
	return strings.Join(parts, "; ")
}
