package forgefake

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// GitHub handlers. Output shapes follow gh's non-tty JSON; the fields a
// handler emits are the ones internal/git parses.

// githubPullFields is every `--json` field `gh pr view` / `gh pr list`
// answers here. A field outside it is refused rather than omitted.
var githubPullFields = map[string]func(r *Repo, p *Pull) any{
	"number":           func(_ *Repo, p *Pull) any { return p.Number },
	"title":            func(_ *Repo, p *Pull) any { return p.Title },
	"body":             func(_ *Repo, p *Pull) any { return p.Body },
	"url":              func(r *Repo, p *Pull) any { return githubPullURL(r, p) },
	"state":            func(_ *Repo, p *Pull) any { return strings.ToUpper(p.State) },
	"isDraft":          func(_ *Repo, p *Pull) any { return p.Draft },
	"author":           func(_ *Repo, p *Pull) any { return map[string]any{"login": p.Author} },
	"headRefName":      func(_ *Repo, p *Pull) any { return p.HeadRef },
	"baseRefName":      func(_ *Repo, p *Pull) any { return p.BaseRef },
	"headRefOid":       func(_ *Repo, p *Pull) any { return p.HeadSHA },
	"files":            func(_ *Repo, p *Pull) any { return diffFiles(p.Diff) },
	"additions":        func(_ *Repo, p *Pull) any { return diffTotals(p.Diff).additions },
	"deletions":        func(_ *Repo, p *Pull) any { return diffTotals(p.Diff).deletions },
	"changedFiles":     func(_ *Repo, p *Pull) any { return len(diffFiles(p.Diff)) },
	"mergeable":        func(_ *Repo, p *Pull) any { return githubMergeable[p.Mergeable] },
	"mergeStateStatus": func(_ *Repo, p *Pull) any { return githubMergeState[p.Mergeable] },
	"reviewDecision":   func(_ *Repo, p *Pull) any { return githubReviewDecision(p.Reviews) },
	"reviews":          func(_ *Repo, p *Pull) any { return githubReviews(p.Reviews) },
	"statusCheckRollup": func(r *Repo, p *Pull) any {
		return githubCheckRollup(r, p.CI)
	},
}

var (
	githubMergeable  = map[string]string{"clean": "MERGEABLE", "conflicts": "CONFLICTING", "checking": "UNKNOWN"}
	githubMergeState = map[string]string{"clean": "CLEAN", "conflicts": "DIRTY", "checking": "UNKNOWN"}
)

func githubPullURL(r *Repo, p *Pull) string {
	return fmt.Sprintf("https://%s/%s/pull/%d", r.Host, r.Project, p.Number)
}

// selectGitHubFields answers a `--json a,b,c` list.
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

func ghNotFoundRepo(project string) response {
	return response{exit: 1, stderr: fmt.Sprintf("GraphQL: Could not resolve to a Repository with the name '%s'. (repository)\n", project)}
}

func ghNotFoundPull(number int) response {
	return response{exit: 1, stderr: fmt.Sprintf("GraphQL: Could not resolve to a PullRequest with the number of %d. (repository.pullRequest)\n", number)}
}

