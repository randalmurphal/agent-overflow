package forgefake

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
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
	{kind: "query", name: "PRTick", required: []string{"owner", "name", "number", "wantDetail", "wantThreads", "wantEvents", "wantChecks"}, run: ghPRTick},
	{kind: "query", name: "PRThreadsPage", required: []string{"owner", "name", "number", "after"}, run: ghPRThreadsPage},
	{kind: "query", name: "PRCommentsPage", required: []string{"owner", "name", "number", "after"}, run: ghPRCommentsPage},
	{kind: "query", name: "PRThreadStatesPage", required: []string{"owner", "name", "number", "after"}, run: ghPRThreadStatesPage},
	{kind: "query", name: "PRTimelinePage", required: []string{"owner", "name", "number", "before"}, run: ghPRTimelinePage},
	{kind: "query", name: "ThreadCommentsPage", required: []string{"threadID", "after"}, run: ghThreadCommentsPage},
	{kind: "query", name: "RollupContextsPage", required: []string{"commitID", "number", "after"}, run: ghRollupContextsPage},
	{kind: "mutation", name: "SetThreadResolved", required: []string{"threadID", "resolved"}, run: ghSetThreadResolved},
	{kind: "query", name: "OpenPRsByHead", required: []string{"owner", "name", "head"}, optional: []string{"after"}, run: ghOpenPRsByHead},
	{kind: "query", name: "MergedPRs", required: []string{"owner", "name", "first"}, optional: []string{"after"}, run: ghMergedPRs},
	{kind: "query", name: "PRStates", required: []string{"ids"}, run: ghPRStates},
	{kind: "query", name: "PRMergeTarget", required: []string{"owner", "name", "number"}, run: ghPRMergeTarget},
	{kind: "query", name: "ViewerApproval", required: []string{"owner", "name", "number"}, run: ghViewerApproval},
	{kind: "mutation", name: "MergePR", required: []string{"pullRequestID", "method", "expectedHeadOid"}, run: ghMergePR},
	{kind: "mutation", name: "EnableAutoMerge", required: []string{"pullRequestID", "method", "expectedHeadOid"}, run: ghEnableAutoMerge},
	{kind: "mutation", name: "DisableAutoMerge", required: []string{"pullRequestID"}, run: ghDisableAutoMerge},
	{kind: "mutation", name: "DismissReview", required: []string{"reviewID", "message"}, run: ghDismissReview},
	{kind: "mutation", name: "UpdatePRBranch", required: []string{"pullRequestID", "expectedHeadOid"}, run: ghUpdatePRBranch},
	{kind: "mutation", name: "MarkPRReady", required: []string{"pullRequestID"}, run: ghMarkPRReady},
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
	resp, refused := e.refusal("gh graphql " + op.name)
	if !refused {
		resp = op.run(e, c, req.vars)
	}
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

