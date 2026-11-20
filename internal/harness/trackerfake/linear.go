package trackerfake

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/tracker/linear/linearschema"
)

// linearService serves Linear's GraphQL API for every workspace; the API
// key selects the workspace. Every request is checked against Linear's
// public schema with linearschema's checks before it runs, and runs on a
// small executor over the parsed document, so an answer holds exactly
// what the document selects. A field, argument or filter the executor
// does not implement is unhandled, by name.
type linearService struct{}

// Linear's documented limits per user per hour with an API key.
const (
	linearRequestLimit = 2500
	linearPointLimit   = 3_000_000
	linearWindow       = time.Hour
)

// linearRequest is a GraphQL request body.
type linearRequest struct {
	Query         string          `json:"query"`
	OperationName string          `json:"operationName"`
	Variables     json.RawMessage `json:"variables"`
}

func (linearService) authenticate(e *Engine, x *exchange) *answer {
	header := x.r.Header.Get("Authorization")
	bare, bearer := strings.CutPrefix(header, "Bearer ")
	x.secrets = append(x.secrets, header, bare)
	if !bearer {
		if w := e.linearTenant(header); w != nil {
			x.tenant = w.key()
			return nil
		}
	}
	if bearer && e.linearTenant(bare) != nil {
		// Measured: a valid key with a Bearer prefix is an input error.
		a := linearErrors(http.StatusBadRequest, gqlErr{code: "INPUT_ERROR", message: "Bad request - Bearer prefix is not supported with API keys", userError: true})
		return &a
	}
	// Measured: a made-up key and a missing one answer 401.
	a := linearErrors(http.StatusUnauthorized, gqlErr{code: "AUTHENTICATION_ERROR", message: "Authentication required, not authenticated", userError: true})
	return &a
}

func (linearService) route(e *Engine, x *exchange) match {
	if x.r.Method != http.MethodPost || x.r.URL.Path != "/graphql" {
		return match{unhandled: "Linear serves POST /graphql only"}
	}
	var in linearRequest
	if err := decodeStrict(x.body, &in, false); err != nil {
		return match{unhandled: err.Error()}
	}
	x.entry.Operation = in.OperationName
	if len(in.Variables) > 0 {
		x.entry.Variables = in.Variables
		x.entry.Body = nil
	}
	if problems := linearschema.DocumentProblems(in.Query, in.OperationName); len(problems) > 0 {
		return match{unhandled: "the document fails Linear's schema: " + strings.Join(problems, "; ")}
	}
	if problems := linearschema.VariableProblems(in.Query, in.OperationName, in.Variables); len(problems) > 0 {
		return match{unhandled: "the variables fail Linear's schema: " + strings.Join(problems, "; ")}
	}
	w := e.linear[x.tenant]
	return match{name: in.OperationName, run: func() answer { return e.execute(w, in) }}
}

// rateLimited is Linear's 429: a RATELIMITED error, Retry-After and an
// exhausted request budget (documented).
func (linearService) rateLimited(e *Engine, x *exchange, retryAfter int) answer {
	a := linearErrors(http.StatusTooManyRequests, gqlErr{code: "RATELIMITED", message: "Rate limit exceeded", userError: false})
	a.header.Set("Retry-After", strconv.Itoa(retryAfter))
	if w := e.linear[x.tenant]; w != nil {
		reset := strconv.FormatInt(e.now().Add(time.Duration(retryAfter)*time.Second).UnixMilli(), 10)
		a.header.Set("X-Ratelimit-Requests-Limit", strconv.Itoa(linearRequestLimit))
		a.header.Set("X-Ratelimit-Requests-Remaining", "0")
		a.header.Set("X-Ratelimit-Requests-Reset", reset)
	}
	return a
}

func (linearService) unhandledAnswer(detail string) answer {
	return linearErrors(http.StatusNotImplemented, gqlErr{code: "TRACKERFAKE_UNHANDLED", message: "trackerfake: unhandled: " + detail})
}

// routeNames is nil: a Linear fault names an operation, and the executor
// runs any operation the schema admits.
func (linearService) routeNames() []string { return nil }

// gqlErr is one GraphQL error of an answer.
type gqlErr struct {
	message, code string
	// presentable is extensions.userPresentableMessage when it differs
	// from message.
	presentable string
	path        []any
	userError   bool
}

func (g gqlErr) wire() map[string]any {
	presentable := g.presentable
	if presentable == "" {
		presentable = g.message
	}
	out := map[string]any{"message": g.message, "extensions": map[string]any{
		"code": g.code, "userError": g.userError, "userPresentableMessage": presentable, "type": strings.ToLower(strings.ReplaceAll(g.code, "_", " ")),
	}}
	if len(g.path) > 0 {
		out["path"] = g.path
	}
	return out
}

func linearErrors(status int, errs ...gqlErr) answer {
	list := make([]any, len(errs))
	for i, g := range errs {
		list[i] = g.wire()
	}
	return jsonAnswer(status, map[string]any{"errors": list, "data": nil})
}

// spend records a request of cost points in the workspace's hourly
// window and returns the usage headers Linear sends on every answer
// (measured). The cost is one point per object answered plus a catalog
// connection's unfilled page (chargedByResults).
func (w *linearWorkspace) spend(now time.Time, cost int) http.Header {
	if now.Sub(w.usage.start) >= linearWindow {
		w.usage = linearUsage{start: now}
	}
	w.usage.requests++
	w.usage.points += cost
	reset := strconv.FormatInt(w.usage.start.Add(linearWindow).UnixMilli(), 10)
	return http.Header{
		"X-Complexity":                     {strconv.Itoa(cost)},
		"X-Ratelimit-Requests-Limit":       {strconv.Itoa(linearRequestLimit)},
		"X-Ratelimit-Requests-Remaining":   {strconv.Itoa(max(linearRequestLimit-w.usage.requests, 0))},
		"X-Ratelimit-Requests-Reset":       {reset},
		"X-Ratelimit-Complexity-Limit":     {strconv.Itoa(linearPointLimit)},
		"X-Ratelimit-Complexity-Remaining": {strconv.Itoa(max(linearPointLimit-w.usage.points, 0))},
		"X-Ratelimit-Complexity-Reset":     {reset},
	}
}
