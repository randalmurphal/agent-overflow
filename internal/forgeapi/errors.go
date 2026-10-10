package forgeapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SetupError is a forge login problem the user fixes outside the app: the
// forge CLI whose login the transport borrows is missing, or it has no
// token for the host. It is the one setup error type; the message is
// user-facing.
type SetupError struct {
	Forge   string // "github" | "gitlab"
	Binary  string // "gh" | "glab"
	Kind    string // SetupMissing | SetupUnauthenticated
	Message string
	Err     error
}

// The two SetupError kinds.
const (
	SetupMissing         = "missing"
	SetupUnauthenticated = "unauthenticated"
)

func (e *SetupError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *SetupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// MissingCLIError is the SetupError for a forge whose CLI is not
// installed.
func MissingCLIError(forge string, err error) *SetupError {
	switch forge {
	case ForgeGitLab:
		return &SetupError{Forge: forge, Binary: "glab", Kind: SetupMissing, Err: err,
			Message: "GitLab CLI (`glab`) is not installed or not on PATH. Install from https://gitlab.com/gitlab-org/cli and run 'glab auth login' to continue"}
	default:
		return &SetupError{Forge: ForgeGitHub, Binary: "gh", Kind: SetupMissing, Err: err,
			Message: "GitHub CLI (`gh`) is not installed or not on PATH. Install from https://cli.github.com and run 'gh auth login' to continue"}
	}
}

// UnauthenticatedError is the SetupError for a forge CLI with no usable
// login for the host.
func UnauthenticatedError(forge string, err error) *SetupError {
	switch forge {
	case ForgeGitLab:
		return &SetupError{Forge: forge, Binary: "glab", Kind: SetupUnauthenticated, Err: err,
			Message: "GitLab CLI (`glab`) is not authenticated. Run 'glab auth login' to continue"}
	default:
		return &SetupError{Forge: ForgeGitHub, Binary: "gh", Kind: SetupUnauthenticated, Err: err,
			Message: "GitHub CLI (`gh`) is not authenticated. Run 'gh auth login' to continue"}
	}
}

// ErrNotFound matches, through errors.Is, a *StatusError whose status is
// 404. Whether a 404 means "gone" or "wrong project" is the caller's call:
// see the error table in docs/architecture/forge-transport.md.
var ErrNotFound = errors.New("forge: not found")

// ErrBodyTooLarge is a response body over the caller's limit (Stream) or
// Options.MaxBodyBytes (Do, JSON, GraphQL).
var ErrBodyTooLarge = errors.New("forge: response body exceeds the limit")

// ErrAbsoluteURL is a Request.Path that is an absolute URL on a request
// that did not set Attachment.
var ErrAbsoluteURL = errors.New("forge: absolute URL on a request that is not an attachment fetch")

// errClosed is a request on a Service after Close.
var errClosed = errors.New("forge: transport is closed")

// maxErrorBody bounds StatusError.Body.
const maxErrorBody = 4 << 10

// StatusError is a forge answer outside 2xx and 304 that is not a rate
// limit, an authentication failure after the one refresh, or a GraphQL
// envelope. URL is redacted (RedactURL); Body holds at most 4 KiB.
type StatusError struct {
	Method     string
	URL        string
	Status     int
	Body       string
	Rate       *RateLimit
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("forge %s %s: HTTP %d", e.Method, e.URL, e.Status)
	if text := http.StatusText(e.Status); text != "" {
		msg += " " + text
	}
	if body := strings.TrimSpace(e.Body); body != "" {
		msg += ": " + body
	}
	return msg
}

// Is makes errors.Is(err, ErrNotFound) true for a 404.
func (e *StatusError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// GraphQLProblem is one entry of a GraphQL envelope's errors.
type GraphQLProblem struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

// GraphQLError is a GraphQL response whose envelope carries errors. Data
// is the envelope's data beside them, possibly partial or null; the
// caller decides what partial data it accepts.
type GraphQLError struct {
	Problems []GraphQLProblem
	Data     json.RawMessage
}

func (e *GraphQLError) Error() string {
	messages := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		messages = append(messages, p.Message)
	}
	return "forge GraphQL: " + strings.Join(messages, "; ")
}

// RateLimitedError is a request the forge refused for its rate limit, or
// one the Service refused before sending because the gate for its pool is
// closed or, for a background request, because the pool's remaining quota
// is under the reserve (Reserve). Until is when the pool may be asked
// again; a poller adds its own 0 to 2s jitter before resuming so panes do
// not fire together.
type RateLimitedError struct {
	Host    string
	Pool    string
	Until   time.Time
	Reserve bool
}

func (e *RateLimitedError) Error() string {
	if e.Reserve {
		return fmt.Sprintf("forge %s rate limit (%s) nearly used up; background requests paused until %s", e.Host, e.Pool, e.Until.Format(time.RFC3339))
	}
	return fmt.Sprintf("forge %s rate limit (%s) reached; requests resume at %s", e.Host, e.Pool, e.Until.Format(time.RFC3339))
}

// TransientError is a failure the next attempt may not repeat: dial, DNS,
// TLS, a timeout, a token handoff that failed for a reason other than a
// missing login (a locked keyring, a CLI that timed out).
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string { return "forge unreachable: " + e.Err.Error() }

func (e *TransientError) Unwrap() error { return e.Err }

// outcome is how one forge response is classified.
type outcome int

const (
	outcomeOK outcome = iota
	outcomeNotModified
	outcomeRateLimited
	outcomeUnauthorized
	outcomeNotFound
	outcomeGraphQL
	outcomeStatus
)

// classified is a response's outcome with the error the caller returns
// for it. outcomeUnauthorized carries the 401 *StatusError; the caller
// refreshes the token once before turning it into a *SetupError.
type classified struct {
	outcome outcome
	err     error
}

// answer is the part of a forge response classification reads.
type answer struct {
	host    string
	pool    string
	method  string
	url     string // redacted
	status  int
	header  http.Header
	body    []byte
	graphQL bool
	now     time.Time
}

// classify sorts a response in the order the design fixes: rate limited,
// 401, 404, GraphQL errors, success. It is the one place that decides what
// a status, header set and body mean.
func classify(a answer) classified {
	rate := parseRateLimit(a.header, a.pool)
	if until, limited := rateLimitedUntil(a, rate); limited {
		return classified{outcome: outcomeRateLimited, err: &RateLimitedError{Host: a.host, Pool: a.pool, Until: until}}
	}
	statusErr := func() *StatusError {
		body := a.body
		if len(body) > maxErrorBody {
			body = body[:maxErrorBody]
		}
		retryAfter, _ := parseRetryAfter(a.header.Get("Retry-After"), a.now)
		return &StatusError{Method: a.method, URL: a.url, Status: a.status, Body: string(body), Rate: rate, RetryAfter: retryAfter}
	}
	switch {
	case a.status == http.StatusUnauthorized:
		return classified{outcome: outcomeUnauthorized, err: statusErr()}
	case a.status == http.StatusNotFound:
		return classified{outcome: outcomeNotFound, err: statusErr()}
	case a.status == http.StatusNotModified:
		return classified{outcome: outcomeNotModified}
	case a.status < 200 || a.status > 299:
		return classified{outcome: outcomeStatus, err: statusErr()}
	}
	if a.graphQL {
		if env, ok := parseGraphQLEnvelope(a.body); ok && len(env.Errors) > 0 {
			return classified{outcome: outcomeGraphQL, err: &GraphQLError{Problems: env.Errors, Data: env.Data}}
		}
	}
	return classified{outcome: outcomeOK}
}

// rateLimitedUntil reports whether a response is a rate-limit refusal and,
// when the forge said, until when. A zero time means the forge gave no
// usable time and the gate's fallback applies.
func rateLimitedUntil(a answer, rate *RateLimit) (time.Time, bool) {
	limited := false
	switch {
	case a.status == http.StatusTooManyRequests:
		limited = true
	case a.status == http.StatusForbidden:
		limited = remainingZero(a.header) || a.header.Get("Retry-After") != "" ||
			bytes.Contains(bytes.ToLower(a.body), []byte("rate limit"))
	case a.graphQL && a.status >= 200 && a.status <= 299:
		if env, ok := parseGraphQLEnvelope(a.body); ok && len(env.Errors) > 0 {
			limited = remainingZero(a.header)
			for _, p := range env.Errors {
				if p.Type == "RATE_LIMITED" {
					limited = true
				}
			}
		}
	}
	if !limited {
		return time.Time{}, false
	}
	if wait, ok := parseRetryAfter(a.header.Get("Retry-After"), a.now); ok {
		return a.now.Add(wait), true
	}
	if rate != nil && rate.Reset.After(a.now) {
		return rate.Reset, true
	}
	return time.Time{}, true
}

func remainingZero(h http.Header) bool {
	for _, name := range []string{"X-RateLimit-Remaining", "RateLimit-Remaining"} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n <= 0 {
				return true
			}
		}
	}
	return false
}

