package forgefake

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// GitLab draft notes (https://docs.gitlab.com/api/draft_notes/): a review
// creates the viewer's drafts one at a time, may delete one by id, and
// publishes all of the viewer's at once.

type gitlabDraftPositionIn struct {
	BaseSHA      string                `json:"base_sha"`
	StartSHA     string                `json:"start_sha"`
	HeadSHA      string                `json:"head_sha"`
	OldPath      string                `json:"old_path"`
	NewPath      string                `json:"new_path"`
	PositionType string                `json:"position_type"`
	OldLine      *int                  `json:"old_line"`
	NewLine      *int                  `json:"new_line"`
	LineRange    *gitlabDraftLineRange `json:"line_range"`
}

type gitlabDraftLineRange struct {
	Start gitlabDraftLinePoint `json:"start"`
	End   gitlabDraftLinePoint `json:"end"`
}

type gitlabDraftLinePoint struct {
	LineCode string `json:"line_code"`
	Type     string `json:"type"`
	OldLine  *int   `json:"old_line"`
	NewLine  *int   `json:"new_line"`
}

// glabDraftCreate adds one of the viewer's draft notes: a summary with no
// position, one positioned on a line, a line range or a file, or a reply
// to a discussion (404 when the merge request has none by that id).
func glabDraftCreate(e *Engine, c *call, m []string) response {
	var in struct {
		Note      string                 `json:"note"`
		Position  *gitlabDraftPositionIn `json:"position"`
		InReplyTo string                 `json:"in_reply_to_discussion_id"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	if strings.TrimSpace(in.Note) == "" {
		return glabStatus(http.StatusBadRequest, "note is missing")
	}
	draft := DraftNote{Author: e.viewer, Note: in.Note, InReplyTo: in.InReplyTo}
	switch {
	case in.InReplyTo != "" && in.Position != nil:
		return unhandled("a draft reply carries no position")
	case in.InReplyTo != "":
		if _, found := gitlabFindDiscussion(p, in.InReplyTo); !found {
			return glabNotFound("Discussion")
		}
	case in.Position != nil:
		anchor, fail, ok := gitlabPositionAnchor(in.Position)
		if !ok {
			return fail
		}
		draft.Path, draft.Side, draft.Line, draft.StartLine, draft.OldLine = anchor.Path, anchor.Side, anchor.Line, anchor.StartLine, anchor.OldLine
	}
	draft.ID = e.ids.next()
	p.DraftNotes = append(p.DraftNotes, draft)
	resp := glabOK(jsonResponse(gitlabDraftJSON(r, p, draft, in.Position)))
	resp.status = http.StatusCreated
	return resp
}

func gitlabDraftJSON(r *Repo, p *Pull, draft DraftNote, position *gitlabDraftPositionIn) map[string]any {
	out := map[string]any{
		"id":                 draft.ID,
		"author_id":          1,
		"merge_request_id":   r.ID*1000 + int64(p.Number),
		"resolve_discussion": false,
		"discussion_id":      nullable(draft.InReplyTo),
		"note":               draft.Note,
		"commit_id":          nil,
		"line_code":          nil,
		"position":           nil,
	}
	if position != nil {
		out["position"] = position
	}
	return out
}

// glabDraftList lists the viewer's pending draft notes, one page: the
// fake's merge requests hold fewer than a page.
func glabDraftList(e *Engine, c *call, m []string) response {
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	out := []map[string]any{}
	for _, draft := range p.DraftNotes {
		if strings.EqualFold(draft.Author, e.viewer) {
			out = append(out, gitlabDraftJSON(r, p, draft, nil))
		}
	}
	return glabOK(jsonResponse(out))
}

// glabDraftDelete deletes one of the viewer's draft notes by id; another
// id, the viewer's published or another author's, is GitLab's 404.
func glabDraftDelete(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	id, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return unhandled("draft note id %q", m[3])
	}
	at := slices.IndexFunc(p.DraftNotes, func(d DraftNote) bool { return d.ID == id && strings.EqualFold(d.Author, e.viewer) })
	if at < 0 {
		return glabNotFound("Draft Note")
	}
	p.DraftNotes = slices.Delete(p.DraftNotes, at, at+1)
	return response{status: http.StatusNoContent}
}

// glabDraftPublish publishes every one of the viewer's draft notes: each
// reply joins its discussion, each positioned one becomes a thread, each
// other a conversation comment.
// Another author's drafts stay pending.
func glabDraftPublish(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	// A reply whose discussion is gone fails the publish before any note
	// is published.
	for _, draft := range p.DraftNotes {
		if _, found := gitlabFindDiscussion(p, draft.InReplyTo); draft.InReplyTo != "" && strings.EqualFold(draft.Author, e.viewer) && !found {
			return glabNotFound("Discussion")
		}
	}
	now := glabNow()
	kept := p.DraftNotes[:0]
	for _, draft := range p.DraftNotes {
		if !strings.EqualFold(draft.Author, e.viewer) {
			kept = append(kept, draft)
			continue
		}
		comment := Comment{ID: e.ids.next(), Author: draft.Author, Body: draft.Note, CreatedAt: now}
		if draft.InReplyTo != "" {
			ref, _ := gitlabFindDiscussion(p, draft.InReplyTo)
			gitlabAppendNote(p, ref, comment)
			continue
		}
		if draft.Path == "" {
			p.Comments = append(p.Comments, comment)
			continue
		}
		p.Threads = append(p.Threads, Thread{
			ID: e.ids.sha(), Path: draft.Path, Line: draft.Line, StartLine: draft.StartLine, OldLine: draft.OldLine, Side: draft.Side,
			Comments: []Comment{comment},
		})
	}
	p.DraftNotes = kept
	return response{status: http.StatusNoContent}
}
