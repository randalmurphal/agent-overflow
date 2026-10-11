package forgefake

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// GitHub GraphQL. A request is dispatched on its operationName to the one
// handler that implements that operation; the handler reads the
// operation's variables, never the query text, and honors the @include
// and @skip variables the app's documents gate parts with. The answers
// carry the node shapes the real API returns for the app's selections:
// every connection has pageInfo, an absent time is null, and an actor
// without a display name answers as a bot, whose author has no name key.
// A cursor is "cursor:<offset>".

// graphQLOp is one operation the fake implements: its kind and name as
// the document declares them, and its variables. The document's
// declared variables must be exactly required plus optional, and a
// request must supply every required one; anything else is a drifted
// operation and is unhandled.
type graphQLOp struct {
	kind     string
	name     string
	required []string
	optional []string
	run      func(e *Engine, c *call, v graphQLVars) response
}

// githubGraphQLPageSize is the page size of every connection the app's
// documents select.
const githubGraphQLPageSize = 100

var githubGraphQL = []graphQLOp{
	{kind: "query", name: "PRTick", required: []string{"owner", "name", "number", "wantDetail", "wantThreads", "wantChecks"}, run: ghPRTick},
	{kind: "query", name: "PRThreadsPage", required: []string{"owner", "name", "number", "after"}, run: ghPRThreadsPage},
	{kind: "query", name: "PRCommentsPage", required: []string{"owner", "name", "number", "after"}, run: ghPRCommentsPage},
	{kind: "query", name: "ThreadCommentsPage", required: []string{"threadID", "after"}, run: ghThreadCommentsPage},
	{kind: "query", name: "RollupContextsPage", required: []string{"commitID", "after"}, run: ghRollupContextsPage},
	{kind: "mutation", name: "SetThreadResolved", required: []string{"threadID", "resolved"}, run: ghSetThreadResolved},
	{kind: "query", name: "OpenPRsByHead", required: []string{"owner", "name", "head"}, run: ghOpenPRsByHead},
	{kind: "query", name: "MergedPRs", required: []string{"owner", "name", "first"}, optional: []string{"after"}, run: ghMergedPRs},
}

// graphQLRequest is the body of a POST to /github/graphql.
type graphQLRequest struct {
	Query         string          `json:"query"`
	OperationName string          `json:"operationName"`
	Variables     json.RawMessage `json:"variables"`

	vars graphQLVars
}

