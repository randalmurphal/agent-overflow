package forgefake

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GitLab handlers. Output shapes follow the GitLab REST API as glab
// prints it; the fields emitted are the ones internal/git parses.

var (
	gitlabStates      = map[string]string{"open": "opened", "closed": "closed", "merged": "merged"}
	gitlabMergeStatus = map[string]string{"clean": "mergeable", "conflicts": "conflict", "checking": "checking"}
)

func glabNotFound(what string) response {
	return response{
		stdout: []byte(fmt.Sprintf(`{"message":"404 %s Not Found"}`, what)),
		stderr: "glab: 404 Not Found (HTTP 404)\n",
		exit:   1,
	}
}

// glabOK answers a JSON body, with the status line and headers first when
// the caller passed --include.
func glabOK(c *call, body response, headers ...string) response {
	if !c.has("include") || body.exit != 0 {
		return body
	}
	contentType := "application/json"
	var extra []string
	for _, header := range headers {
		if value, ok := strings.CutPrefix(header, "Content-Type: "); ok {
			contentType = value
			continue
		}
		extra = append(extra, header)
	}
	head := "HTTP/2.0 200 OK\nContent-Type: " + contentType + "\n"
	for _, header := range extra {
		head += header + "\n"
	}
	body.stdout = append([]byte(head+"\n"), body.stdout...)
	return body
}

// glabProject resolves an escaped project path or numeric id.
func (e *Engine) glabProject(escaped string) (*Repo, response, bool) {
	project, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, unhandled("project %q is not path-escaped", escaped), false
	}
	r := e.repo("gitlab", project)
	if r == nil {
		return nil, glabNotFound("Project"), false
	}
	return r, response{}, true
}

func (e *Engine) glabMRFrom(m []string) (*Repo, *Pull, response, bool) {
	r, fail, ok := e.glabProject(m[1])
	if !ok {
		return nil, nil, fail, false
	}
	number, _ := strconv.Atoi(m[2])
	p := r.pull(number)
	if p == nil {
		return nil, nil, glabNotFound("Merge Request"), false
	}
	return r, p, response{}, true
}

func gitlabMRURL(r *Repo, p *Pull) string {
	return fmt.Sprintf("https://%s/%s/-/merge_requests/%d", r.Host, r.Project, p.Number)
}

func glabMR(e *Engine, c *call, m []string) response {
	r, p, fail, ok := e.glabMRFrom(m)
	if !ok {
		return fail
	}
	var pipeline any
	if p.CI != nil {
		pipeline = map[string]any{
			"id":      p.CI.ID,
			"status":  p.CI.Status,
			"web_url": fmt.Sprintf("https://%s/%s/-/pipelines/%d", r.Host, r.Project, p.CI.ID),
		}
	}
	return glabOK(c, jsonResponse(map[string]any{
		"id":                    r.ID*1000 + int64(p.Number),
		"iid":                   p.Number,
		"project_id":            r.ID,
		"title":                 p.Title,
		"description":           p.Body,
		"state":                 gitlabStates[p.State],
		"draft":                 p.Draft,
		"work_in_progress":      p.Draft,
		"source_branch":         p.HeadRef,
		"target_branch":         p.BaseRef,
		"sha":                   p.HeadSHA,
		"web_url":               gitlabMRURL(r, p),
		"author":                map[string]any{"username": p.Author},
		"changes_count":         strconv.Itoa(len(diffFiles(p.Diff))),
		"has_conflicts":         p.Mergeable == "conflicts",
		"detailed_merge_status": gitlabMergeStatus[p.Mergeable],
		"diff_refs":             map[string]any{"base_sha": p.BaseSHA, "head_sha": p.HeadSHA, "start_sha": p.StartSHA},
		"head_pipeline":         pipeline,
	}))
}

func glabApprovals(e *Engine, c *call, m []string) response {
	_, p, fail, ok := e.glabMRFrom(m)
	if !ok {
		return fail
	}
	approved := []map[string]any{}
	for _, review := range p.Reviews {
		approved = append(approved, map[string]any{
			"user":        map[string]any{"username": review.Author},
			"approved_at": review.SubmittedAt,
		})
	}
	return glabOK(c, jsonResponse(map[string]any{"approved": len(approved) > 0, "approved_by": approved}))
}

