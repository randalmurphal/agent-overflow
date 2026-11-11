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
// position, or one positioned on a line, a line range or a file.
func glabDraftCreate(e *Engine, c *call, m []string) response {
	var in struct {
		Note     string                 `json:"note"`
		Position *gitlabDraftPositionIn `json:"position"`
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
	draft := DraftNote{Author: e.viewer, Note: in.Note}
	if pos := in.Position; pos != nil {
		if pos.OldPath != pos.NewPath || pos.NewPath == "" {
			return unhandled("draft position names old_path %q and new_path %q; the fake models one path", pos.OldPath, pos.NewPath)
		}
		draft.Path = pos.NewPath
		switch {
		case pos.PositionType == "file" && pos.OldLine == nil && pos.NewLine == nil && pos.LineRange == nil:
			draft.Side = "file"
		case pos.PositionType == "text" && (pos.OldLine == nil) != (pos.NewLine == nil):
			draft.Side, draft.Line = "right", pos.NewLine
			if pos.OldLine != nil {
				draft.Side, draft.Line = "left", pos.OldLine
			}
			if pos.LineRange != nil {
				start := pos.LineRange.Start.NewLine
				if draft.Side == "left" {
					start = pos.LineRange.Start.OldLine
				}
				if start == nil {
					return unhandled("draft line_range has no start line on side %s", draft.Side)
				}
				draft.StartLine = start
			}
		default:
			return glabStatus(http.StatusBadRequest, "position is invalid")
		}
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
		"discussion_id":      nil,
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
// positioned one becomes a thread, each other a conversation comment.
// Another author's drafts stay pending.
func glabDraftPublish(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	now := glabNow()
	kept := p.DraftNotes[:0]
	for _, draft := range p.DraftNotes {
		if !strings.EqualFold(draft.Author, e.viewer) {
			kept = append(kept, draft)
			continue
		}
		comment := Comment{ID: e.ids.next(), Author: draft.Author, Body: draft.Note, CreatedAt: now}
		if draft.Path == "" {
			p.Comments = append(p.Comments, comment)
			continue
		}
		p.Threads = append(p.Threads, Thread{
			ID: e.ids.sha(), Path: draft.Path, Line: draft.Line, StartLine: draft.StartLine, Side: draft.Side,
			Comments: []Comment{comment},
		})
	}
	p.DraftNotes = kept
	return response{status: http.StatusNoContent}
}
