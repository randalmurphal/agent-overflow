package forgefake

import (
	"fmt"
	"strconv"
	"strings"
	"time"
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

// ghPRComment answers `gh pr comment N -R R --body-file -`: a
// conversation comment by the viewer, printed as its URL.
func ghPRComment(e *Engine, c *call) response {
	if c.flag("body-file") != "-" {
		return unhandled("pr comment reads its body only from --body-file -")
	}
	r, p, fail, ok := e.ghRepoPull(c)
	if !ok {
		return fail
	}
	body := string(c.stdin)
	if strings.TrimSpace(body) == "" {
		return response{exit: 1, stderr: "body cannot be blank\n"}
	}
	comment := Comment{ID: e.ids.next(), Author: e.viewer, Body: body, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	p.Comments = append(p.Comments, comment)
	return response{stdout: []byte(fmt.Sprintf("%s#issuecomment-%d\n", githubPullURL(r, p), comment.ID))}
}

// ghPRMerge answers `gh pr merge N -R R --squash|--merge|--rebase
// [--auto]`. A pull the forge would refuse fails as gh does; with --auto
// a pull whose checks have not passed is armed instead of merged.
func ghPRMerge(e *Engine, c *call) response {
	methods := 0
	for _, flag := range []string{"squash", "merge", "rebase"} {
		if c.has(flag) {
			methods++
		}
	}
	if methods != 1 {
		return response{exit: 1, stderr: "--merge, --rebase, or --squash required when not running interactively\n"}
	}
	r, p, fail, ok := e.ghRepoPull(c)
	if !ok {
		return fail
	}
	ref := fmt.Sprintf("%s#%d", r.Project, p.Number)
	switch {
	case p.State == "merged":
		return response{exit: 1, stderr: fmt.Sprintf("X Pull request %s was already merged\n", ref)}
	case p.State == "closed":
		return response{exit: 1, stderr: fmt.Sprintf("X Pull request %s is closed\n", ref)}
	case p.Mergeable == "conflicts":
		return response{exit: 1, stderr: fmt.Sprintf("X Pull request %s is not mergeable: the merge commit cannot be cleanly created.\n", ref)}
	case c.has("auto") && (p.ciBlocks() || p.Mergeable == "checking"):
		p.AutoMerge = true
		return response{stderr: fmt.Sprintf("✓ Pull request %s will be automatically merged when all requirements are met\n", ref)}
	case githubMergeStateStatus(p) != "CLEAN":
		return response{exit: 1, stderr: fmt.Sprintf("X Pull request %s is not mergeable: the base branch policy prohibits the merge.\n", ref)}
	}
	p.State, p.AutoMerge, p.MergedAt = "merged", false, time.Now().UTC().Format(time.RFC3339)
	return response{stderr: fmt.Sprintf("✓ Merged pull request %s (%s)\n", ref, p.Title)}
}

// ghPREdit answers `gh pr edit N -R R --base B`.
func ghPREdit(e *Engine, c *call) response {
	base := c.flag("base")
	if base == "" {
		return unhandled("pr edit implements only --base")
	}
	r, p, fail, ok := e.ghRepoPull(c)
	if !ok {
		return fail
	}
	if p.State != "open" {
		return response{exit: 1, stderr: "GraphQL: Cannot change the base branch of a closed pull request. (updatePullRequest)\n"}
	}
	p.BaseRef = base
	return response{stdout: []byte(githubPullURL(r, p) + "\n")}
}

// ghRuns answers the REST workflow runs of a branch, newest first: the
// runs of the pulls whose head is that branch. Paginated like ghRunJobs.
func ghRuns(e *Engine, c *call, m []string) response {
	query, perPage, page, err := glabPage(m[2], "branch")
	if err != nil {
		return unhandled("%v", err)
	}
	if query.Get("per_page") == "" {
		perPage = 30
	}
	r := e.repoOn("github", c.http.host, m[1])
	if r == nil {
		return ghHTTPNotFound()
	}
	runs := []map[string]any{}
	for i := len(r.Pulls) - 1; i >= 0; i-- {
		p := &r.Pulls[i]
		if p.CI == nil || (query.Has("branch") && p.HeadRef != query.Get("branch")) {
			continue
		}
		status, conclusion := githubRESTState(p.CI.Status)
		runs = append(runs, map[string]any{
			"id":          p.CI.ID,
			"name":        p.CI.Name,
			"head_branch": p.HeadRef,
			"head_sha":    p.HeadSHA,
			"status":      status,
			"conclusion":  conclusion,
			"html_url":    fmt.Sprintf("https://%s/%s/actions/runs/%d", r.Host, r.Project, p.CI.ID),
		})
	}
	items, next := paginate(runs, perPage, page)
	resp := jsonResponse(map[string]any{"total_count": len(runs), "workflow_runs": items})
	if next != "" {
		query.Set("per_page", strconv.Itoa(perPage))
		query.Set("page", next)
		link := fmt.Sprintf("%s/github/rest/repos/%s/actions/runs?%s", c.http.base, r.Project, query.Encode())
		resp.header.Set("Link", "<"+link+`>; rel="next"`)
	}
	return resp
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

// ghForgeComments: one page of the pull's conversation comments, oldest
// first.
func ghForgeComments(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	offset, err1 := v.cursor("endCursor")
	if err := firstErr(err, err1); err != nil {
		return unhandled("ForgeComments: %v", err)
	}
	r, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	comments := make([]map[string]any, 0, len(p.Comments))
	for _, comment := range p.Comments {
		comments = append(comments, map[string]any{
			"databaseId": comment.ID,
			"author":     map[string]any{"login": comment.Author},
			"body":       comment.Body,
			"createdAt":  comment.CreatedAt,
			"url":        fmt.Sprintf("%s#issuecomment-%d", githubPullURL(r, p), comment.ID),
		})
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"comments": graphQLConnection(comments, offset, githubGraphQLPageSize),
	}}})
}

// ghForgeReply adds the viewer's reply to a review thread
// (addPullRequestReviewThreadReply) and answers the comment's databaseId
// and url.
func ghForgeReply(e *Engine, c *call, v graphQLVars) response {
	id, err := v.str("threadID")
	body, err1 := v.str("body")
	if err := firstErr(err, err1); err != nil {
		return unhandled("ForgeReply: %v", err)
	}
	thread := e.githubThread(c, id)
	if thread == nil {
		return graphQLNotFound(map[string]any{"addPullRequestReviewThreadReply": nil}, []string{"addPullRequestReviewThreadReply"}, fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id))
	}
	if strings.TrimSpace(body) == "" {
		return graphQLNotFound(map[string]any{"addPullRequestReviewThreadReply": nil}, []string{"addPullRequestReviewThreadReply"}, "Body can't be blank")
	}
	comment := Comment{ID: e.ids.next(), Author: e.viewer, Body: body, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	thread.Comments = append(thread.Comments, comment)
	return graphQLData(map[string]any{"addPullRequestReviewThreadReply": map[string]any{"comment": map[string]any{
		"databaseId": comment.ID,
		"url":        fmt.Sprintf("https://%s#discussion_r%d", c.http.host, comment.ID),
	}}})
}
