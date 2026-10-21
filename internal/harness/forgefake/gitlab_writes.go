package forgefake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"
)

// The GitLab discussion note the app writes over HTTP. A write's JSON
// body is decoded strictly: a key the handler does not implement is
// refused. It changes the fixture the next read answers from.

// decodeRequestBody decodes a request body into target, refusing unknown
// keys and a body whose Content-Type is not application/json.
func decodeRequestBody(c *call, target any) error {
	if len(bytes.TrimSpace(c.body)) == 0 {
		return fmt.Errorf("the request has no JSON body")
	}
	if mediaType, _, err := mime.ParseMediaType(c.contentType); err != nil || mediaType != "application/json" {
		return fmt.Errorf("the request body's Content-Type is %q, not application/json", c.contentType)
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