func (v graphQLVars) strs(name string) ([]string, error) {
	list, ok := v[name].([]any)
	if !ok {
		return nil, fmt.Errorf("variable %q is not a list", name)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("variable %q holds a non-string", name)
		}
		out = append(out, s)
	}
	return out, nil
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
// selected; $wantDetail adds the viewer, the repository's merge settings
// and the detail fields, $wantChecks the head commit's rollup,
// $wantThreads the first page of review threads and conversation
// comments, and $wantEvents the newest page of timeline items.
func ghPRTick(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	wantDetail, err1 := v.boolean("wantDetail")
	wantThreads, err2 := v.boolean("wantThreads")
	wantChecks, err3 := v.boolean("wantChecks")
	wantEvents, err4 := v.boolean("wantEvents")
	if err := firstErr(err, err1, err2, err3, err4); err != nil {
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
	repository := map[string]any{"pullRequest": pull}
	if wantDetail {
		maps.Copy(repository, githubRepositoryMerge(r))
		maps.Copy(pull, e.githubPullDetail(r, p))
	}
	if wantChecks {
		var rollup any
		if contexts := githubContextNodes(r, p); len(contexts) > 0 {
			rollup = map[string]any{"contexts": graphQLConnection(contexts, 0, githubGraphQLPageSize)}
		}
		pull["commits"] = map[string]any{"nodes": []map[string]any{{"commit": map[string]any{"id": githubCommitNodeID(r, p), "statusCheckRollup": rollup}}}}
	}
	if wantThreads {
		pull["reviewThreads"] = graphQLConnection(githubThreadNodes(p), 0, githubGraphQLPageSize)
		pull["comments"] = graphQLConnection(githubCommentNodes(p), 0, githubGraphQLPageSize)
	}
	if wantEvents {
		items := githubTimelineNodes(p)
		pull["timelineItems"] = graphQLLast(items, len(items), githubGraphQLPageSize)
	}
	data["repository"] = repository
	return graphQLData(data)
}

// githubRepositoryMerge is the repository half of the detail: the merge
// settings the Merge menu offers and the viewer's permission.
func githubRepositoryMerge(r *Repo) map[string]any {
	m := r.Merge
	return map[string]any{
		"mergeCommitAllowed":       slices.Contains(m.Methods, "merge"),
		"squashMergeAllowed":       slices.Contains(m.Methods, "squash"),
		"rebaseMergeAllowed":       slices.Contains(m.Methods, "rebase"),
		"deleteBranchOnMerge":      m.DeleteBranchOnMerge,
		"autoMergeAllowed":         m.AutoMergeAllowed,
		"viewerPermission":         m.Permission,
		"viewerDefaultMergeMethod": strings.ToUpper(m.DefaultMethod),
	}
}

// githubPullDetail is the detail selection of a pull. Callers hold mu.
func (e *Engine) githubPullDetail(r *Repo, p *Pull) map[string]any {
	var decision any
	if d := githubReviewDecision(r, p, e.viewer); d != "" {
		decision = d
	}
	var mergedAt, mergedBy, closedAt any
	if p.State == "merged" {
		mergedAt, mergedBy = p.MergedAt, githubActor(p.MergedBy, "")
	}
	if p.State != "open" {
		closedAt = p.ClosedAt
	}
	var autoMerge any
	if armed := p.Readiness.AutoMerge; armed != nil {
		autoMerge = map[string]any{"enabledAt": armed.At, "mergeMethod": strings.ToUpper(armed.Method), "enabledBy": githubActor(armed.By, armed.ByName)}
	}
	write := githubRoles[r.Merge.Permission] >= githubRoles["WRITE"]
	open := p.State == "open"
	return map[string]any{
		"title":                     p.Title,
		"body":                      p.Body,
		"state":                     strings.ToUpper(p.State),
		"isDraft":                   p.Draft,
		"baseRefName":               p.BaseRef,
		"baseRefOid":                p.StartSHA,
		"headRefName":               p.HeadRef,
		"headRefOid":                p.HeadSHA,
		"url":                       githubPullURL(r, p),
		"additions":                 diffTotals(p.Diff).additions,
		"deletions":                 diffTotals(p.Diff).deletions,
		"changedFiles":              len(diffFiles(p.Diff)),
		"mergeable":                 githubMergeable[p.Mergeable],
		"mergeStateStatus":          githubMergeStateStatus(r, p, e.viewer),
		"reviewDecision":            decision,
		"author":                    githubActor(p.Author, p.AuthorName),
		"latestReviews":             map[string]any{"nodes": githubLatestReviews(p.Reviews)},
		"merged":                    p.State == "merged",
		"mergedAt":                  mergedAt,
		"mergedBy":                  mergedBy,
		"closedAt":                  closedAt,
		"viewerDidAuthor":           strings.EqualFold(p.Author, e.viewer),
		"isCrossRepository":         p.HeadRepo != "",
		"viewerCanEnableAutoMerge":  open && r.Merge.AutoMergeAllowed && write && p.Readiness.AutoMerge == nil,
		"viewerCanDisableAutoMerge": open && p.Readiness.AutoMerge != nil && (write || strings.EqualFold(p.Readiness.AutoMerge.By, e.viewer)),
		"autoMergeRequest":          autoMerge,
		"latestOpinionatedReviews":  map[string]any{"nodes": githubOpinionNodes(r, p, e.viewer, true)},
		"threadStates":              graphQLConnection(githubThreadStateNodes(p), 0, githubGraphQLPageSize),
		"baseRef":                   githubBaseRef(p),
	}
}

// githubBaseRef is the base branch's protection as a non-admin reads it
// (refUpdateRule, null when the base requires nothing) and its ruleset
// rules, of which the fake seeds none.
func githubBaseRef(p *Pull) map[string]any {
	rd := p.Readiness
	var rule any
	if rd.RequiredApprovals > 0 || len(rd.RequiredChecks) > 0 || rd.ResolveThreads {
		contexts := slices.Clone(rd.RequiredChecks)
		if contexts == nil {
			contexts = []string{}
		}
		rule = map[string]any{
			"requiredApprovingReviewCount":   rd.RequiredApprovals,
			"requiredStatusCheckContexts":    contexts,
			"requiresCodeOwnerReviews":       false,
			"requiresConversationResolution": rd.ResolveThreads,
		}
	}
	return map[string]any{"refUpdateRule": rule, "rules": map[string]any{"nodes": []map[string]any{}}}
}

// githubCanPush is whether login can push to the repository, which is
// whether GitHub counts their approval: the viewer by their permission,
// every other reviewer as a collaborator.
func githubCanPush(r *Repo, login, viewer string) bool {
	if strings.EqualFold(login, viewer) {
		return githubRoles[r.Merge.Permission] >= githubRoles["WRITE"]
	}
	return true
}

// githubOpinions is each reviewer's latest approval or change request,
// in the order of those reviews: a later comment does not replace one,
// and a dismissed one leaves the reviewer with none.
func githubOpinions(p *Pull) []*Review {
	latest := map[string]int{}
	for i, review := range p.Reviews {
		switch review.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			latest[strings.ToLower(review.Author)] = i
		}
	}
	out := []*Review{}
	for _, i := range slices.Sorted(maps.Values(latest)) {
		if p.Reviews[i].State != "DISMISSED" {
			out = append(out, &p.Reviews[i])
		}
	}
	return out
}

