package forgefake

import (
	"fmt"
	"strconv"
	"strings"
)

// The gh pull request commands and GitHub operations the pstack forge
// script runs (assets/skills/pstack/skills/poteto-mode/scripts/forge).
// Each names its repository with -R HOST/OWNER/REPO, so a GitHub
// Enterprise host is the seeded host. Output follows gh's non-tty
// printing; a mutation changes the fixture the next read answers from.

// githubPullFields is every `--json` field `gh pr view` and `gh pr list`
// answer. A field outside it is refused rather than omitted.
var githubPullFields = map[string]func(r *Repo, p *Pull) any{
	"number":           func(_ *Repo, p *Pull) any { return p.Number },
	"title":            func(_ *Repo, p *Pull) any { return p.Title },
	"body":             func(_ *Repo, p *Pull) any { return p.Body },
	"url":              func(r *Repo, p *Pull) any { return githubPullURL(r, p) },
	"state":            func(_ *Repo, p *Pull) any { return strings.ToUpper(p.State) },
	"isDraft":          func(_ *Repo, p *Pull) any { return p.Draft },
	"author":           func(_ *Repo, p *Pull) any { return githubCLIAuthor(p.Author, p.AuthorName) },
	"headRefName":      func(_ *Repo, p *Pull) any { return p.HeadRef },
	"headRefOid":       func(_ *Repo, p *Pull) any { return p.HeadSHA },
	"baseRefName":      func(_ *Repo, p *Pull) any { return p.BaseRef },
	"mergeable":        func(_ *Repo, p *Pull) any { return githubMergeable(p) },
	"mergeStateStatus": func(_ *Repo, p *Pull) any { return githubMergeStateStatus(p) },
	"reviewDecision":   func(_ *Repo, p *Pull) any { return githubReviewDecision(p.Reviews) },
	"mergedAt":         func(_ *Repo, p *Pull) any { return nullable(p.MergedAt) },
	"autoMergeRequest": func(_ *Repo, p *Pull) any {
		if !p.AutoMerge {
			return nil
		}
		return map[string]any{"mergeMethod": "SQUASH"}
	},
}

// githubCLIAuthor is gh's author object: login, name ("" without one),
// and is_bot.
func githubCLIAuthor(login, name string) map[string]any {
	return map[string]any{"login": login, "name": name, "is_bot": name == "" && strings.HasSuffix(login, "[bot]")}
}

func selectGitHubFields(list string, r *Repo, p *Pull) (map[string]any, error) {
	if list == "" {
		return nil, fmt.Errorf("--json is required")
	}
	out := make(map[string]any)
	for _, field := range strings.Split(list, ",") {
		value, ok := githubPullFields[field]
		if !ok {
			return nil, fmt.Errorf("--json field %q is not implemented", field)
		}
		out[field] = value(r, p)
	}
	return out, nil
}

// ghRepo resolves -R HOST/OWNER/REPO (OWNER/REPO means github.com).
func (e *Engine) ghRepo(c *call) (*Repo, response, bool) {
	spec := c.flag("repo")
	parts := strings.Split(spec, "/")
	host := "github.com"
	switch len(parts) {
	case 2:
	case 3:
		host, parts = strings.ToLower(parts[0]), parts[1:]
	default:
		return nil, unhandled("expected -R [HOST/]OWNER/REPO, got %q", spec), false
	}
	project := strings.Join(parts, "/")
	r := e.repoOn("github", host, project)
	if r == nil {
		return nil, response{exit: 1, stderr: fmt.Sprintf("GraphQL: Could not resolve to a Repository with the name '%s'. (repository)\n", project)}, false
	}
	return r, response{}, true
}

// ghRepoPull resolves -R and the one PR number positional.
func (e *Engine) ghRepoPull(c *call) (*Repo, *Pull, response, bool) {
	if len(c.positional) != 1 {
		return nil, nil, unhandled("expected one PR number"), false
	}
	number, err := strconv.Atoi(c.positional[0])
	if err != nil || number <= 0 {
		return nil, nil, unhandled("PR number %q is not a positive integer", c.positional[0]), false
	}
	r, fail, ok := e.ghRepo(c)
	if !ok {
		return nil, nil, fail, false
	}
	p := r.pull(number)
	if p == nil {
		return nil, nil, response{exit: 1, stderr: fmt.Sprintf("GraphQL: Could not resolve to a PullRequest with the number of %d. (repository.pullRequest)\n", number)}, false
	}
	return r, p, response{}, true
}

func ghPRView(e *Engine, c *call) response {
	r, p, fail, ok := e.ghRepoPull(c)
	if !ok {
		return fail
	}
	out, err := selectGitHubFields(c.flag("json"), r, p)
	if err != nil {
		return unhandled("%v", err)
	}
	return jsonResponse(out)
}

