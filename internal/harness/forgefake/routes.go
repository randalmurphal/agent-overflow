package forgefake

import (
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

// apiRoute is one `api` endpoint. The endpoint (path plus query, as the
// app passes it) must match pattern in full; flags lists the optional
// `api` flags the handler honors. A flag outside that list is refused,
// because a handler that ignores a flag answers a different question
// than the one asked.
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
	{cli: "gh", path: []string{"pr", "view"}, flags: []flagDef{{long: "repo", short: "R", value: true}, {long: "json", value: true}}, run: ghPRView},
	{cli: "gh", path: []string{"pr", "diff"}, flags: []flagDef{{long: "repo", short: "R", value: true}}, run: ghPRDiff},
	{cli: "gh", path: []string{"pr", "list"}, flags: []flagDef{
		{long: "repo", short: "R", value: true}, {long: "head", short: "H", value: true},
		{long: "state", short: "s", value: true}, {long: "limit", short: "L", value: true}, {long: "json", value: true},
	}, run: ghPRList},
	{cli: "gh", path: []string{"run", "view"}, flags: []flagDef{{long: "repo", short: "R", value: true}, {long: "json", value: true}}, run: ghRunView},
	{cli: "gh", path: []string{"api"}, flags: apiFlags, run: func(e *Engine, c *call) response { return e.api(c, githubAPI) }},
	{cli: "glab", path: []string{"mr", "diff"}, flags: []flagDef{{long: "repo", short: "R", value: true}}, run: glabMRDiff},
	{cli: "glab", path: []string{"api"}, flags: apiFlags, run: func(e *Engine, c *call) response { return e.api(c, gitlabAPI) }},
}

// apiFlags is the `api` flag vocabulary the two CLIs share.
var apiFlags = []flagDef{
	{long: "method", short: "X", value: true},
	{long: "header", short: "H", value: true},
	{long: "raw-field", short: "f", value: true},
	{long: "field", short: "F", value: true},
	{long: "input", value: true},
	{long: "jq", short: "q", value: true},
	{long: "include", short: "i"},
	{long: "allow-escape-sequences"},
	{long: "paginate"},
}

var githubAPI = []apiRoute{
	{name: "gh api user", method: "GET", pattern: regexp.MustCompile(`^user$`), flags: []string{"jq"}, run: ghAPIUser},
	{name: "gh api graphql", method: "POST", pattern: regexp.MustCompile(`^graphql$`), flags: []string{"raw-field"}, run: ghGraphQL},
	{name: "gh api job logs", method: "GET", pattern: regexp.MustCompile(`^repos/([^/]+/[^/]+)/actions/jobs/(\d+)/logs$`), run: ghJobLogs},
	{name: "gh api attachment", method: "GET", pattern: regexp.MustCompile(`^https://.+`), flags: []string{"header", "allow-escape-sequences"}, run: ghAttachment},
}

var gitlabAPI = []apiRoute{
	{name: "glab api merge request", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/merge_requests/(\d+)$`), flags: []string{"include"}, run: glabMR},
	{name: "glab api approvals", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/merge_requests/(\d+)/approvals$`), flags: []string{"include"}, run: glabApprovals},
	{name: "glab api discussions", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/merge_requests/(\d+)/discussions(?:\?(.*))?$`), flags: []string{"include"}, run: glabDiscussions},
	{name: "glab api merge request list", method: "GET", pattern: regexp.MustCompile(`^projects/:fullpath/merge_requests(?:\?(.*))?$`), flags: []string{"include"}, run: glabMRList},
	{name: "glab api pipeline jobs", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/pipelines/(\d+)/jobs(?:\?(.*))?$`), flags: []string{"include"}, run: glabPipelineJobs},
	{name: "glab api job trace", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/jobs/(\d+)/trace$`), flags: []string{"include"}, run: glabJobTrace},
	{name: "glab api upload", method: "GET", pattern: regexp.MustCompile(`^projects/([^/?]+)/uploads/([0-9a-f]{32})/([^/?]+)$`), flags: []string{"include"}, run: glabUpload},
}

// api dispatches `<cli> api <endpoint>`. Callers hold mu.
func (e *Engine) api(c *call, routes []apiRoute) response {
	if len(c.positional) != 1 {
		return unhandled("api takes exactly one endpoint, got %d", len(c.positional))
	}
	endpoint := c.positional[0]
	method := strings.ToUpper(c.flag("method"))
	if method == "" {
		method = "GET"
		if c.has("raw-field") || c.has("field") || c.has("input") {
			method = "POST"
		}
	}
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
		return resp
	}
	return unhandled("no %s api route for %s %s", c.cli, method, endpoint)
}