// githubOpinionNodes answers latestOpinionatedReviews: full is the
// detail's selection, otherwise ViewerApproval's (id state
// viewerDidAuthor).
func githubOpinionNodes(r *Repo, p *Pull, viewer string, full bool) []map[string]any {
	out := []map[string]any{}
	for _, review := range githubOpinions(p) {
		node := map[string]any{"id": githubReviewNodeID(review.ID), "state": review.State, "viewerDidAuthor": strings.EqualFold(review.Author, viewer)}
		if full {
			node["authorCanPushToRepository"] = githubCanPush(r, review.Author, viewer)
			node["author"] = githubActor(review.Author, review.AuthorName)
		}
		out = append(out, node)
	}
	return out
}

func githubReviewNodeID(id int64) string { return "PRR_ao" + strconv.FormatInt(id, 10) }

// githubThreadStateNodes are the review threads' resolution alone.
func githubThreadStateNodes(p *Pull) []map[string]any {
	out := make([]map[string]any, 0, len(p.Threads))
	for _, thread := range p.Threads {
		out = append(out, map[string]any{"isResolved": thread.Resolved})
	}
	return out
}

// githubMergeStateStatus is GitHub's verdict on merging the pull now. A
// pull that is not open answers UNKNOWN, as GitHub does for a merged one.
// BEHIND, then BLOCKED (a draft, approvals short of the requirement, a
// required check not passed, an unresolved thread the base requires
// resolved), then UNSTABLE (a check that is not required has not passed).
func githubMergeStateStatus(r *Repo, p *Pull, viewer string) string {
	if p.State != "open" {
		return "UNKNOWN"
	}
	switch p.Mergeable {
	case "checking":
		return "UNKNOWN"
	case "conflicts":
		return "DIRTY"
	}
	rd := p.Readiness
	if rd.Behind {
		return "BEHIND"
	}
	blocked := p.Draft || (rd.RequiredApprovals > 0 && githubReviewDecision(r, p, viewer) != "APPROVED")
	if rd.ResolveThreads && slices.ContainsFunc(p.Threads, func(t Thread) bool { return !t.Resolved }) {
		blocked = true
	}
	unstable := false
	for _, name := range rd.RequiredChecks {
		if job := p.ciJob(name); job == nil || !githubCheckPassed(job.Status) {
			blocked = true
		}
	}
	if p.CI != nil {
		for _, job := range p.CI.Jobs {
			if !slices.Contains(rd.RequiredChecks, job.Name) && !githubCheckPassed(job.Status) {
				unstable = true
			}
		}
	}
	switch {
	case blocked:
		return "BLOCKED"
	case unstable:
		return "UNSTABLE"
	}
	return "CLEAN"
}

// githubCheckPassed is a check conclusion GitHub counts as passing.
func githubCheckPassed(status string) bool { return status == "success" || status == "skipped" }

