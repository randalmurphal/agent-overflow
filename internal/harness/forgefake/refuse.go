package forgefake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Refusal makes one route answer Status, Header and Body in place of its
// handler, so a spec can drive the app's handling of a forge refusal the
// fixture cannot cause: a token without a scope (403 with GitLab's
// insufficient_scope body, or GitHub's X-Accepted-OAuth-Scopes beside an
// X-OAuth-Scopes that grants none of them), a login the forge refuses
// (401), a merge it will not take (405, 422), a head that moved (409). Route is a REST route's name (`glab api
// approve`) or a GraphQL operation's (`gh graphql MergePR`); a GraphQL
// route may also answer 200 with a body carrying `errors`, which is how
// GitHub refuses a mutation. Times is how many requests it refuses, 0
// until cleared. A refused request is answered after its route matched
// and before its handler runs, so the fixture does not change, and is
// recorded under its route. A held refusal (Hold) is recorded at once and
// answered only once ReleaseRefusal names its route, so a spec can act
// while the app's request is in flight; a release that drops instead
// closes the request with no reply, the answer lost.
type Refusal struct {
	Route  string            `json:"route"`
	Status int               `json:"status"`
	Header map[string]string `json:"header,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
	Times  int               `json:"times,omitempty"`
	Hold   bool              `json:"hold,omitempty"`
}

// refusalStatuses are the statuses a refusal may answer.
var refusalStatuses = map[int]bool{
	http.StatusUnauthorized: true, http.StatusForbidden: true, http.StatusMethodNotAllowed: true,
	http.StatusConflict: true, http.StatusUnprocessableEntity: true,
}

// SetRefusal arms refusal on its route, replacing any it had; a Status of
// 0 clears the route's refusal. Reset clears every refusal.
func (e *Engine) SetRefusal(refusal Refusal) error {
	graphQL, known := refusableRoute(refusal.Route)
	if !known {
		return fmt.Errorf("forge refusal: no route %q (a REST route name from routes.go, \"gh graphql <operation>\" or \"glab graphql <operation>\")", refusal.Route)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if refusal.Status == 0 {
		delete(e.refusals, refusal.Route)
		return nil
	}
	switch {
	case refusal.Times < 0:
		return fmt.Errorf("forge refusal: times %d is negative", refusal.Times)
	case refusalStatuses[refusal.Status]:
	case graphQL && refusal.Status == http.StatusOK:
		var body struct {
			Errors []json.RawMessage `json:"errors"`
		}
		if err := json.Unmarshal(refusal.Body, &body); err != nil || len(body.Errors) == 0 {
			return fmt.Errorf("forge refusal: a 200 on %s is a GraphQL refusal and its body carries errors", refusal.Route)
		}
	default:
		return fmt.Errorf("forge refusal: status %d is not 401, 403, 405, 409 or 422 (or 200 with errors on a GraphQL route)", refusal.Status)
	}
	if len(refusal.Body) > 0 && !json.Valid(refusal.Body) {
		return fmt.Errorf("forge refusal: body is not JSON")
	}
	for name := range refusal.Header {
		if http.CanonicalHeaderKey(name) == "Content-Type" {
			return fmt.Errorf("forge refusal: the body is JSON; Content-Type is not a header to set")
		}
	}
	if e.refusals == nil {
		e.refusals = make(map[string]*Refusal)
	}
	stored := refusal
	stored.Body = bytes.Clone(refusal.Body)
	e.refusals[refusal.Route] = &stored
	if refusal.Hold && e.holds[refusal.Route] == nil {
		if e.holds == nil {
			e.holds = make(map[string]*heldAnswer)
		}
		e.holds[refusal.Route] = &heldAnswer{done: make(chan struct{})}
	}
	return nil
}

// heldAnswer is a held refusal's gate: done closes on release, and drop
// (set before) says the requests it held lose their answer.
type heldAnswer struct {
	done chan struct{}
	drop bool
}

// Release ends a held refusal (HarnessForgeRelease): its requests are
// answered with the refusal, or with Drop closed with no reply.
type Release struct {
	Route string `json:"route"`
	Drop  bool   `json:"drop,omitempty"`
}

// ReleaseRefusal ends the held refusal on release.Route for every request
// it is holding and those it holds from now on; one armed again with Hold
// holds anew. Reset releases every route, answering.
func (e *Engine) ReleaseRefusal(release Release) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.holds[release.Route]
	if held == nil {
		return fmt.Errorf("forge refusal: %q holds no answer", release.Route)
	}
	held.drop = release.Drop
	close(held.done)
	delete(e.holds, release.Route)
	return nil
}

// refusableRoute reports whether route names a REST route or a GraphQL
// operation, and which.
func refusableRoute(route string) (graphQL, known bool) {
	if op, ok := strings.CutPrefix(route, "gh graphql "); ok {
		return true, slices.ContainsFunc(githubGraphQL, func(o graphQLOp) bool { return o.name == op })
	}
	if op, ok := strings.CutPrefix(route, "glab graphql "); ok {
		return true, slices.ContainsFunc(gitlabGraphQL, func(o graphQLOp) bool { return o.name == op })
	}
	for _, routes := range [][]apiRoute{githubAPI, gitlabAPI} {
		if slices.ContainsFunc(routes, func(r apiRoute) bool { return r.name == route }) {
			return false, true
		}
	}
	return false, false
}

// refusal answers route's armed refusal, counting it down, or reports
// none. Callers hold mu.
func (e *Engine) refusal(route string) (response, bool) {
	armed := e.refusals[route]
	if armed == nil {
		return response{}, false
	}
	if armed.Times > 0 {
		armed.Times--
		if armed.Times == 0 {
			delete(e.refusals, route)
		}
	}
	header := http.Header{"Content-Type": {"application/json; charset=utf-8"}}
	for name, value := range armed.Header {
		header.Set(name, value)
	}
	body := bytes.Clone(armed.Body)
	resp := response{route: route, status: armed.Status, header: header, stdout: body, stderr: fmt.Sprintf("refused (HarnessForgeRefuse): %d %s", armed.Status, body)}
	if armed.Hold {
		resp.held = e.holds[route]
	}
	return resp, true
}
