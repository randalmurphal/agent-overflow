package forgefake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GitLab writes and pipeline lists the pstack forge script makes through
// `glab api` (assets/skills/pstack/skills/poteto-mode/scripts/forge). A
// write's JSON body is decoded strictly: a key the handler does not
// implement is refused. Each changes the fixture the next read answers
// from.

// decodeRequestBody decodes a request body into target, refusing unknown keys.
func decodeRequestBody(c *call, target any) error {
	if len(bytes.TrimSpace(c.body)) == 0 {
		return fmt.Errorf("the request has no JSON body")
	}
	dec := json.NewDecoder(bytes.NewReader(c.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("request body: %w", err)
	}
	return nil
}

func glabStatus(status int, message string) response {
	out, _ := json.Marshal(map[string]string{"message": message})
	return response{stdout: out, stderr: message, status: status, header: http.Header{"Content-Type": {"application/json"}}}
}

// glabNote answers POST projects/P/merge_requests/N/notes {body}: a
// conversation note by the viewer.
func glabNote(e *Engine, c *call, m []string) response {
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := decodeRequestBody(c, &req); err != nil {
		return unhandled("%v", err)
	}
	if strings.TrimSpace(req.Body) == "" {
		return glabStatus(http.StatusBadRequest, "400 Bad request - Note {:note=>[\"can't be blank\"]}")
	}
	comment := Comment{ID: e.ids.next(), Author: e.viewer, Body: req.Body, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	p.Comments = append(p.Comments, comment)
	resp := glabOK(jsonResponse(gitlabNote(comment, map[string]any{"resolvable": false})))
	resp.status = http.StatusCreated
	return resp
}

// glabDiscussionNote answers POST
// projects/P/merge_requests/N/discussions/D/notes {body}: the viewer's
// reply in a review thread.
func glabDiscussionNote(e *Engine, c *call, m []string) response {
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	var thread *Thread
	for i := range p.Threads {
		if p.Threads[i].ID == m[3] {
			thread = &p.Threads[i]
		}
	}
	if thread == nil {
		return glabNotFound("Discussion")
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := decodeRequestBody(c, &req); err != nil {
		return unhandled("%v", err)
	}
	if strings.TrimSpace(req.Body) == "" {
		return glabStatus(http.StatusBadRequest, "400 Bad request - Note {:note=>[\"can't be blank\"]}")
	}
	comment := Comment{ID: e.ids.next(), Author: e.viewer, Body: req.Body, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	thread.Comments = append(thread.Comments, comment)
	resp := glabOK(jsonResponse(gitlabNote(comment, map[string]any{"type": "DiffNote", "resolvable": true, "resolved": thread.Resolved})))
	resp.status = http.StatusCreated
	return resp
}

// glabMerge answers PUT projects/P/merge_requests/N/merge {squash,
// auto_merge}. GitLab refuses a merge request that cannot merge now with
// 405 (406 for conflicts); with auto_merge a merge request whose pipeline
// has not passed is armed instead and answered as it stands.
func glabMerge(e *Engine, c *call, m []string) response {
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	// squash shapes the merge commit, which the fixture does not record.
	var req struct {
		Squash    *bool `json:"squash"`
		AutoMerge bool  `json:"auto_merge"`
	}
	if err := decodeRequestBody(c, &req); err != nil {
		return unhandled("%v", err)
	}
	status := gitlabDetailedMergeStatus(p)
	switch {
	case status == "conflict":
		return glabStatus(http.StatusNotAcceptable, "406 Branch cannot be merged")
	case req.AutoMerge && (status == "ci_still_running" || status == "checking"):
		p.AutoMerge = true
	case status != "mergeable":
		return glabStatus(http.StatusMethodNotAllowed, "405 Method Not Allowed")
	default:
		p.State, p.AutoMerge, p.MergedAt = "merged", false, time.Now().UTC().Format(time.RFC3339)
	}
	return glabMR(e, c, m)
}

// glabUpdateMR answers PUT projects/P/merge_requests/N {target_branch}.
func glabUpdateMR(e *Engine, c *call, m []string) response {
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	var req struct {
		TargetBranch string `json:"target_branch"`
	}
	if err := decodeRequestBody(c, &req); err != nil {
		return unhandled("%v", err)
	}
	if req.TargetBranch == "" {
		return unhandled("merge request update implements only target_branch")
	}
	if p.State != "open" {
		return glabStatus(http.StatusMethodNotAllowed, "405 Method Not Allowed")
	}
	p.BaseRef = req.TargetBranch
	return glabMR(e, c, m)
}

func gitlabPipeline(r *Repo, p *Pull) map[string]any {
	return map[string]any{
		"id":      p.CI.ID,
		"sha":     p.HeadSHA,
		"ref":     p.HeadRef,
		"status":  p.CI.Status,
		"web_url": fmt.Sprintf("https://%s/%s/-/pipelines/%d", r.Host, r.Project, p.CI.ID),
	}
}

// glabPipelines answers GET projects/P/pipelines?ref=B, newest first: the
// pipelines of the merge requests whose source branch is B.
func glabPipelines(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	query, perPage, page, err := glabPage(m[2], "ref")
	if err != nil {
		return unhandled("%v", err)
	}
	pipelines := []map[string]any{}
	for i := len(r.Pulls) - 1; i >= 0; i-- {
		p := &r.Pulls[i]
		if p.CI != nil && (!query.Has("ref") || p.HeadRef == query.Get("ref")) {
			pipelines = append(pipelines, gitlabPipeline(r, p))
		}
	}
	items, next := paginate(pipelines, perPage, page)
	return glabOK(jsonResponse(items), "X-Next-Page: "+next)
}

// glabMRPipelines answers GET projects/P/merge_requests/N/pipelines: the
// fixture's head pipeline, the only one it records.
func glabMRPipelines(e *Engine, c *call, m []string) response {
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	_, perPage, page, err := glabPage(m[3])
	if err != nil {
		return unhandled("%v", err)
	}
	pipelines := []map[string]any{}
	if p.CI != nil {
		pipelines = append(pipelines, gitlabPipeline(r, p))
	}
	items, next := paginate(pipelines, perPage, page)
	return glabOK(jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page))
}