// ciJob is the pull's CI job named name, or nil.
func (p *Pull) ciJob(name string) *Job {
	if p.CI == nil {
		return nil
	}
	for i := range p.CI.Jobs {
		if p.CI.Jobs[i].Name == name {
			return &p.CI.Jobs[i]
		}
	}
	return nil
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
// latest opinion: a change request decides it; with approvals required,
// it is REVIEW_REQUIRED until enough reviewers who can push approved;
// without, an approval makes it APPROVED and none leaves it null.
func githubReviewDecision(r *Repo, p *Pull, viewer string) string {
	opinions := githubOpinions(p)
	counted, approved := 0, 0
	for _, review := range opinions {
		switch review.State {
		case "CHANGES_REQUESTED":
			return "CHANGES_REQUESTED"
		case "APPROVED":
			approved++
			if githubCanPush(r, review.Author, viewer) {
				counted++
			}
		}
	}
	required := p.Readiness.RequiredApprovals
	switch {
	case required > 0 && counted < required:
		return "REVIEW_REQUIRED"
	case required > 0 || approved > 0:
		return "APPROVED"
	}
	return ""
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
// the pull's workflow run, each required when the base requires its name.
func githubContextNodes(r *Repo, p *Pull) []map[string]any {
	pipeline := p.CI
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
			"isRequired":  slices.Contains(p.Readiness.RequiredChecks, job.Name),
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
		// GitHub answers the original lines for every thread and nulls the
		// current ones once the thread is outdated.
		var line, startLine, startSide, originalLine, originalStartLine any
		if thread.Line != nil {
			originalLine = *thread.Line
			if !thread.Outdated {
				line = *thread.Line
			}
		}
		if thread.StartLine != nil {
			originalStartLine = *thread.StartLine
			if !thread.Outdated {
				startLine, startSide = *thread.StartLine, diffSide
			}
		}
		out = append(out, map[string]any{
			"id":                thread.ID,
			"isResolved":        thread.Resolved,
			"isOutdated":        thread.Outdated,
			"path":              thread.Path,
			"line":              line,
			"startLine":         startLine,
			"originalLine":      originalLine,
			"originalStartLine": originalStartLine,
			"diffSide":          diffSide,
			"startDiffSide":     startSide,
			"subjectType":       subjectType,
			"comments":          graphQLConnection(githubThreadCommentNodes(thread), 0, githubGraphQLPageSize),
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
	pullNumber, err2 := v.integer("number")
	if err := firstErr(err, err1, err2); err != nil {
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
		if pullNumber != number {
			// isRequired(pullRequestNumber:) answers for the pull it names;
			// the app asks only for the pull whose head this is.
			return unhandled("RollupContextsPage asks isRequired for #%d of the head of #%d", pullNumber, number)
		}
		var rollup any
		if contexts := githubContextNodes(r, p); len(contexts) > 0 {
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

// ghOpenPRsByHead answers the open pull requests of a head branch name, a
// fork's included, as headRefName does, a page of 100 from after.
func ghOpenPRsByHead(e *Engine, c *call, v graphQLVars) response {
	owner, err1 := v.str("owner")
	name, err2 := v.str("name")
	head, err3 := v.str("head")
	offset, err4 := v.cursor("after")
	if err := firstErr(err1, err2, err3, err4); err != nil {
		return unhandled("OpenPRsByHead: %v", err)
	}
	r := e.repoOn("github", c.http.host, owner+"/"+name)
	if r == nil {
		return graphQLNotFound(map[string]any{"repository": nil}, []string{"repository"}, fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name))
	}
	nodes := []map[string]any{}
	for i := range r.Pulls {
		p := &r.Pulls[i]
		if p.State != "open" || p.HeadRef != head {
			continue
		}
		nodes = append(nodes, map[string]any{
			"id": p.NodeID, "url": githubPullURL(r, p), "number": p.Number, "title": p.Title, "state": "OPEN",
			"isCrossRepository": p.HeadRepo != "",
		})
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequests": graphQLConnection(nodes, offset, githubGraphQLPageSize)}})
}

// ghPRStates answers pull requests by node id, at most 100, each resolved
// among the GitHub repositories on the request's host. An id that does
// not resolve answers null at its position beside a NOT_FOUND error, as
// nodes(ids:) does.
func ghPRStates(e *Engine, c *call, v graphQLVars) response {
	ids, err := v.strs("ids")
	if err != nil {
		return unhandled("PRStates: %v", err)
	}
	if len(ids) < 1 || len(ids) > 100 {
		return unhandled("PRStates names %d ids; nodes(ids:) takes 1 to 100", len(ids))
	}
	nodes := make([]any, len(ids))
	var problems []map[string]any
	for i, id := range ids {
		if node := e.githubPullNode(c, id); node != nil {
			nodes[i] = node
			continue
		}
		problems = append(problems, map[string]any{"type": "NOT_FOUND", "path": []any{"nodes", i}, "message": fmt.Sprintf("Could not resolve to a node with the global id of '%s'.", id)})
	}
	if len(problems) > 0 {
		return jsonResponse(map[string]any{"data": map[string]any{"nodes": nodes}, "errors": problems})
	}
	return graphQLData(map[string]any{"nodes": nodes})
}

// githubPullNode is the PRStates selection of the pull whose node id is
// id on the request's host, or nil.
func (e *Engine) githubPullNode(c *call, id string) map[string]any {
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "github" || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		for i := range r.Pulls {
			p := &r.Pulls[i]
			if p.NodeID != id {
				continue
			}
			var mergedAt any
			if p.MergedAt != "" {
				mergedAt = p.MergedAt
			}
			return map[string]any{"id": p.NodeID, "number": p.Number, "state": strings.ToUpper(p.State), "mergedAt": mergedAt}
		}
	}
	return nil
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

// graphQLLast is the page of at most last nodes before the before offset
// (a backward connection, timelineItems(last:, before:)), with its
// pageInfo: hasPreviousPage while older nodes remain, startCursor the
// page's first node.
func graphQLLast(nodes []map[string]any, before, last int) map[string]any {
	end := min(max(before, 0), len(nodes))
	start := max(end-last, 0)
	var startCursor any
	if end > start {
		startCursor = "cursor:" + strconv.Itoa(start)
	}
	page := nodes[start:end]
	if page == nil {
		page = []map[string]any{}
	}
	return map[string]any{
		"pageInfo": map[string]any{"hasPreviousPage": start > 0, "startCursor": startCursor},
		"nodes":    page,
	}
}

// githubEventTypes are the timeline item types of each event kind.
var githubEventTypes = map[string]string{
	"review_dismissed":    "ReviewDismissedEvent",
	"merged":              "MergedEvent",
	"closed":              "ClosedEvent",
	"reopened":            "ReopenedEvent",
	"marked_draft":        "ConvertToDraftEvent",
	"marked_ready":        "ReadyForReviewEvent",
	"auto_merge_canceled": "AutoMergeDisabledEvent",
	"force_pushed":        "HeadRefForcePushedEvent",
	"base_changed":        "BaseRefChangedEvent",
	"review_requested":    "ReviewRequestedEvent",
}

var githubAutoMergeEventTypes = map[string]string{"merge": "AutoMergeEnabledEvent", "squash": "AutoSquashEnabledEvent", "rebase": "AutoRebaseEnabledEvent"}

// githubTimelineNodes are the pull's timeline items of the types the app
// selects, oldest first: each submitted review (a PullRequestReview in
// its current state, so a dismissed approval reads DISMISSED) and each
// event.
func githubTimelineNodes(p *Pull) []map[string]any {
	type item struct {
		at   time.Time
		node map[string]any
	}
	var items []item
	for _, review := range p.Reviews {
		if review.State == "PENDING" {
			continue
		}
		at, _ := time.Parse(time.RFC3339, review.SubmittedAt)
		items = append(items, item{at, map[string]any{
			"__typename":  "PullRequestReview",
			"id":          githubReviewNodeID(review.ID),
			"state":       review.State,
			"submittedAt": review.SubmittedAt,
			"author":      githubActor(review.Author, review.AuthorName),
			"commit":      map[string]any{"oid": review.CommitSHA},
		}})
	}
	for _, event := range p.Events {
		at, _ := time.Parse(time.RFC3339, event.At)
		typename := githubEventTypes[event.Kind]
		if event.Kind == "auto_merge_set" {
			typename = githubAutoMergeEventTypes[event.Method]
		}
		node := map[string]any{
			"__typename": typename,
			"id":         fmt.Sprintf("%s_ao%d", typename, event.ID),
			"createdAt":  event.At,
			"actor":      githubActor(event.Actor, event.ActorName),
		}
		switch event.Kind {
		case "review_dismissed":
			node["dismissalMessage"] = nullable(event.Text)
			node["review"] = map[string]any{"author": map[string]any{"login": event.Ref}}
		case "merged":
			node["commit"] = map[string]any{"oid": defaultString(event.Ref, p.MergeCommitSHA)}
		case "auto_merge_canceled":
			node["reason"] = nullable(event.Text)
		case "force_pushed":
			node["afterCommit"] = map[string]any{"oid": event.Ref}
		case "base_changed":
			node["currentRefName"] = event.Ref
		case "review_requested":
			node["requestedReviewer"] = map[string]any{"__typename": "User", "login": event.Ref}
		}
		items = append(items, item{at, node})
	}
	slices.SortStableFunc(items, func(a, b item) int { return a.at.Compare(b.at) })
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, it.node)
	}
	return out
}

// ghPRThreadStatesPage answers one page of the review threads'
// resolution states from the after cursor.
func ghPRThreadStatesPage(e *Engine, c *call, v graphQLVars) response {
	return ghPullConnectionPage(e, c, v, "reviewThreads", githubThreadStateNodes)
}

// ghPRTimelinePage answers the timeline page before the before cursor,
// which PRTick or an earlier page handed out.
func ghPRTimelinePage(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	before, err1 := v.cursor("before")
	if err := firstErr(err, err1); err != nil {
		return unhandled("PRTimelinePage: %v", err)
	}
	_, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"timelineItems": graphQLLast(githubTimelineNodes(p), before, githubGraphQLPageSize),
	}}})
}

// ghPRMergeTarget answers what a merge action addresses: the pull's node
// id, its head branch and whether that is a fork's, and whether the
// repository deletes merged branches itself.
func ghPRMergeTarget(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	if err != nil {
		return unhandled("PRMergeTarget: %v", err)
	}
	r, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	return graphQLData(map[string]any{"repository": map[string]any{
		"deleteBranchOnMerge": r.Merge.DeleteBranchOnMerge,
		"pullRequest":         map[string]any{"id": p.NodeID, "headRefName": p.HeadRef, "isCrossRepository": p.HeadRepo != ""},
	}})
}

// ghViewerApproval answers the reviewers' standing verdicts.
func ghViewerApproval(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	if err != nil {
		return unhandled("ViewerApproval: %v", err)
	}
	r, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"latestOpinionatedReviews": map[string]any{"nodes": githubOpinionNodes(r, p, e.viewer, false)},
	}}})
}