// glabPage reads per_page and page, refusing any other query parameter
// the route does not list.
func glabPage(rawQuery string, allowed ...string) (url.Values, int, int, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, 0, 0, err
	}
	for key := range query {
		if key != "per_page" && key != "page" && !slices.Contains(allowed, key) {
			return nil, 0, 0, fmt.Errorf("query parameter %q is not implemented", key)
		}
	}
	perPage, page := 20, 1
	if raw := query.Get("per_page"); raw != "" {
		if perPage, err = strconv.Atoi(raw); err != nil || perPage <= 0 || perPage > 100 {
			return nil, 0, 0, fmt.Errorf("per_page %q is not 1..100", raw)
		}
	}
	if raw := query.Get("page"); raw != "" {
		if page, err = strconv.Atoi(raw); err != nil || page <= 0 {
			return nil, 0, 0, fmt.Errorf("page %q is not positive", raw)
		}
	}
	return query, perPage, page, nil
}

// paginate slices items for page and answers the X-Next-Page header.
func paginate[T any](items []T, perPage, page int) ([]T, string) {
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(page + 1)
	}
	return items[start:end], next
}

func glabDiscussions(e *Engine, c *call, m []string) response {
	_, p, fail, ok := e.glabMRFrom(m)
	if !ok {
		return fail
	}
	_, perPage, page, err := glabPage(m[3])
	if err != nil {
		return unhandled("%v", err)
	}
	discussions := gitlabDiscussions(p)
	items, next := paginate(discussions, perPage, page)
	return glabOK(c, jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page))
}

func gitlabDiscussions(p *Pull) []map[string]any {
	out := make([]map[string]any, 0, len(p.Threads)+len(p.Comments))
	for _, thread := range p.Threads {
		headSHA := p.HeadSHA
		if thread.Outdated {
			headSHA = p.BaseSHA
		}
		position := map[string]any{
			"base_sha":      p.BaseSHA,
			"start_sha":     p.StartSHA,
			"head_sha":      headSHA,
			"old_path":      thread.Path,
			"new_path":      thread.Path,
			"position_type": "text",
			"old_line":      nil,
			"new_line":      nil,
		}
		lineKey := "new_line"
		if thread.Side == "left" {
			lineKey = "old_line"
		}
		if thread.Side == "file" {
			position["position_type"] = "file"
		} else {
			position[lineKey] = *thread.Line
		}
		if thread.StartLine != nil {
			position["line_range"] = map[string]any{
				"start": map[string]any{lineKey: *thread.StartLine, "type": "new"},
				"end":   map[string]any{lineKey: *thread.Line, "type": "new"},
			}
		}
		notes := make([]map[string]any, 0, len(thread.Comments))
		for _, comment := range thread.Comments {
			notes = append(notes, gitlabNote(comment, map[string]any{
				"type": "DiffNote", "position": position, "resolvable": true, "resolved": thread.Resolved,
			}))
		}
		out = append(out, map[string]any{"id": thread.ID, "individual_note": false, "notes": notes})
	}
	for _, comment := range p.Comments {
		out = append(out, map[string]any{
			"id":              fmt.Sprintf("%040x", comment.ID),
			"individual_note": true,
			"notes":           []map[string]any{gitlabNote(comment, map[string]any{"resolvable": false})},
		})
	}
	return out
}

func gitlabNote(comment Comment, extra map[string]any) map[string]any {
	note := map[string]any{
		"id":         comment.ID,
		"body":       comment.Body,
		"system":     false,
		"created_at": comment.CreatedAt,
		"author":     map[string]any{"username": comment.Author},
	}
	for key, value := range extra {
		note[key] = value
	}
	return note
}

