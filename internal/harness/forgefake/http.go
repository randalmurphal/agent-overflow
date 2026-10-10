package forgefake

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// maxRequestBody bounds what the fake reads of a request body.
const maxRequestBody = 1 << 20

// ServeHTTP answers the forge API transport an isolated boot's forgeapi
// Service sends (internal/forgeapi, Options.Isolated) from the REST and
// GraphQL route tables. Mounts:
//
//	/github/rest/<endpoint>      GitHub REST (the githubAPI routes)
//	/github/graphql              GitHub GraphQL (githubGraphQL, by operationName)
//	/gitlab/api/v4/<endpoint>    GitLab REST (the gitlabAPI routes)
//	/{forge}/absolute/<host>/<p> https://<host>/<p> (attachments)
//
// The forge host a REST or GraphQL request is for is its Host header,
// compared with a seeded host as given, port included; an attachment's is
// the host in its path. The invocation records that host, and a GraphQL
// request's operation and variables. A request with a token other than
// the fixed one is refused with 401 and recorded as unhandled; so is one
// no route claims (404, with the method and path in the detail). A
// request with no token at all is refused with 401 and not recorded: it
// is not the forge transport's. If-None-Match is a flag only the routes
// that list it implement (conditional); Range has no handler, so a
// request carrying it is unhandled. A pool under a rate limit
// (SetRateLimit) answers as the forge does.
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	inv := Invocation{Via: ViaHTTP, Method: r.Method, Path: path}
	forge, tail, _ := strings.Cut(path, "/")
	mount, rest, _ := strings.Cut(tail, "/")
	inv.Forge = forge

	if r.Header.Get("Authorization") == "" && r.Header.Get("Private-Token") == "" {
		// Not the forge transport, which always presents a token: the
		// isolated boot's dev-server scan probes every listener in its own
		// process tree, this one included. Refused and not recorded, so a
		// probe is not an app forge call.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.Error(w, "forge token required", http.StatusUnauthorized)
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	c, endpoint, reason := e.httpCall(r, forge, mount, rest, body)
	if endpoint != "" {
		inv.Path = endpoint
	}
	inv.Host = c.http.host
	if c.graphQL != nil {
		inv.Operation = c.graphQL.OperationName
		inv.Variables = c.graphQL.Variables
	}
	if mount == "absolute" {
		host, _, _ := strings.Cut(rest, "/")
		inv.Host = strings.ToLower(host)
	}
	switch {
	case readErr != nil:
		reason = "read request body: " + readErr.Error()
	case len(body) > maxRequestBody:
		reason = "request body over 1 MiB"
	}

	e.mu.Lock()
	if e.offline {
		// An unreachable forge sends no reply; the harness drops pooled
		// connections on going offline, so every request lands here.
		inv.Route = "offline"
		inv.Detail = "connection dropped: the forge is offline"
		e.record(inv)
		panic(http.ErrAbortHandler)
	}
	var resp response
	switch {
	case !e.authorized(r):
		resp = response{unhandled: "wrong forge token", status: http.StatusUnauthorized}
	case reason != "":
		resp = unhandled("%s", reason)
	default:
		// An attachment is served from the forge's web host, outside the
		// API quota.
		limit, limited := RateLimit{}, false
		if mount != "absolute" {
			limit, limited = e.rateLimitLocked(c, time.Now())
		}
		switch {
		case limited && limit.Remaining == 0:
			resp = limit.refusal(time.Now())
		case c.graphQL != nil:
			resp = e.graphQL(c)
		default:
			routes := githubAPI
			if c.cli == "glab" {
				routes = gitlabAPI
			}
			resp = e.api(c, routes)
		}
		if limited && limit.Remaining > 0 && resp.unhandled == "" {
			resp = limit.decorate(resp)
		}
	}
	if resp.unhandled != "" && resp.status == 0 {
		resp.status = http.StatusNotFound
	}
	status, header, out := renderHTTP(resp)
	inv.Route = resp.route
	inv.Unhandled = resp.unhandled != ""
	inv.Status = status
	switch {
	case inv.Unhandled:
		inv.Detail = fmt.Sprintf("ao-mockforge: unhandled http request (%s): %s %s. Add a handler in internal/harness/forgefake (see its AGENTS.md).", resp.unhandled, r.Method, inv.Path)
		out = []byte(inv.Detail + "\n")
		header = http.Header{"Content-Type": {"text/plain; charset=utf-8"}}
	case status >= 400:
		inv.Detail = strings.TrimSpace(resp.stderr)
	}
	e.record(inv)

	for name, values := range header {
		w.Header()[name] = values
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// httpCall maps a request onto the `api` call it stands for. It returns
// the endpoint the invocation records and, for a request the fake cannot
// map, the reason it is unhandled.
func (e *Engine) httpCall(r *http.Request, forge, mount, rest string, body []byte) (*call, string, string) {
	c := &call{http: &httpCall{host: strings.ToLower(r.Host)}, flags: map[string][]string{"method": {r.Method}}}
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		c.http.base = "http://" + addr.String()
	}
	switch forge {
	case "github":
		c.cli = "gh"
	case "gitlab":
		c.cli = "glab"
	default:
		return c, "", "no forge mount " + forge
	}
	if r.Header.Get("Range") != "" {
		return c, "", "header Range is not implemented"
	}
	if tag := r.Header.Get("If-None-Match"); tag != "" {
		// A route that lists if-none-match answers it; any other refuses
		// it as a flag it does not implement.
		c.flags["if-none-match"] = []string{tag}
	}
	query := ""
	if r.URL.RawQuery != "" {
		query = "?" + r.URL.RawQuery
	}
	var endpoint, recorded string
	switch {
	case forge == "github" && mount == "rest" && rest != "":
		endpoint = rest + query
		c.flags["hostname"] = []string{c.http.host}
	case forge == "github" && mount == "graphql" && rest == "":
		if r.Method != http.MethodPost || query != "" {
			return c, "graphql", "GraphQL is a POST with no query string"
		}
		var req graphQLRequest
		if err := json.Unmarshal(body, &req); err != nil || req.Query == "" || req.OperationName == "" {
			return c, "graphql", "GraphQL body is not {\"query\", \"operationName\", \"variables\"}"
		}
		if len(req.Variables) > 0 && string(req.Variables) != "null" {
			if err := json.Unmarshal(req.Variables, &req.vars); err != nil {
				return c, "graphql", "GraphQL variables are not an object"
			}
		}
		c.graphQL = &req
		return c, "graphql", ""
	case forge == "gitlab" && mount == "api" && strings.HasPrefix(rest, "v4/") && len(rest) > len("v4/"):
		endpoint = strings.TrimPrefix(rest, "v4/") + query
		c.flags["hostname"] = []string{c.http.host}
	case mount == "absolute" && strings.Contains(rest, "/"):
		// The attachment's own URL; its host is in the path, not the Host
		// header, and its query (a signature) is not recorded.
		endpoint = "https://" + rest + query
		recorded = "https://" + rest
		c.http.host = ""
	default:
		return c, "", "no mount for /" + strings.Join([]string{forge, mount, rest}, "/")
	}
	if recorded == "" {
		recorded = endpoint
	}
	c.positional = []string{endpoint}
	return c, recorded, ""
}

// authorized reports whether r carries the fixed token in either header
// form. Callers hold mu.
func (e *Engine) authorized(r *http.Request) bool {
	want := e.opts.APIToken
	if want == "" {
		return false
	}
	got := r.Header.Get("Private-Token")
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		got = bearer
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// renderHTTP turns a handler's answer into a status, headers and body.
func renderHTTP(resp response) (int, http.Header, []byte) {
	status := resp.status
	if status == 0 {
		status = http.StatusOK
		if resp.exit != 0 {
			status = http.StatusInternalServerError
		}
	}
	header := resp.header.Clone()
	if header == nil {
		header = http.Header{}
	}
	body := resp.stdout
	if status >= 400 && len(body) == 0 {
		message, _ := json.Marshal(map[string]string{"message": strings.TrimSpace(resp.stderr)})
		body = message
		header.Set("Content-Type", "application/json")
	}
	return status, header, body
}