// graphQLMutationError is GitHub's refusal of a mutation: 200, null at
// the mutation's field and one error of kind (UNPROCESSABLE, FORBIDDEN,
// NOT_FOUND) carrying the forge's message. Nothing changed.
func graphQLMutationError(field, kind, message string) response {
	return jsonResponse(map[string]any{
		"data":   map[string]any{field: nil},
		"errors": []map[string]any{{"type": kind, "path": []string{field}, "message": message}},
	})
}

// ghMutationPull resolves the pull a mutation's pullRequestID names among
// the GitHub repositories on the request's host; on a miss it answers
// GitHub's NOT_FOUND for field.
func (e *Engine) ghMutationPull(c *call, v graphQLVars, field string) (*Repo, *Pull, response, bool) {
	id, err := v.str("pullRequestID")
	if err != nil {
		return nil, nil, unhandled("%s: %v", field, err), false
	}
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "github" || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		for i := range r.Pulls {
			if r.Pulls[i].NodeID == id {
				return r, &r.Pulls[i], response{}, true
			}
		}
	}
	return nil, nil, graphQLMutationError(field, "NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id)), false
}

// githubNow is the time an action happens, as GitHub spells it.
func githubNow() string { return time.Now().UTC().Format(time.RFC3339) }

// githubHeadModified is GitHub's refusal of a merge whose expectedHeadOid
// is not the head.
const githubHeadModified = "Head branch was modified. Review and try the merge again."