// glabMRList answers the two `:fullpath` lists the app makes: the open MR
// for a source branch and recently merged MRs. glab resolves :fullpath
// from the checkout's origin remote.
func glabMRList(e *Engine, c *call, m []string) response {
	r, err := e.repoForCheckout("gitlab", c.cwd)
	if err != nil {
		return response{exit: 1, stderr: err.Error() + "\n"}
	}
	query, perPage, page, err := glabPage(m[1], "state", "source_branch", "view", "order_by", "sort")
	if err != nil {
		return unhandled("%v", err)
	}
	if view := query.Get("view"); view != "" && view != "simple" {
		return unhandled("view=%q is not implemented", view)
	}
	if order := query.Get("order_by"); order != "" && order != "updated_at" {
		return unhandled("order_by=%q is not implemented", order)
	}
	if sortOrder := query.Get("sort"); sortOrder != "" && sortOrder != "desc" {
		return unhandled("sort=%q is not implemented", sortOrder)
	}
	state := query.Get("state")
	if state != "opened" && state != "merged" {
		return unhandled("state=%q is not implemented", state)
	}
	branch := query.Get("source_branch")
	var matches []map[string]any
	// Fixture order is creation order; the API's default and the
	// updated_at order the app asks for are both newest first.
	for i := len(r.Pulls) - 1; i >= 0; i-- {
		p := &r.Pulls[i]
		if gitlabStates[p.State] != state || (branch != "" && p.HeadRef != branch) {
			continue
		}
		matches = append(matches, map[string]any{
			"iid":           p.Number,
			"title":         p.Title,
			"state":         gitlabStates[p.State],
			"web_url":       gitlabMRURL(r, p),
			"source_branch": p.HeadRef,
			"target_branch": p.BaseRef,
			"sha":           p.HeadSHA,
		})
	}
	items, next := paginate(matches, perPage, page)
	if items == nil {
		items = []map[string]any{}
	}
	return glabOK(c, jsonResponse(items), "X-Next-Page: "+next)
}

func glabPipelineJobs(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(m[1])
	if !ok {
		return fail
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	pipeline := r.pipeline(id)
	if pipeline == nil {
		return glabNotFound("Pipeline")
	}
	_, perPage, page, err := glabPage(m[3])
	if err != nil {
		return unhandled("%v", err)
	}
	jobs := make([]map[string]any, 0, len(pipeline.Jobs))
	for _, job := range pipeline.Jobs {
		var startedAt, duration any
		if job.StartedAt != "" {
			startedAt = job.StartedAt
			if seconds, ok := jobSeconds(job); ok {
				duration = seconds
			}
		}
		jobs = append(jobs, map[string]any{
			"id":            job.ID,
			"name":          job.Name,
			"stage":         job.Stage,
			"status":        job.Status,
			"duration":      duration,
			"web_url":       fmt.Sprintf("https://%s/%s/-/jobs/%d", r.Host, r.Project, job.ID),
			"allow_failure": job.AllowFailure,
			"started_at":    startedAt,
		})
	}
	// The API lists jobs newest first.
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i]["id"].(int64) > jobs[j]["id"].(int64) })
	items, next := paginate(jobs, perPage, page)
	return glabOK(c, jsonResponse(items), "X-Next-Page: "+next)
}

func jobSeconds(job Job) (float64, bool) {
	started, err1 := time.Parse(time.RFC3339, job.StartedAt)
	completed, err2 := time.Parse(time.RFC3339, job.CompletedAt)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return completed.Sub(started).Seconds(), true
}

func glabJobTrace(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(m[1])
	if !ok {
		return fail
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	job := r.job(id)
	if job == nil || job.StartedAt == "" {
		return glabNotFound("Job")
	}
	return glabOK(c, response{stdout: []byte(job.Log)}, "Content-Type: text/plain")
}

func glabUpload(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(m[1])
	if !ok {
		return fail
	}
	name, err := url.PathUnescape(m[3])
	if err != nil {
		return unhandled("upload name %q is not path-escaped", m[3])
	}
	for _, attachment := range r.Attachments {
		if attachment.Secret == m[2] && attachment.Filename == name {
			return glabOK(c, response{stdout: slices.Clone(attachment.content)}, "Content-Type: "+attachment.ContentType)
		}
	}
	return glabNotFound("File")
}

func glabMRDiff(e *Engine, c *call) response {
	project := c.flag("repo")
	if project == "" || len(c.positional) != 1 {
		return unhandled("expected one MR number and -R GROUP/REPO")
	}
	number, err := strconv.Atoi(c.positional[0])
	if err != nil || number <= 0 {
		return unhandled("MR number %q is not a positive integer", c.positional[0])
	}
	r := e.repo("gitlab", project)
	if r == nil {
		return response{exit: 1, stderr: "glab: 404 Project Not Found (HTTP 404)\n"}
	}
	p := r.pull(number)
	if p == nil {
		return response{exit: 1, stderr: "glab: 404 Not found (HTTP 404)\n"}
	}
	return response{stdout: []byte(p.Diff)}
}