// parseRetryAfter reads a Retry-After header: delay seconds or an HTTP
// date.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		if !at.After(now) {
			return 0, true
		}
		return at.Sub(now), true
	}
	return 0, false
}

type graphQLEnvelope struct {
	Data   json.RawMessage  `json:"data"`
	Errors []GraphQLProblem `json:"errors"`
}

func parseGraphQLEnvelope(body []byte) (graphQLEnvelope, bool) {
	var env graphQLEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return graphQLEnvelope{}, false
	}
	return env, true
}

// RateLimit is one response's view of a quota pool, from its headers.
type RateLimit struct {
	// Pool is the pool the forge named (X-RateLimit-Resource,
	// RateLimit-Name), or the request's pool when it named none.
	Pool      string
	Limit     int
	Remaining int
	Reset     time.Time
}

// parseRateLimit reads GitHub's X-RateLimit-* or GitLab's RateLimit-*
// headers. Nil when the response carries neither a limit nor a remaining
// count.
func parseRateLimit(h http.Header, requestPool string) *RateLimit {
	for _, prefix := range []string{"X-RateLimit-", "RateLimit-"} {
		limit, limitErr := strconv.Atoi(strings.TrimSpace(h.Get(prefix + "Limit")))
		remaining, remainingErr := strconv.Atoi(strings.TrimSpace(h.Get(prefix + "Remaining")))
		if limitErr != nil || remainingErr != nil {
			continue
		}
		rate := &RateLimit{Pool: requestPool, Limit: limit, Remaining: remaining}
		if reset, err := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+"Reset")), 10, 64); err == nil && reset > 0 {
			rate.Reset = time.Unix(reset, 0)
		}
		name := h.Get("X-RateLimit-Resource")
		if prefix == "RateLimit-" {
			name = ""
		}
		if name = strings.TrimSpace(name); name != "" {
			rate.Pool = name
		}
		return rate
	}
	return nil
}