// ghMergeChecks refuses a merge or an auto-merge the forge would refuse:
// a pull that is not open, a viewer without write access, a method the
// repository does not allow, or a head other than expectedHeadOid.
func (e *Engine) ghMergeChecks(r *Repo, p *Pull, v graphQLVars, field, mutation string) (string, response, bool) {
	method, err := v.str("method")
	head, err1 := v.str("expectedHeadOid")
	if err := firstErr(err, err1); err != nil {
		return "", unhandled("%s: %v", field, err), false
	}
	name := strings.ToLower(method)
	if !mergeMethods[name] {
		return "", unhandled("%s: method %q is not a PullRequestMergeMethod", field, method), false
	}
	switch {
	case p.State == "merged":
		return "", graphQLMutationError(field, "UNPROCESSABLE", "Pull request is already merged"), false
	case p.State == "closed":
		return "", graphQLMutationError(field, "UNPROCESSABLE", "Pull request is closed"), false
	case githubRoles[r.Merge.Permission] < githubRoles["WRITE"]:
		return "", graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `%s`", e.viewer, mutation)), false
	case !slices.Contains(r.Merge.Methods, name):
		return "", graphQLMutationError(field, "UNPROCESSABLE", map[string]string{
			"merge": "Merge commits are not allowed on this repository.", "squash": "Squash merges are not allowed on this repository.", "rebase": "Rebase merges are not allowed on this repository.",
		}[name]), false
	case head != p.HeadSHA:
		return "", graphQLMutationError(field, "UNPROCESSABLE", githubHeadModified), false
	}
	return name, response{}, true
}