var (
	graphQLSignature   = regexp.MustCompile(`^\s*(query|mutation)\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(([^)]*)\))?`)
	graphQLDeclaredVar = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)\s*:`)
)

// graphQL answers one GraphQL request. Callers hold mu.
func (e *Engine) graphQL(c *call) response {
	req := c.graphQL
	i := slices.IndexFunc(githubGraphQL, func(op graphQLOp) bool { return op.name == req.OperationName })
	if i < 0 {
		return unhandled("GraphQL operation %q is not implemented", req.OperationName)
	}
	op := githubGraphQL[i]
	m := graphQLSignature.FindStringSubmatch(req.Query)
	if m == nil || m[1] != op.kind || m[2] != op.name {
		return unhandled("GraphQL document does not declare %s %s", op.kind, op.name)
	}
	var declared []string
	for _, v := range graphQLDeclaredVar.FindAllStringSubmatch(m[3], -1) {
		declared = append(declared, v[1])
	}
	want := append(slices.Clone(op.required), op.optional...)
	slices.Sort(declared)
	slices.Sort(want)
	if !slices.Equal(declared, want) {
		return unhandled("%s declares variables %v; the fake implements %v", op.name, declared, want)
	}
	for name, value := range req.vars {
		if !slices.Contains(want, name) {
			return unhandled("%s variable %q is not declared", op.name, name)
		}
		if value == nil && slices.Contains(op.required, name) {
			return unhandled("%s variable %q is null", op.name, name)
		}
	}
	for _, name := range op.required {
		if _, ok := req.vars[name]; !ok {
			return unhandled("%s variable %q is missing", op.name, name)
		}
	}
	resp := op.run(e, c, req.vars)
	if resp.route == "" {
		resp.route = "gh graphql " + op.name
	}
	return resp
}

// graphQLVars are a request's variables as JSON decoded them.
type graphQLVars map[string]any

func (v graphQLVars) str(name string) (string, error) {
	s, ok := v[name].(string)
	if !ok {
		return "", fmt.Errorf("variable %q is not a string", name)
	}
	return s, nil
}

func (v graphQLVars) integer(name string) (int, error) {
	f, ok := v[name].(float64)
	if !ok || f != float64(int(f)) {
		return 0, fmt.Errorf("variable %q is not an integer", name)
	}
	return int(f), nil
}

func (v graphQLVars) boolean(name string) (bool, error) {
	b, ok := v[name].(bool)
	if !ok {
		return false, fmt.Errorf("variable %q is not a boolean", name)
	}
	return b, nil
}

// cursor is the offset an after variable names: 0 when it is absent or
// null, else a cursor this fake handed out.
func (v graphQLVars) cursor(name string) (int, error) {
	raw, ok := v[name]
	if !ok || raw == nil {
		return 0, nil
	}
	s, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("variable %q is not a string", name)
	}
	offset, err := strconv.Atoi(strings.TrimPrefix(s, "cursor:"))
	if err != nil || !strings.HasPrefix(s, "cursor:") || offset < 0 {
		return 0, fmt.Errorf("cursor %q is not one the fake handed out", s)
	}
	return offset, nil
}

// pullVars reads the owner, name and number variables.
func (v graphQLVars) pullVars() (project string, number int, err error) {
	owner, err1 := v.str("owner")
	name, err2 := v.str("name")
	number, err3 := v.integer("number")
	return owner + "/" + name, number, firstErr(err1, err2, err3)
}

// graphQLData is a successful GraphQL answer.
func graphQLData(data map[string]any) response {
	return jsonResponse(map[string]any{"data": data})
}

// graphQLNotFound is GitHub's answer for an object that does not
// resolve: 200, the data with null at path, and a NOT_FOUND error.
func graphQLNotFound(data map[string]any, path []string, message string) response {
	return jsonResponse(map[string]any{
		"data":   data,
		"errors": []map[string]any{{"type": "NOT_FOUND", "path": path, "message": message}},
	})
}

// graphQLConnection is the page of nodes from offset, with pageInfo.
func graphQLConnection(nodes []map[string]any, offset, first int) map[string]any {
	start := min(offset, len(nodes))
	end := min(start+first, len(nodes))
	var endCursor any
	if end > start {
		endCursor = "cursor:" + strconv.Itoa(end)
	}
	page := nodes[start:end]
	if page == nil {
		page = []map[string]any{}
	}
	return map[string]any{
		"pageInfo": map[string]any{"hasNextPage": end < len(nodes), "endCursor": endCursor},
		"nodes":    page,
	}
}

// ghGraphQLPull resolves the pull a PR-scoped operation names on the
// request's host. On a miss it returns GitHub's NOT_FOUND answer, with
// base merged into the data beside the null.
func (e *Engine) ghGraphQLPull(c *call, project string, number int, base map[string]any) (*Repo, *Pull, response, bool) {
	r := e.repoOn("github", c.http.host, project)
	if r == nil {
		data := maps.Clone(base)
		data["repository"] = nil
		return nil, nil, graphQLNotFound(data, []string{"repository"}, fmt.Sprintf("Could not resolve to a Repository with the name '%s'.", project)), false
	}
	p := r.pull(number)
	if p == nil {
		data := maps.Clone(base)
		data["repository"] = map[string]any{"pullRequest": nil}
		return nil, nil, graphQLNotFound(data, []string{"repository", "pullRequest"}, fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number)), false
	}
	return r, p, response{}, true
}

// ghPRTick answers the pump's one read of a PR. number is always
// selected; $wantDetail adds the viewer and the detail fields,
// $wantChecks the head commit's rollup, and $wantThreads the first page
// of review threads and conversation comments.
func ghPRTick(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	wantDetail, err1 := v.boolean("wantDetail")
	wantThreads, err2 := v.boolean("wantThreads")
	wantChecks, err3 := v.boolean("wantChecks")
	if err := firstErr(err, err1, err2, err3); err != nil {
		return unhandled("PRTick: %v", err)
	}
	data := map[string]any{}
	if wantDetail {
		data["viewer"] = map[string]any{"login": e.viewer}
	}
	r, p, fail, ok := e.ghGraphQLPull(c, project, number, data)
	if !ok {
		return fail
	}
	pull := map[string]any{"number": p.Number}
	if wantDetail {
		maps.Copy(pull, githubPullDetail(r, p))
	}
	if wantChecks {
		var rollup any
		if contexts := githubContextNodes(r, p.CI); len(contexts) > 0 {
			rollup = map[string]any{"contexts": graphQLConnection(contexts, 0, githubGraphQLPageSize)}
		}
		pull["commits"] = map[string]any{"nodes": []map[string]any{{"commit": map[string]any{"id": githubCommitNodeID(r, p), "statusCheckRollup": rollup}}}}
	}
	if wantThreads {
		pull["reviewThreads"] = graphQLConnection(githubThreadNodes(p), 0, githubGraphQLPageSize)
		pull["comments"] = graphQLConnection(githubCommentNodes(p), 0, githubGraphQLPageSize)
	}
	data["repository"] = map[string]any{"pullRequest": pull}
	return graphQLData(data)
}

// githubPullDetail is the detail selection of a pull.
func githubPullDetail(r *Repo, p *Pull) map[string]any {
	var decision any
	if d := githubReviewDecision(p.Reviews); d != "" {
		decision = d
	}
	return map[string]any{
		"title":            p.Title,
		"body":             p.Body,
		"state":            strings.ToUpper(p.State),
		"isDraft":          p.Draft,
		"baseRefName":      p.BaseRef,
		"headRefName":      p.HeadRef,
		"headRefOid":       p.HeadSHA,
		"url":              githubPullURL(r, p),
		"additions":        diffTotals(p.Diff).additions,
		"deletions":        diffTotals(p.Diff).deletions,
		"changedFiles":     len(diffFiles(p.Diff)),
		"mergeable":        githubMergeable[p.Mergeable],
		"mergeStateStatus": githubMergeState[p.Mergeable],
		"reviewDecision":   decision,
		"author":           githubActor(p.Author, p.AuthorName),
		"latestReviews":    map[string]any{"nodes": githubLatestReviews(p.Reviews)},
	}
}

// githubLatestReviews is latestReviews: each author's latest submitted
// review, in the order of those reviews. A pending review is its
// author's draft and is not listed.
func githubLatestReviews(reviews []Review) []map[string]any {
	latest := map[string]int{}
	for i, review := range reviews {
		if review.State != "PENDING" {
			latest[review.Author] = i
		}
	}
	indexes := slices.Sorted(maps.Values(latest))
	out := make([]map[string]any, 0, len(indexes))
	for _, i := range indexes {
		review := reviews[i]
		out = append(out, map[string]any{
			"author":      githubActor(review.Author, review.AuthorName),
			"body":        review.Body,
			"submittedAt": review.SubmittedAt,
			"state":       review.State,
			"commit":      map[string]any{"oid": review.CommitSHA},
		})
	}
	return out
}

// githubReviewDecision derives GitHub's decision from each reviewer's
// latest verdict.
func githubReviewDecision(reviews []Review) string {
	latest := make(map[string]string)
	for _, review := range reviews {
		if review.State == "APPROVED" || review.State == "CHANGES_REQUESTED" || review.State == "DISMISSED" {
			latest[review.Author] = review.State
		}
	}
	decision := ""
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return "CHANGES_REQUESTED"
		}
		if state == "APPROVED" {
			decision = "APPROVED"
		}
	}
	return decision
}

// githubActor answers `author { login ... on User { name } }`. An actor
// without a display name answers as a bot: the User fragment does not
// apply, so there is no name key.
func githubActor(login, name string) map[string]any {
	actor := map[string]any{"login": login}
	if name != "" {
		actor["name"] = name
	}
	return actor
}

// githubCommitNodeID is the node id of a pull's head commit, which
// RollupContextsPage resolves back to the pull.
func githubCommitNodeID(r *Repo, p *Pull) string {
	return fmt.Sprintf("C_ao%d_%d", r.ID, p.Number)
}

// githubContextNodes are the rollup's CheckRun contexts, one per job of
// the pull's workflow run.
func githubContextNodes(r *Repo, pipeline *Pipeline) []map[string]any {
	if pipeline == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(pipeline.Jobs))
	for _, job := range pipeline.Jobs {
		status, conclusion := githubCheckState(job.Status)
		out = append(out, map[string]any{
			"__typename":  "CheckRun",
			"databaseId":  job.ID,
			"name":        job.Name,
			"status":      status,
			"conclusion":  nullable(conclusion),
			"startedAt":   nullable(job.StartedAt),
			"completedAt": nullable(job.CompletedAt),
			"detailsUrl":  githubJobURL(r, pipeline, job),
			"checkSuite": map[string]any{"workflowRun": map[string]any{
				"databaseId": pipeline.ID,
				"url":        fmt.Sprintf("https://%s/%s/actions/runs/%d", r.Host, r.Project, pipeline.ID),
				"workflow":   map[string]any{"name": pipeline.Name},
			}},
		})
	}
	return out
}

// nullable is null for an empty string.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func githubThreadNodes(p *Pull) []map[string]any {
	out := make([]map[string]any, 0, len(p.Threads))
	for i := range p.Threads {
		thread := &p.Threads[i]
		subjectType, diffSide := "LINE", strings.ToUpper(thread.Side)
		if thread.Side == "file" {
			subjectType, diffSide = "FILE", "RIGHT"
		}
		var line, startLine, startSide any
		if thread.Line != nil && !thread.Outdated {
			line = *thread.Line
		}
		if thread.StartLine != nil && !thread.Outdated {
			startLine, startSide = *thread.StartLine, diffSide
		}
		out = append(out, map[string]any{
			"id":            thread.ID,
			"isResolved":    thread.Resolved,
			"isOutdated":    thread.Outdated,
			"path":          thread.Path,
			"line":          line,
			"startLine":     startLine,
			"diffSide":      diffSide,
			"startDiffSide": startSide,
			"subjectType":   subjectType,
			"comments":      graphQLConnection(githubThreadCommentNodes(thread), 0, githubGraphQLPageSize),
		})
	}
	return out
}

func githubThreadCommentNodes(thread *Thread) []map[string]any {
	out := make([]map[string]any, 0, len(thread.Comments))
	for i, comment := range thread.Comments {
		var replyTo any
		if i > 0 {
			root := thread.Comments[0]
			replyTo = map[string]any{"id": githubCommentNodeID("PRRC", root.ID), "databaseId": root.ID}
		}
		out = append(out, map[string]any{
			"id":         githubCommentNodeID("PRRC", comment.ID),
			"databaseId": comment.ID,
			"author":     githubActor(comment.Author, comment.AuthorName),
			"body":       comment.Body,
			"createdAt":  comment.CreatedAt,
			"replyTo":    replyTo,
		})
	}
	return out
}

func githubCommentNodes(p *Pull) []map[string]any {
	out := make([]map[string]any, 0, len(p.Comments))
	for _, comment := range p.Comments {
		out = append(out, map[string]any{
			"id":          githubCommentNodeID("IC", comment.ID),
			"databaseId":  comment.ID,
			"author":      githubActor(comment.Author, comment.AuthorName),
			"body":        comment.Body,
			"createdAt":   comment.CreatedAt,
			"isMinimized": false,
		})
	}
	return out
}

func githubCommentNodeID(prefix string, id int64) string {
	return prefix + "_ao" + strconv.FormatInt(id, 10)
}

// ghPullConnectionPage answers PRThreadsPage and PRCommentsPage: one page
// of a pull's connection from the after cursor.
func ghPullConnectionPage(e *Engine, c *call, v graphQLVars, connection string, nodes func(*Pull) []map[string]any) response {
	project, number, err := v.pullVars()
	offset, err1 := v.cursor("after")
	if err := firstErr(err, err1); err != nil {
		return unhandled("%v", err)
	}
	_, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		connection: graphQLConnection(nodes(p), offset, githubGraphQLPageSize),
	}}})
}

func ghPRThreadsPage(e *Engine, c *call, v graphQLVars) response {
	return ghPullConnectionPage(e, c, v, "reviewThreads", githubThreadNodes)
}

func ghPRCommentsPage(e *Engine, c *call, v graphQLVars) response {
	return ghPullConnectionPage(e, c, v, "comments", githubCommentNodes)
}

// githubThread finds a review thread by node id among the GitHub
// repositories on the request's host. Callers hold mu.
func (e *Engine) githubThread(c *call, id string) *Thread {
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "github" || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		for i := range r.Pulls {
			for j := range r.Pulls[i].Threads {
				if r.Pulls[i].Threads[j].ID == id {
					return &r.Pulls[i].Threads[j]
				}
			}
		}
	}
	return nil
}

func graphQLNodeNotFound(id string) response {
	return graphQLNotFound(map[string]any{"node": nil}, []string{"node"}, fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id))
}

func ghThreadCommentsPage(e *Engine, c *call, v graphQLVars) response {
	id, err := v.str("threadID")
	offset, err1 := v.cursor("after")
	if err := firstErr(err, err1); err != nil {
		return unhandled("ThreadCommentsPage: %v", err)
	}
	thread := e.githubThread(c, id)
	if thread == nil {
		return graphQLNodeNotFound(id)
	}
	return graphQLData(map[string]any{"node": map[string]any{
		"comments": graphQLConnection(githubThreadCommentNodes(thread), offset, githubGraphQLPageSize),
	}})
}

var githubCommitNodePattern = regexp.MustCompile(`^C_ao(\d+)_(\d+)$`)

func ghRollupContextsPage(e *Engine, c *call, v graphQLVars) response {
	id, err := v.str("commitID")
	offset, err1 := v.cursor("after")
	if err := firstErr(err, err1); err != nil {
		return unhandled("RollupContextsPage: %v", err)
	}
	m := githubCommitNodePattern.FindStringSubmatch(id)
	if m == nil {
		return graphQLNodeNotFound(id)
	}
	repoID, _ := strconv.ParseInt(m[1], 10, 64)
	number, _ := strconv.Atoi(m[2])
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "github" || r.ID != repoID || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		p := r.pull(number)
		if p == nil {
			break
		}
		var rollup any
		if contexts := githubContextNodes(r, p.CI); len(contexts) > 0 {
			rollup = map[string]any{"contexts": graphQLConnection(contexts, offset, githubGraphQLPageSize)}
		}
		return graphQLData(map[string]any{"node": map[string]any{"statusCheckRollup": rollup}})
	}
	return graphQLNodeNotFound(id)
}

// ghSetThreadResolved resolves or unresolves a review thread. The
// document aliases resolveReviewThread as resolve (@include) and
// unresolveReviewThread as unresolve (@skip), so the answer carries the
// one alias $resolved selects.
func ghSetThreadResolved(e *Engine, c *call, v graphQLVars) response {
	id, err := v.str("threadID")
	resolved, err1 := v.boolean("resolved")
	if err := firstErr(err, err1); err != nil {
		return unhandled("SetThreadResolved: %v", err)
	}
	alias := "unresolve"
	if resolved {
		alias = "resolve"
	}
	thread := e.githubThread(c, id)
	if thread == nil {
		return graphQLNotFound(map[string]any{alias: nil}, []string{alias}, fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id))
	}
	thread.Resolved = resolved
	return graphQLData(map[string]any{alias: map[string]any{"thread": map[string]any{"isResolved": thread.Resolved}}})
}

// ghOpenPRsByHead answers the open pull requests of a head branch, at
// most 10.
func ghOpenPRsByHead(e *Engine, c *call, v graphQLVars) response {
	owner, err1 := v.str("owner")
	name, err2 := v.str("name")
	head, err3 := v.str("head")
	if err := firstErr(err1, err2, err3); err != nil {
		return unhandled("OpenPRsByHead: %v", err)
	}
	r := e.repoOn("github", c.http.host, owner+"/"+name)
	if r == nil {
		return graphQLNotFound(map[string]any{"repository": nil}, []string{"repository"}, fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name))
	}
	nodes := []map[string]any{}
	for i := range r.Pulls {
		p := &r.Pulls[i]
		if p.State != "open" || p.HeadRef != head || len(nodes) == 10 {
			continue
		}
		nodes = append(nodes, map[string]any{"url": githubPullURL(r, p), "number": p.Number, "title": p.Title, "state": "OPEN"})
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": nodes}}})
}

// ghMergedPRs answers merged pull requests a page of first (1 to 100) at
// a time. The fixture records no update times, so its order stands for
// most recently updated first.
func ghMergedPRs(e *Engine, c *call, v graphQLVars) response {
	owner, err1 := v.str("owner")
	name, err2 := v.str("name")
	first, err3 := v.integer("first")
	offset, err4 := v.cursor("after")
	if err := firstErr(err1, err2, err3, err4); err != nil {
		return unhandled("MergedPRs: %v", err)
	}
	if first < 1 || first > 100 {
		return unhandled("MergedPRs first %d is not 1..100", first)
	}
	r := e.repoOn("github", c.http.host, owner+"/"+name)
	if r == nil {
		return graphQLNotFound(map[string]any{"repository": nil}, []string{"repository"}, fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name))
	}
	nodes := []map[string]any{}
	for i := range r.Pulls {
		if p := &r.Pulls[i]; p.State == "merged" {
			nodes = append(nodes, map[string]any{"headRefName": p.HeadRef, "headRefOid": p.HeadSHA, "url": githubPullURL(r, p)})
		}
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequests": graphQLConnection(nodes, offset, first)}})
}