// ghRepoPull resolves `--repo P <number>`.
func (e *Engine) ghRepoPull(c *call) (*Repo, *Pull, response, bool) {
	project := c.flag("repo")
	if project == "" || len(c.positional) != 1 {
		return nil, nil, unhandled("expected --repo OWNER/REPO and one PR number"), false
	}
	number, err := strconv.Atoi(c.positional[0])
	if err != nil || number <= 0 {
		return nil, nil, unhandled("PR number %q is not a positive integer", c.positional[0]), false
	}
	r := e.repo("github", project)
	if r == nil {
		return nil, nil, ghNotFoundRepo(project), false
	}
	p := r.pull(number)
	if p == nil {
		return nil, nil, ghNotFoundPull(number), false
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

func ghPRDiff(e *Engine, c *call) response {
	_, p, fail, ok := e.ghRepoPull(c)
	if !ok {
		return fail
	}
	return response{stdout: []byte(p.Diff)}
}

// ghPRList answers the two lists the app makes: the open PR for a head
// branch and recently merged heads. The repository is the checkout's
// origin unless --repo names one.
func ghPRList(e *Engine, c *call) response {
	if len(c.positional) != 0 {
		return unhandled("pr list takes no positional arguments")
	}
	var r *Repo
	if project := c.flag("repo"); project != "" {
		if r = e.repo("github", project); r == nil {
			return ghNotFoundRepo(project)
		}
	} else {
		var err error
		if r, err = e.repoForCheckout("github", c.cwd); err != nil {
			return response{exit: 1, stderr: err.Error() + "\n"}
		}
	}
	state := c.flag("state")
	if state != "open" && state != "merged" {
		return unhandled("--state %q is not implemented", state)
	}
	limit := 30
	if raw := c.flag("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return unhandled("--limit %q is not a positive integer", raw)
		}
		limit = n
	}
	head := c.flag("head")
	out := []map[string]any{}
	for i := range r.Pulls {
		p := &r.Pulls[i]
		if p.State != state || (head != "" && p.HeadRef != head) {
			continue
		}
		if len(out) == limit {
			break
		}
		fields, err := selectGitHubFields(c.flag("json"), r, p)
		if err != nil {
			return unhandled("%v", err)
		}
		out = append(out, fields)
	}
	return jsonResponse(out)
}

func ghRunView(e *Engine, c *call) response {
	project := c.flag("repo")
	if project == "" || len(c.positional) != 1 {
		return unhandled("expected a run id and --repo OWNER/REPO")
	}
	r := e.repo("github", project)
	if r == nil {
		return ghNotFoundRepo(project)
	}
	runID, _ := strconv.ParseInt(c.positional[0], 10, 64)
	pipeline := r.pipeline(runID)
	if pipeline == nil {
		return response{exit: 1, stderr: fmt.Sprintf("could not find any workflow run with ID %s\n", c.positional[0])}
	}
	fields := map[string]func() any{
		"workflowName": func() any { return pipeline.Name },
		"jobs": func() any {
			jobs := make([]map[string]any, 0, len(pipeline.Jobs))
			for _, job := range pipeline.Jobs {
				status, conclusion := githubCheckState(job.Status)
				steps := make([]map[string]any, 0, len(job.Steps))
				for i, step := range job.Steps {
					stepStatus, stepConclusion := githubCheckState(step.Status)
					steps = append(steps, map[string]any{"number": i + 1, "name": step.Name, "status": stepStatus, "conclusion": stepConclusion})
				}
				jobs = append(jobs, map[string]any{
					"databaseId":  job.ID,
					"name":        job.Name,
					"status":      status,
					"conclusion":  conclusion,
					"startedAt":   githubTime(job.StartedAt),
					"completedAt": githubTime(job.CompletedAt),
					"url":         githubJobURL(r, pipeline, job),
					"steps":       steps,
				})
			}
			return jobs
		},
	}
	out := make(map[string]any)
	for _, field := range strings.Split(c.flag("json"), ",") {
		value, ok := fields[field]
		if !ok {
			return unhandled("--json field %q is not implemented for run view", field)
		}
		out[field] = value()
	}
	return jsonResponse(out)
}

func (r *Repo) pipeline(id int64) *Pipeline {
	for i := range r.Pulls {
		if ci := r.Pulls[i].CI; ci != nil && ci.ID == id {
			return ci
		}
	}
	return nil
}

func (r *Repo) job(id int64) *Job {
	for i := range r.Pulls {
		if ci := r.Pulls[i].CI; ci != nil {
			for j := range ci.Jobs {
				if ci.Jobs[j].ID == id {
					return &ci.Jobs[j]
				}
			}
		}
	}
	return nil
}

// githubCheckState spells a job status as gh's (status, conclusion).
func githubCheckState(status string) (string, string) {
	switch status {
	case "running":
		return "IN_PROGRESS", ""
	case "pending":
		return "QUEUED", ""
	case "failed":
		return "COMPLETED", "FAILURE"
	case "skipped":
		return "COMPLETED", "SKIPPED"
	case "canceled":
		return "COMPLETED", "CANCELLED"
	default:
		return "COMPLETED", "SUCCESS"
	}
}

// githubTime is gh's spelling of an absent time.
func githubTime(value string) string {
	if value == "" {
		return "0001-01-01T00:00:00Z"
	}
	return value
}

func githubJobURL(r *Repo, pipeline *Pipeline, job Job) string {
	return fmt.Sprintf("https://%s/%s/actions/runs/%d/job/%d", r.Host, r.Project, pipeline.ID, job.ID)
}

func githubCheckRollup(r *Repo, pipeline *Pipeline) []map[string]any {
	out := []map[string]any{}
	if pipeline == nil {
		return out
	}
	for _, job := range pipeline.Jobs {
		status, conclusion := githubCheckState(job.Status)
		out = append(out, map[string]any{
			"__typename":   "CheckRun",
			"name":         job.Name,
			"workflowName": pipeline.Name,
			"status":       status,
			"conclusion":   conclusion,
			"detailsUrl":   githubJobURL(r, pipeline, job),
			"startedAt":    githubTime(job.StartedAt),
			"completedAt":  githubTime(job.CompletedAt),
		})
	}
	return out
}

func githubReviews(reviews []Review) []map[string]any {
	out := make([]map[string]any, 0, len(reviews))
	for _, review := range reviews {
		out = append(out, map[string]any{
			"author":      map[string]any{"login": review.Author},
			"state":       review.State,
			"body":        review.Body,
			"submittedAt": review.SubmittedAt,
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

func ghAPIUser(e *Engine, c *call, _ []string) response {
	user := map[string]any{"login": e.viewer, "id": 1, "type": "User"}
	if expr := c.flag("jq"); c.has("jq") {
		out, err := applyJQ(expr, user)
		if err != nil {
			return unhandled("%v", err)
		}
		return response{stdout: out}
	}
	return jsonResponse(user)
}

// GraphQL queries are matched against the app's exact query text with
// whitespace collapsed, so a changed selection set is an unhandled query
// rather than an answer missing the new field.
const (
	githubReviewThreadsQuery = `query { repository(owner: @OWNER, name: @NAME) { pullRequest(number: @NUMBER) { reviewThreads(first: 50@AFTER) { pageInfo { hasNextPage endCursor } nodes { id isResolved isOutdated path line startLine diffSide startDiffSide subjectType comments(first: 50) { nodes { id databaseId author { login } body createdAt replyTo { id databaseId } } } } } } } }`
	githubPRCommentsQuery    = `query { repository(owner: @OWNER, name: @NAME) { pullRequest(number: @NUMBER) { comments(first: 50@AFTER) { pageInfo { hasNextPage endCursor } nodes { id databaseId author { login } body createdAt isMinimized } } } } }`
	githubGraphQLPageSize    = 50
)

var (
	githubReviewThreadsPattern = graphQLPattern(githubReviewThreadsQuery)
	githubPRCommentsPattern    = graphQLPattern(githubPRCommentsQuery)
	whitespaceRun              = regexp.MustCompile(`\s+`)
)

func graphQLPattern(template string) *regexp.Regexp {
	quoted := `("(?:[^"\\]|\\.)*")`
	pattern := regexp.QuoteMeta(template)
	pattern = strings.Replace(pattern, "@OWNER", quoted, 1)
	pattern = strings.Replace(pattern, "@NAME", quoted, 1)
	pattern = strings.Replace(pattern, "@NUMBER", `(\d+)`, 1)
	pattern = strings.Replace(pattern, "@AFTER", `(?:, after: `+quoted+`)?`, 1)
	return regexp.MustCompile("^" + pattern + "$")
}

func ghGraphQL(e *Engine, c *call, _ []string) response {
	fields := c.flags["raw-field"]
	if len(fields) != 1 || !strings.HasPrefix(fields[0], "query=") {
		return unhandled("graphql expects exactly one -f query=...")
	}
	query := strings.TrimSpace(whitespaceRun.ReplaceAllString(strings.TrimPrefix(fields[0], "query="), " "))
	if m := githubReviewThreadsPattern.FindStringSubmatch(query); m != nil {
		return e.ghGraphQLPage(m, "reviewThreads", githubThreadNodes)
	}
	if m := githubPRCommentsPattern.FindStringSubmatch(query); m != nil {
		return e.ghGraphQLPage(m, "comments", githubCommentNodes)
	}
	return unhandled("GraphQL query is not one the fake implements")
}

// ghGraphQLPage answers one page of a pullRequest connection. The cursor
// is the offset of the page's last node.
func (e *Engine) ghGraphQLPage(m []string, connection string, nodes func(*Pull) []map[string]any) response {
	owner, err1 := strconv.Unquote(m[1])
	name, err2 := strconv.Unquote(m[2])
	number, err3 := strconv.Atoi(m[3])
	offset := 0
	var err4 error
	if m[4] != "" {
		var after string
		if after, err4 = strconv.Unquote(m[4]); err4 == nil {
			offset, err4 = strconv.Atoi(strings.TrimPrefix(after, "cursor:"))
		}
	}
	if err := firstErr(err1, err2, err3, err4); err != nil {
		return unhandled("GraphQL arguments: %v", err)
	}
	r := e.repo("github", owner+"/"+name)
	if r == nil {
		return ghNotFoundRepo(owner + "/" + name)
	}
	p := r.pull(number)
	if p == nil {
		return ghNotFoundPull(number)
	}
	all := nodes(p)
	end := min(offset+githubGraphQLPageSize, len(all))
	page := all[min(offset, len(all)):end]
	return jsonResponse(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		connection: map[string]any{
			"pageInfo": map[string]any{"hasNextPage": end < len(all), "endCursor": "cursor:" + strconv.Itoa(end)},
			"nodes":    page,
		},
	}}}})
}

func githubThreadNodes(p *Pull) []map[string]any {
	out := make([]map[string]any, 0, len(p.Threads))
	for _, thread := range p.Threads {
		comments := make([]map[string]any, 0, len(thread.Comments))
		for i, comment := range thread.Comments {
			var replyTo any
			if i > 0 {
				root := thread.Comments[0]
				replyTo = map[string]any{"id": githubCommentNodeID("PRRC", root.ID), "databaseId": root.ID}
			}
			comments = append(comments, map[string]any{
				"id":         githubCommentNodeID("PRRC", comment.ID),
				"databaseId": comment.ID,
				"author":     map[string]any{"login": comment.Author},
				"body":       comment.Body,
				"createdAt":  comment.CreatedAt,
				"replyTo":    replyTo,
			})
		}
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
			"comments":      map[string]any{"nodes": comments},
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
			"author":      map[string]any{"login": comment.Author},
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

func ghJobLogs(e *Engine, _ *call, m []string) response {
	r := e.repo("github", m[1])
	id, _ := strconv.ParseInt(m[2], 10, 64)
	var job *Job
	if r != nil {
		job = r.job(id)
	}
	if job == nil || job.StartedAt == "" {
		return ghHTTPNotFound()
	}
	// The real endpoint prepends a UTF-8 byte order mark.
	return response{stdout: append([]byte("\xef\xbb\xbf"), job.Log...)}
}

// ghAttachment serves a GitHub attachment by its request URL. The URL is
// not scoped to a repository, so every seeded GitHub repository is
// searched.
func ghAttachment(e *Engine, c *call, m []string) response {
	for _, header := range c.flags["header"] {
		if !strings.EqualFold(strings.ReplaceAll(header, " ", ""), "Accept:*/*") {
			return unhandled("attachment request header %q is not implemented", header)
		}
	}
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "github" {
			continue
		}
		for _, attachment := range r.Attachments {
			if attachment.URL == m[0] {
				return response{stdout: slices.Clone(attachment.content)}
			}
		}
	}
	return ghHTTPNotFound()
}

func ghHTTPNotFound() response {
	return response{
		stdout: []byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest","status":"404"}`),
		stderr: "gh: Not Found (HTTP 404)\n",
		exit:   1,
	}
}

// applyJQ evaluates the one jq form the app uses: a path of object keys.
func applyJQ(expr string, value any) ([]byte, error) {
	if !regexp.MustCompile(`^(\.[A-Za-z_][A-Za-z0-9_]*)+$`).MatchString(expr) {
		return nil, fmt.Errorf("--jq %q is not a plain key path", expr)
	}
	for _, key := range strings.Split(expr, ".")[1:] {
		object, ok := value.(map[string]any)
		if !ok {
			value = nil
			break
		}
		value = object[key]
	}
	if s, ok := value.(string); ok {
		return []byte(s + "\n"), nil
	}
	out, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func jsonResponse(value any) response {
	out, err := json.Marshal(value)
	if err != nil {
		return response{exit: 1, stderr: "ao-mockforge: encode answer: " + err.Error() + "\n"}
	}
	return response{stdout: append(out, '\n')}
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// diffFile is one file's line counts in a unified diff.
type diffFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type diffCounts struct{ additions, deletions int }

func diffFiles(diff string) []diffFile {
	out := []diffFile{}
	var current *diffFile
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			out = append(out, diffFile{})
			current = &out[len(out)-1]
			if _, b, ok := strings.Cut(line, " b/"); ok {
				current.Path = b
			}
			inHunk = false
		case current == nil:
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk && strings.HasPrefix(line, "+++ b/"):
			current.Path = strings.TrimPrefix(line, "+++ b/")
		case inHunk && strings.HasPrefix(line, "+"):
			current.Additions++
		case inHunk && strings.HasPrefix(line, "-"):
			current.Deletions++
		}
	}
	return out
}

func diffTotals(diff string) diffCounts {
	var totals diffCounts
	for _, file := range diffFiles(diff) {
		totals.additions += file.Additions
		totals.deletions += file.Deletions
	}
	return totals
}