// ghMergePR merges a pull GitHub would merge now (CLEAN, UNSTABLE or
// HAS_HOOKS) with the method asked, while its head is expectedHeadOid:
// it is merged by the viewer now with a new merge commit, its armed
// auto-merge is spent, and its timeline gains the MergedEvent and
// ClosedEvent; a repository that deletes merged branches deletes its
// head.
func ghMergePR(e *Engine, c *call, v graphQLVars) response {
	const field = "mergePullRequest"
	r, p, fail, ok := e.ghMutationPull(c, v, field)
	if !ok {
		return fail
	}
	if _, fail, ok := e.ghMergeChecks(r, p, v, field, "MergePullRequest"); !ok {
		return fail
	}
	switch githubMergeStateStatus(r, p, e.viewer) {
	case "CLEAN", "UNSTABLE", "HAS_HOOKS":
	case "DIRTY":
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull Request has merge conflicts")
	default:
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull Request is not mergeable")
	}
	now := githubNow()
	p.State, p.MergedAt, p.MergedBy, p.ClosedAt, p.ClosedBy = "merged", now, e.viewer, now, e.viewer
	p.MergeCommitSHA = e.ids.sha()
	p.Readiness.AutoMerge = nil
	p.Events = append(p.Events,
		Event{ID: e.ids.next(), Kind: "merged", Actor: e.viewer, At: now, Ref: p.MergeCommitSHA},
		Event{ID: e.ids.next(), Kind: "closed", Actor: e.viewer, At: now})
	if r.Merge.DeleteBranchOnMerge && p.HeadRepo == "" {
		r.deleteBranch(p.HeadRef)
	}
	return graphQLData(map[string]any{field: map[string]any{"pullRequest": map[string]any{
		"merged": true, "mergeCommit": map[string]any{"oid": p.MergeCommitSHA},
	}}})
}

// ghEnableAutoMerge arms auto-merge with the method asked while the head
// is expectedHeadOid, on a repository that allows it and a pull that is
// not already mergeable: the arm names the viewer and now, and the
// timeline gains the method's enabled event.
func ghEnableAutoMerge(e *Engine, c *call, v graphQLVars) response {
	const field = "enablePullRequestAutoMerge"
	r, p, fail, ok := e.ghMutationPull(c, v, field)
	if !ok {
		return fail
	}
	method, fail, ok := e.ghMergeChecks(r, p, v, field, "EnablePullRequestAutoMerge")
	if !ok {
		return fail
	}
	switch {
	case !r.Merge.AutoMergeAllowed:
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request Auto merge is not allowed for this repository")
	case p.Readiness.AutoMerge != nil:
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request Auto merge is already enabled")
	}
	switch githubMergeStateStatus(r, p, e.viewer) {
	case "CLEAN", "HAS_HOOKS":
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is in clean status")
	case "DIRTY":
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request has merge conflicts")
	}
	now := githubNow()
	p.Readiness.AutoMerge = &AutoMerge{Method: method, By: e.viewer, At: now}
	p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "auto_merge_set", Actor: e.viewer, At: now, Method: method})
	return graphQLData(map[string]any{field: map[string]any{"pullRequest": map[string]any{
		"autoMergeRequest": map[string]any{"enabledAt": now},
	}}})
}

// ghDisableAutoMerge disarms the pull's auto-merge: the arm is cleared and
// the timeline gains an AutoMergeDisabledEvent.
func ghDisableAutoMerge(e *Engine, c *call, v graphQLVars) response {
	const field = "disablePullRequestAutoMerge"
	r, p, fail, ok := e.ghMutationPull(c, v, field)
	if !ok {
		return fail
	}
	armed := p.Readiness.AutoMerge
	switch {
	case armed == nil:
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request Auto merge is not enabled")
	case githubRoles[r.Merge.Permission] < githubRoles["WRITE"] && !strings.EqualFold(armed.By, e.viewer):
		return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `DisablePullRequestAutoMerge`", e.viewer))
	}
	p.Readiness.AutoMerge = nil
	p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "auto_merge_canceled", Actor: e.viewer, At: githubNow()})
	return graphQLData(map[string]any{field: map[string]any{"pullRequest": map[string]any{"autoMergeRequest": nil}}})
}

