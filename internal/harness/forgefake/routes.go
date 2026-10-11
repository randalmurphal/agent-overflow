package forgefake

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// command is one CLI subcommand the fake implements.
type command struct {
	cli   string
	path  []string
	flags []flagDef
	run   func(e *Engine, c *call) response
}

// apiRoute is one REST endpoint. The endpoint (path plus query relative
// to the forge's REST base, as the app sends it) must match pattern in
// full; flags lists the request properties httpCall synthesizes as flags
// that the handler honors. A flag outside that list is refused, because a
// handler that ignores one answers a different question than the one
// asked. A route that lists if-none-match answers its 200s with an ETag
// and a request naming the current one with 304 (conditional); only the
// endpoints whose real counterparts send an ETag the app revalidates list
// it.
type apiRoute struct {
	name    string
	method  string
	pattern *regexp.Regexp
	flags   []string
	run     func(e *Engine, c *call, match []string) response
}

// Every invocation the fake answers. The report of which app calls have
// handlers lives in AGENTS.md; keep the two in step.
var commands = []command{
	{cli: "ssh", flags: []flagDef{{long: "config", short: "G"}, {long: "option", short: "o", value: true}, {long: "user", short: "l", value: true}}, run: repositorySSHConfig},
	{cli: "gh", path: []string{"pr", "create"}, flags: []flagDef{
		{long: "title", short: "t", value: true}, {long: "body", short: "b", value: true},
		{long: "base", short: "B", value: true}, {long: "draft", short: "d"},
	}, run: ghPRCreate},
	{cli: "glab", path: []string{"mr", "create"}, flags: []flagDef{
		{long: "title", short: "t", value: true}, {long: "description", short: "d", value: true},
		{long: "target-branch", short: "b", value: true}, {long: "draft"},
		{long: "yes", short: "y"}, {long: "no-editor"},
	}, run: glabMRCreate},
}

// githubAPI is GitHub's REST surface, served over HTTP only; GitHub's
// GraphQL operations are githubGraphQL.
var githubAPI = []apiRoute{
	{name: "gh repository identity", method: "GET", pattern: regexp.MustCompile(`^repos/([^/]+/[^/]+)$`), flags: []string{"hostname"}, run: forgeRepositoryIdentity},
	{name: "gh api run jobs", method: "GET", pattern: regexp.MustCompile(`^repos/([^/]+/[^/]+)/actions/runs/(\d+)/jobs(?:\?(.*))?$`), flags: []string{"hostname", "if-none-match"}, run: ghRunJobs},
	{name: "gh api job logs", method: "GET", pattern: regexp.MustCompile(`^repos/([^/]+/[^/]+)/actions/jobs/(\d+)/logs$`), flags: []string{"hostname", "if-none-match"}, run: ghJobLogs},
	{name: "gh api attachment", method: "GET", pattern: regexp.MustCompile(`^https://.+`), run: ghAttachment},
}

var gitlabAPI = []apiRoute{
	{name: "glab repository identity", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)$`), flags: []string{"hostname"}, run: forgeRepositoryIdentity},
	{name: "glab api merge request", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/merge_requests/(\d+)$`), flags: []string{"hostname", "if-none-match"}, run: glabMR},
	{name: "glab api approvals", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/merge_requests/(\d+)/approvals$`), flags: []string{"hostname"}, run: glabApprovals},
	{name: "glab api discussions", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/merge_requests/(\d+)/discussions(?:\?(.*))?$`), flags: []string{"hostname"}, run: glabDiscussions},
	{name: "glab api merge request list", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/merge_requests(?:\?(.*))?$`), flags: []string{"hostname", "if-none-match"}, run: glabMRList},
	{name: "glab api pipeline jobs", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/pipelines/(\d+)/jobs(?:\?(.*))?$`), flags: []string{"hostname"}, run: glabPipelineJobs},
	{name: "glab api job trace", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/jobs/(\d+)/trace$`), flags: []string{"hostname", "if-none-match"}, run: glabJobTrace},
	{name: "glab api upload", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/uploads/([0-9a-f]{32})/([^/?]+)$`), flags: []string{"hostname"}, run: glabUpload},
}

// api dispatches a REST request, mapped by httpCall onto its endpoint,
// method and host. Callers hold mu.
func (e *Engine) api(c *call, routes []apiRoute) response {
	if len(c.positional) != 1 {
		return unhandled("api takes exactly one endpoint, got %d", len(c.positional))
	}
	endpoint := c.positional[0]
	method := strings.ToUpper(c.flag("method"))
	for _, route := range routes {
		match := route.pattern.FindStringSubmatch(endpoint)
		if match == nil || route.method != method {
			continue
		}
		for name := range c.flags {
			if name != "method" && !slices.Contains(route.flags, name) {
				return unhandled("%s does not implement --%s", route.name, name)
			}
		}
		resp := route.run(e, c, match)
		if resp.route == "" {
			resp.route = route.name
		}
		if slices.Contains(route.flags, "if-none-match") {
			resp = conditional(resp, c.flag("if-none-match"))
		}
		return resp
	}
	return unhandled("no %s api route for %s %s", c.cli, method, endpoint)
}

// conditional gives a route's 200 its ETag, a hash of the body, and
// answers 304 with no body when ifNoneMatch names it. Any other answer
// passes through.
func conditional(resp response, ifNoneMatch string) response {
	if resp.unhandled != "" || (resp.status != 0 && resp.status != http.StatusOK) || resp.exit != 0 {
		return resp
	}
	sum := sha256.Sum256(resp.stdout)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	if ifNoneMatch == etag {
		return response{route: resp.route, status: http.StatusNotModified, header: http.Header{"Etag": {etag}}}
	}
	resp.header = resp.header.Clone()
	if resp.header == nil {
		resp.header = http.Header{}
	}
	resp.header.Set("ETag", etag)
	return resp
}
