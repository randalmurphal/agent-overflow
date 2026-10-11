package forgefake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// GitHub handlers. The PR reads, open and merged lists and the thread
// resolve mutation are GraphQL operations (github_graphql.go); the REST
// routes here serve repository identity, a run's jobs, job logs and
// attachments, and `gh pr create` is the one gh command the app runs.

var (
	githubMergeable  = map[string]string{"clean": "MERGEABLE", "conflicts": "CONFLICTING", "checking": "UNKNOWN"}
	githubMergeState = map[string]string{"clean": "CLEAN", "conflicts": "DIRTY", "checking": "UNKNOWN"}
)

func githubPullURL(r *Repo, p *Pull) string {
	return fmt.Sprintf("https://%s/%s/pull/%d", r.Host, r.Project, p.Number)
}

// ghRunJobs answers the REST jobs list of a workflow run: snake_case,
// lowercase states, null for an absent time, paginated by per_page (30 by
// default, at most 100) and page with a Link rel="next" to the following
// page on the fake's own listener.
func ghRunJobs(e *Engine, c *call, m []string) response {
	query, perPage, page, err := glabPage(m[3])
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
	runID, _ := strconv.ParseInt(m[2], 10, 64)
	pipeline := r.pipeline(runID)
	if pipeline == nil {
		return ghHTTPNotFound()
	}
	jobs := make([]map[string]any, 0, len(pipeline.Jobs))
	for _, job := range pipeline.Jobs {
		status, conclusion := githubRESTState(job.Status)
		steps := make([]map[string]any, 0, len(job.Steps))
		for _, step := range job.Steps {
			stepStatus, stepConclusion := githubRESTState(step.Status)
			steps = append(steps, map[string]any{
				"number":       step.Number,
				"name":         step.Name,
				"status":       stepStatus,
				"conclusion":   stepConclusion,
				"started_at":   githubRESTTime(step.StartedAt),
				"completed_at": githubRESTTime(step.CompletedAt),
			})
		}
		jobs = append(jobs, map[string]any{
			"id":           job.ID,
			"run_id":       pipeline.ID,
			"name":         job.Name,
			"status":       status,
			"conclusion":   conclusion,
			"started_at":   githubRESTTime(job.StartedAt),
			"completed_at": githubRESTTime(job.CompletedAt),
			"html_url":     githubJobURL(r, pipeline, job),
			"steps":        steps,
		})
	}
	items, next := paginate(jobs, perPage, page)
	resp := jsonResponse(map[string]any{"total_count": len(jobs), "jobs": items})
	if next != "" {
		query.Set("per_page", strconv.Itoa(perPage))
		query.Set("page", next)
		link := fmt.Sprintf("%s/github/rest/repos/%s/actions/runs/%d/jobs?%s", c.http.base, r.Project, pipeline.ID, query.Encode())
		resp.header.Set("Link", "<"+link+`>; rel="next"`)
	}
	return resp
}

// githubRESTState spells a job status as the REST API's (status,
// conclusion): lowercase, conclusion null until completed.
func githubRESTState(status string) (string, any) {
	state, conclusion := githubCheckState(status)
	if conclusion == "" {
		return strings.ToLower(state), nil
	}
	return strings.ToLower(state), strings.ToLower(conclusion)
}

// githubRESTTime is the REST API's spelling of a time: null when absent.
func githubRESTTime(value string) any {
	if value == "" {
		return nil
	}
	return value
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

// githubCheckState spells a job status as GraphQL's CheckRun (status,
// conclusion); the conclusion is empty until the run completes.
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

func githubJobURL(r *Repo, pipeline *Pipeline, job Job) string {
	return fmt.Sprintf("https://%s/%s/actions/runs/%d/job/%d", r.Host, r.Project, pipeline.ID, job.ID)
}

func ghJobLogs(e *Engine, c *call, m []string) response {
	r := e.repoOn("github", c.http.host, m[1])
	id, _ := strconv.ParseInt(m[2], 10, 64)
	var job *Job
	if r != nil {
		job = r.job(id)
	}
	// The real endpoint answers 404 until the job's log blob exists, in
	// practice once the job completed (LogWithheld), and for a queued job.
	// A running job with a log is the window where the jobs API still
	// reports a completed job running.
	if job == nil || job.StartedAt == "" || job.Status == "pending" || job.LogWithheld {
		return ghHTTPNotFound()
	}
	// The real endpoint prepends a UTF-8 byte order mark.
	return response{stdout: append([]byte("\xef\xbb\xbf"), job.Log...), header: http.Header{"Content-Type": {"text/plain; charset=utf-8"}}}
}

// ghAttachment serves a GitHub attachment by its request URL. The URL is
// not scoped to a repository, so every seeded GitHub repository is
// searched.
func ghAttachment(e *Engine, c *call, m []string) response {
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
		stderr: "404 Not Found",
		status: http.StatusNotFound,
		header: http.Header{"Content-Type": {"application/json; charset=utf-8"}},
	}
}

func jsonResponse(value any) response {
	out, err := json.Marshal(value)
	if err != nil {
		return response{exit: 1, stderr: "ao-mockforge: encode answer: " + err.Error() + "\n"}
	}
	return response{stdout: append(out, '\n'), header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}}
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
type diffFile struct{ Additions, Deletions int }

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
			inHunk = false
		case current == nil:
		case strings.HasPrefix(line, "@@"):
			inHunk = true
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

// ghPRCreate answers `gh pr create --title T --body B [--base B] [--draft]`
// with the new pull request's URL. Without a terminal gh refuses a call
// missing either --title or --body, a head that is the base, and a head
// that already has an open pull request into the base.
func ghPRCreate(e *Engine, c *call) response {
	if len(c.positional) != 0 {
		return unhandled("pr create takes no positional arguments")
	}
	if !c.has("title") || !c.has("body") {
		return response{exit: 1, stderr: "must provide `--title` and `--body` (or `--fill` or `fill-first` or `--fillverbose`) when not running interactively\n"}
	}
	r, head, err := e.createTarget("github", c)
	if err != nil {
		return response{exit: 1, stderr: err.Error() + "\n"}
	}
	req := createRequest{title: c.flag("title"), body: c.flag("body"), base: defaultString(c.flag("base"), defaultBaseRef), draft: c.has("draft")}
	if strings.TrimSpace(req.title) == "" {
		return response{exit: 1, stderr: "GraphQL: Title can't be blank (createPullRequest)\n"}
	}
	if head.Branch == req.base {
		return response{exit: 1, stderr: fmt.Sprintf("GraphQL: No commits between %s and %s (createPullRequest)\n", req.base, head.Branch)}
	}
	if existing := r.openPullFor(head.Branch, req.base); existing != nil {
		return response{exit: 1, stderr: fmt.Sprintf("a pull request for branch %q into branch %q already exists:\n%s\n", head.Branch, req.base, githubPullURL(r, existing))}
	}
	pull, err := e.addPull(r, head, req)
	if err != nil {
		return response{exit: 1, stderr: "ao-mockforge: " + err.Error() + "\n"}
	}
	return response{stdout: []byte(githubPullURL(r, pull) + "\n")}
}