// ghDismissReview dismisses an approval or change request by its node
// id: the review reads DISMISSED, leaves the reviewer's opinions, and the
// timeline gains a ReviewDismissedEvent carrying the message.
func ghDismissReview(e *Engine, c *call, v graphQLVars) response {
	const field = "dismissPullRequestReview"
	id, err := v.str("reviewID")
	message, err1 := v.str("message")
	if err := firstErr(err, err1); err != nil {
		return unhandled("DismissReview: %v", err)
	}
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "github" || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		for i := range r.Pulls {
			p := &r.Pulls[i]
			for j := range p.Reviews {
				review := &p.Reviews[j]
				if githubReviewNodeID(review.ID) != id {
					continue
				}
				switch {
				case strings.TrimSpace(message) == "":
					return graphQLMutationError(field, "UNPROCESSABLE", "Message can't be blank")
				case review.State != "APPROVED" && review.State != "CHANGES_REQUESTED":
					return graphQLMutationError(field, "UNPROCESSABLE", "Can not dismiss a "+strings.ToLower(review.State)+" pull request review")
				case githubRoles[r.Merge.Permission] < githubRoles["WRITE"]:
					return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `DismissPullRequestReview`", e.viewer))
				}
				review.State = "DISMISSED"
				p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "review_dismissed", Actor: e.viewer, At: githubNow(), Ref: review.Author, Text: message})
				return graphQLData(map[string]any{field: map[string]any{"pullRequestReview": map[string]any{"state": review.State}}})
			}
		}
	}
	return graphQLMutationError(field, "NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id))
}

// ghUpdatePRBranch merges the base into a head that is behind it, while
// the head is expectedHeadOid: the head moves to a new commit and is no
// longer behind.
func ghUpdatePRBranch(e *Engine, c *call, v graphQLVars) response {
	const field = "updatePullRequestBranch"
	head, err := v.str("expectedHeadOid")
	if err != nil {
		return unhandled("UpdatePRBranch: %v", err)
	}
	r, p, fail, ok := e.ghMutationPull(c, v, field)
	if !ok {
		return fail
	}
	switch {
	case p.State != "open":
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is not open")
	case githubRoles[r.Merge.Permission] < githubRoles["WRITE"]:
		return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `UpdatePullRequestBranch`", e.viewer))
	case head != p.HeadSHA:
		return graphQLMutationError(field, "UNPROCESSABLE", "Expected head sha didn't match current head ref.")
	case !p.Readiness.Behind:
		return graphQLMutationError(field, "UNPROCESSABLE", "There are no new commits on the base branch.")
	}
	p.HeadSHA = e.ids.sha()
	p.Readiness.Behind = false
	return graphQLData(map[string]any{field: map[string]any{"pullRequest": map[string]any{"id": p.NodeID}}})
}

// ghMarkPRReady takes a draft out of draft, adding a ReadyForReviewEvent;
// a pull that is not a draft is answered as it is.
func ghMarkPRReady(e *Engine, c *call, v graphQLVars) response {
	const field = "markPullRequestReadyForReview"
	r, p, fail, ok := e.ghMutationPull(c, v, field)
	if !ok {
		return fail
	}
	switch {
	case p.State != "open":
		return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is not open")
	case githubRoles[r.Merge.Permission] < githubRoles["WRITE"] && !strings.EqualFold(p.Author, e.viewer):
		return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `MarkPullRequestReadyForReview`", e.viewer))
	}
	if p.Draft {
		p.Draft = false
		p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "marked_ready", Actor: e.viewer, At: githubNow()})
	}
	return graphQLData(map[string]any{field: map[string]any{"pullRequest": map[string]any{"isDraft": p.Draft}}})
}

// branchExists is whether a pull names branch as its head or base and it
// has not been deleted.
func (r *Repo) branchExists(branch string) bool {
	if slices.Contains(r.DeletedBranches, branch) {
		return false
	}
	return slices.ContainsFunc(r.Pulls, func(p Pull) bool { return p.HeadRef == branch || p.BaseRef == branch })
}

func (r *Repo) deleteBranch(branch string) {
	if !slices.Contains(r.DeletedBranches, branch) {
		r.DeletedBranches = append(r.DeletedBranches, branch)
	}
}