// ghPRList answers `gh pr list -R R [--state S] [--head B] [--author A]
// [--limit N] --json F`, newest first. --author @me is the viewer.
func ghPRList(e *Engine, c *call) response {
	if len(c.positional) != 0 {
		return unhandled("pr list takes no positional arguments")
	}
	r, fail, ok := e.ghRepo(c)
	if !ok {
		return fail
	}
	state := defaultString(c.flag("state"), "open")
	if state != "open" && state != "closed" && state != "merged" && state != "all" {
		return unhandled("--state %q is not open, closed, merged or all", state)
	}
	limit := 30
	if raw := c.flag("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return unhandled("--limit %q is not a positive integer", raw)
		}
		limit = n
	}
	author := c.flag("author")
	if author == "@me" {
		author = e.viewer
	}
	out := []map[string]any{}
	for i := len(r.Pulls) - 1; i >= 0 && len(out) < limit; i-- {
		p := &r.Pulls[i]
		if (state != "all" && p.State != state) || (c.has("head") && p.HeadRef != c.flag("head")) || (author != "" && p.Author != author) {
			continue
		}
		fields, err := selectGitHubFields(c.flag("json"), r, p)
		if err != nil {
			return unhandled("%v", err)
		}
		out = append(out, fields)
	}
	return jsonResponse(out)
}

// The forge script's GraphQL operations. Each answers exactly the
// selection of the script's document.

// ghForgeChecks: the head commit's oid and rollup state with one page of
// its contexts (CheckRun databaseId, name, status, conclusion,
// detailsUrl).
func ghForgeChecks(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	offset, err1 := v.cursor("endCursor")
	if err := firstErr(err, err1); err != nil {
		return unhandled("ForgeChecks: %v", err)
	}
	r, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	var rollup any
	if p.CI != nil && len(p.CI.Jobs) > 0 {
		contexts := make([]map[string]any, 0, len(p.CI.Jobs))
		for _, job := range p.CI.Jobs {
			status, conclusion := githubCheckState(job.Status)
			contexts = append(contexts, map[string]any{
				"__typename": "CheckRun",
				"databaseId": job.ID,
				"name":       job.Name,
				"status":     status,
				"conclusion": nullable(conclusion),
				"detailsUrl": githubJobURL(r, p.CI, job),
			})
		}
		rollup = map[string]any{"state": githubRollupState(p.CI), "contexts": graphQLConnection(contexts, offset, githubGraphQLPageSize)}
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"commits": map[string]any{"nodes": []map[string]any{{"commit": map[string]any{"oid": p.HeadSHA, "statusCheckRollup": rollup}}}},
	}}})
}

// githubRollupState is a commit's StatusCheckRollup state.
func githubRollupState(ci *Pipeline) string {
	switch ci.Status {
	case "failed":
		return "FAILURE"
	case "running", "pending":
		return "PENDING"
	}
	return "SUCCESS"
}

// ghForgeCommits: the pull's commits with their rollup state. The fixture
// knows the head commit only.
func ghForgeCommits(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	if err != nil {
		return unhandled("ForgeCommits: %v", err)
	}
	_, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	var rollup any
	if p.CI != nil {
		rollup = map[string]any{"state": githubRollupState(p.CI)}
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"commits": map[string]any{"nodes": []map[string]any{{"commit": map[string]any{"oid": p.HeadSHA, "statusCheckRollup": rollup}}}},
	}}})
}

// ghForgeThreads: one page of review threads, each with its comments
// (databaseId, author login, body, createdAt) and their total count.
func ghForgeThreads(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	offset, err1 := v.cursor("endCursor")
	if err := firstErr(err, err1); err != nil {
		return unhandled("ForgeThreads: %v", err)
	}
	_, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	threads := make([]map[string]any, 0, len(p.Threads))
	for i := range p.Threads {
		thread := &p.Threads[i]
		var line any
		if thread.Line != nil && !thread.Outdated {
			line = *thread.Line
		}
		comments := make([]map[string]any, 0, len(thread.Comments))
		for _, comment := range thread.Comments[:min(len(thread.Comments), githubGraphQLPageSize)] {
			comments = append(comments, map[string]any{
				"databaseId": comment.ID,
				"author":     map[string]any{"login": comment.Author},
				"body":       comment.Body,
				"createdAt":  comment.CreatedAt,
			})
		}
		threads = append(threads, map[string]any{
			"id":         thread.ID,
			"isResolved": thread.Resolved,
			"isOutdated": thread.Outdated,
			"path":       thread.Path,
			"line":       line,
			"comments":   map[string]any{"totalCount": len(thread.Comments), "nodes": comments},
		})
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"reviewThreads": graphQLConnection(threads, offset, githubGraphQLPageSize),
	}}})
}
