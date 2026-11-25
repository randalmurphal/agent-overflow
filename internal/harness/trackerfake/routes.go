package trackerfake

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// route is one REST route of a tracker: its method and path pattern from
// the origin's root ({name} matches one segment), the query parameters it
// implements (repeat names those it takes more than once), whether an
// anonymous request gets its not-found answer rather than a 401 (Jira),
// whether it only reads, and its handler. A route that does not set
// reads is a write, and its handler runs on a saved copy of the tenant
// (txn.go); TestReadRoutesChangeNothing drives every one that sets it. A
// route whose requests read or write by what they carry (one GraphQL
// endpoint) sets readsWhen instead, which reports a request body that
// only reads.
type route[C any] struct {
	method, pattern string
	query           []string
	repeat          []string
	anonymous404    bool
	reads           bool
	readsWhen       func(body []byte) bool
	handle          func(c C) answer
}

func (r route[C]) name() string { return r.method + " " + r.pattern }

// readsFor reports whether the route's request x only reads.
func (r route[C]) readsFor(x *exchange) bool {
	if r.readsWhen != nil {
		return r.readsWhen(x.body)
	}
	return r.reads
}

// findRoute matches x to the route with the most literal segments.
func findRoute[C any](routes []route[C], x *exchange) (route[C], map[string]string, bool) {
	segs := strings.Split(strings.TrimPrefix(x.r.URL.EscapedPath(), "/"), "/")
	var best route[C]
	var bestParams map[string]string
	bestScore := -1
	for _, r := range routes {
		if r.method != x.r.Method {
			continue
		}
		params, score, ok := matchPattern(r.pattern, segs)
		if ok && score > bestScore {
			best, bestParams, bestScore = r, params, score
		}
	}
	return best, bestParams, bestScore >= 0
}

// matchPattern matches path segments to a pattern; score counts its
// literal segments.
func matchPattern(pattern string, segs []string) (map[string]string, int, bool) {
	parts := strings.Split(pattern, "/")
	if len(parts) != len(segs) {
		return nil, 0, false
	}
	params := map[string]string{}
	score := 0
	for i, p := range parts {
		if name, ok := strings.CutPrefix(p, "{"); ok {
			v, err := url.PathUnescape(segs[i])
			if err != nil || v == "" {
				return nil, 0, false
			}
			params[strings.TrimSuffix(name, "}")] = v
			continue
		}
		if p != segs[i] {
			return nil, 0, false
		}
		score++
	}
	return params, score, true
}

// checkQuery names a query parameter r does not implement, or one
// repeated that it takes once.
func (r route[C]) checkQuery(q url.Values) string {
	for _, key := range slices.Sorted(maps.Keys(q)) {
		switch {
		case !slices.Contains(r.query, key):
			return fmt.Sprintf("query parameter %q is not implemented on %s", key, r.name())
		case len(q[key]) > 1 && !slices.Contains(r.repeat, key):
			return fmt.Sprintf("query parameter %q is repeated on %s", key, r.name())
		}
	}
	return ""
}

func routeNames[C any](routes []route[C]) []string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = r.name()
	}
	return out
}
