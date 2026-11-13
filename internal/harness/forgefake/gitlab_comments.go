package forgefake

import (
	"crypto/sha1"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// GitLab's comment actions (https://docs.gitlab.com/api/discussions/): a
// positioned discussion or a note posted now, a note added to a
// discussion, and a note's edit and delete. A note's author may edit it;
// its author, a maintainer or an owner may delete it. A discussion whose
// last note is deleted goes with it.

// gitlabAnchor is where a position puts a note, as a Thread records it.
type gitlabAnchor struct {
	Path                     string
	Side                     string
	Line, StartLine, OldLine *int
}

// gitlabPositionAnchor reads a position: a file, or a line with an
// optional line_range whose points carry their line codes. A position
// GitLab refuses is its 400; one the fake does not model, or a line code
// that is not the path's, is unhandled.
func gitlabPositionAnchor(pos *gitlabDraftPositionIn) (gitlabAnchor, response, bool) {
	if pos.OldPath != pos.NewPath || pos.NewPath == "" {
		return gitlabAnchor{}, unhandled("position names old_path %q and new_path %q; the fake models one path", pos.OldPath, pos.NewPath), false
	}
	anchor := gitlabAnchor{Path: pos.NewPath}
	switch {
	case pos.PositionType == "file" && pos.OldLine == nil && pos.NewLine == nil && pos.LineRange == nil:
		anchor.Side = "file"
		return anchor, response{}, true
	case pos.PositionType != "text" || (pos.OldLine == nil && pos.NewLine == nil):
		return gitlabAnchor{}, glabStatus(http.StatusBadRequest, "position is invalid"), false
	case pos.OldLine != nil && pos.NewLine != nil:
		// An unchanged line: GitLab names it by both numbers.
		anchor.Side, anchor.Line, anchor.OldLine = "right", pos.NewLine, pos.OldLine
	case pos.OldLine != nil:
		anchor.Side, anchor.Line = "left", pos.OldLine
	default:
		anchor.Side, anchor.Line = "right", pos.NewLine
	}
	if pos.LineRange == nil {
		return anchor, response{}, true
	}
	for _, point := range []gitlabDraftLinePoint{pos.LineRange.Start, pos.LineRange.End} {
		if reason := gitlabPointDrift(pos.NewPath, point); reason != "" {
			return gitlabAnchor{}, unhandled("line_range: %s", reason), false
		}
	}
	end := pos.LineRange.End
	if !samePtr(end.OldLine, pos.OldLine) || !samePtr(end.NewLine, pos.NewLine) {
		return gitlabAnchor{}, unhandled("line_range end does not name the position's line"), false
	}
	start := pos.LineRange.Start.NewLine
	if anchor.Side == "left" {
		start = pos.LineRange.Start.OldLine
	}
	if start == nil {
		return gitlabAnchor{}, unhandled("line_range has no start line on side %s; the fake models a range on one half", anchor.Side), false
	}
	anchor.StartLine = start
	return anchor, response{}, true
}

// gitlabPointDrift is why a line_range point is not one GitLab's diff
// would name: its line code is sha1(path)_old_new, 0 for the half it is
// not on, and its type is "new" for an added line, "old" for a removed
// one and absent for an unchanged one.
func gitlabPointDrift(path string, point gitlabDraftLinePoint) string {
	value := func(n *int) int {
		if n == nil {
			return 0
		}
		return *n
	}
	want := fmt.Sprintf("%x_%d_%d", sha1.Sum([]byte(path)), value(point.OldLine), value(point.NewLine))
	if point.LineCode != want {
		return fmt.Sprintf("line_code %q, want %q", point.LineCode, want)
	}
	wantType := map[[2]bool]string{{false, true}: "new", {true, false}: "old", {true, true}: ""}[[2]bool{point.OldLine != nil, point.NewLine != nil}]
	if point.OldLine == nil && point.NewLine == nil {
		return "a point names no line"
	}
	if point.Type != wantType {
		return fmt.Sprintf("type %q, want %q", point.Type, wantType)
	}
	return ""
}

func samePtr(a, b *int) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func glabCreated(value any) response {
	resp := glabOK(jsonResponse(value))
	resp.status = http.StatusCreated
	return resp
}

// gitlabConversationID is the discussion id of a conversation comment.
func gitlabConversationID(comment Comment) string {
	if comment.discussion != "" {
		return comment.discussion
	}
	return fmt.Sprintf("%040x", comment.ID)
}

// glabCreateDiscussion starts a positioned discussion now: `{body,
// position}` against the merge request's current diff. A position whose
// shas are not the merge request's is GitLab's 400.
func glabCreateDiscussion(e *Engine, c *call, m []string) response {
	var in struct {
		Body     string                 `json:"body"`
		Position *gitlabDraftPositionIn `json:"position"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	if in.Position == nil {
		return unhandled("a discussion without a position: the app posts a note")
	}
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	if strings.TrimSpace(in.Body) == "" {
		return glabStatus(http.StatusBadRequest, "body is missing")
	}
	pos := in.Position
	if pos.BaseSHA != p.BaseSHA || pos.StartSHA != p.StartSHA || pos.HeadSHA != p.HeadSHA {
		return glabStatus(http.StatusBadRequest, "Note {:position=>[\"is invalid\"]}")
	}
	anchor, fail, ok := gitlabPositionAnchor(pos)
	if !ok {
		return fail
	}
	comment := Comment{ID: e.ids.next(), Author: e.viewer, Body: in.Body, CreatedAt: glabNow()}
	thread := Thread{ID: e.ids.sha(), Path: anchor.Path, Side: anchor.Side, Line: anchor.Line, StartLine: anchor.StartLine, OldLine: anchor.OldLine, Comments: []Comment{comment}}
	p.Threads = append(p.Threads, thread)
	return glabCreated(map[string]any{"id": thread.ID, "individual_note": false, "notes": []map[string]any{gitlabNote(comment, map[string]any{"type": "DiffNote"})}})
}

// glabCreateNote posts a note on the merge request itself.
func glabCreateNote(e *Engine, c *call, m []string) response {
	var in struct {
		Body string `json:"body"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	if strings.TrimSpace(in.Body) == "" {
		return glabStatus(http.StatusBadRequest, "body is missing")
	}
	comment := Comment{ID: e.ids.next(), Author: e.viewer, Body: in.Body, CreatedAt: glabNow()}
	p.Comments = append(p.Comments, comment)
	return glabCreated(gitlabNote(comment, nil))
}

// gitlabDiscussionNotes is the notes of discussion id: a thread's
// comments, or a conversation comment's replies with the comment itself
// at index -1 (root). ok is false when the merge request has no such
// discussion.
type gitlabDiscussionRef struct {
	thread       int // index in p.Threads, or -1
	conversation int // index in p.Comments, or -1
}

func gitlabFindDiscussion(p *Pull, id string) (gitlabDiscussionRef, bool) {
	if at := slices.IndexFunc(p.Threads, func(t Thread) bool { return t.ID == id }); at >= 0 {
		return gitlabDiscussionRef{thread: at, conversation: -1}, true
	}
	if at := slices.IndexFunc(p.Comments, func(c Comment) bool { return gitlabConversationID(c) == id }); at >= 0 {
		return gitlabDiscussionRef{thread: -1, conversation: at}, true
	}
	return gitlabDiscussionRef{}, false
}

// gitlabAppendNote adds comment to the discussion ref names.
func gitlabAppendNote(p *Pull, ref gitlabDiscussionRef, comment Comment) {
	if ref.thread >= 0 {
		p.Threads[ref.thread].Comments = append(p.Threads[ref.thread].Comments, comment)
		return
	}
	p.Comments[ref.conversation].Replies = append(p.Comments[ref.conversation].Replies, comment)
}

// glabCreateDiscussionNote adds a note to a discussion; an unknown one is
// GitLab's 404.
func glabCreateDiscussionNote(e *Engine, c *call, m []string) response {
	var in struct {
		Body string `json:"body"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	ref, found := gitlabFindDiscussion(p, m[3])
	if !found {
		return glabNotFound("Discussion")
	}
	if strings.TrimSpace(in.Body) == "" {
		return glabStatus(http.StatusBadRequest, "body is missing")
	}
	comment := Comment{ID: e.ids.next(), Author: e.viewer, Body: in.Body, CreatedAt: glabNow()}
	gitlabAppendNote(p, ref, comment)
	return glabCreated(gitlabNote(comment, nil))
}

// glabNoteTarget resolves `.../discussions/D/notes/N` to the note's
// comment and a delete of it.
func (e *Engine) glabNoteTarget(c *call, m []string) (*Repo, *Comment, func(), response, bool) {
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return nil, nil, nil, fail, false
	}
	noteID, err := strconv.ParseInt(m[4], 10, 64)
	if err != nil {
		return nil, nil, nil, unhandled("note id %q", m[4]), false
	}
	ref, found := gitlabFindDiscussion(p, m[3])
	if !found {
		return nil, nil, nil, glabNotFound("Note"), false
	}
	byID := func(c Comment) bool { return c.ID == noteID }
	if ref.thread >= 0 {
		thread := &p.Threads[ref.thread]
		at := slices.IndexFunc(thread.Comments, byID)
		if at < 0 {
			return nil, nil, nil, glabNotFound("Note"), false
		}
		return r, &thread.Comments[at], func() {
			thread.Comments = slices.Delete(thread.Comments, at, at+1)
			if len(thread.Comments) == 0 {
				p.Threads = slices.Delete(p.Threads, ref.thread, ref.thread+1)
			}
		}, response{}, true
	}
	root := &p.Comments[ref.conversation]
	if root.ID == noteID {
		return r, root, func() {
			if len(root.Replies) == 0 {
				p.Comments = slices.Delete(p.Comments, ref.conversation, ref.conversation+1)
				return
			}
			// The discussion stays with its id; its first reply leads it.
			id := gitlabConversationID(*root)
			next := root.Replies[0]
			next.Replies, next.discussion = root.Replies[1:], id
			*root = next
		}, response{}, true
	}
	at := slices.IndexFunc(root.Replies, byID)
	if at < 0 {
		return nil, nil, nil, glabNotFound("Note"), false
	}
	return r, &root.Replies[at], func() { root.Replies = slices.Delete(root.Replies, at, at+1) }, response{}, true
}

func glabForbidden() response {
	return glabStatus(http.StatusForbidden, "403 Forbidden")
}

// glabUpdateDiscussionNote replaces a note's body; only its author may.
func glabUpdateDiscussionNote(e *Engine, c *call, m []string) response {
	var in struct {
		Body string `json:"body"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	_, comment, _, fail, ok := e.glabNoteTarget(c, m)
	if !ok {
		return fail
	}
	if !strings.EqualFold(comment.Author, e.viewer) {
		return glabForbidden()
	}
	if strings.TrimSpace(in.Body) == "" {
		return glabStatus(http.StatusBadRequest, "body is missing")
	}
	comment.Body, comment.EditedAt = in.Body, glabNow()
	return glabOK(jsonResponse(gitlabNote(*comment, nil)))
}

// glabDeleteDiscussionNote deletes a note: its author's, or any as a
// maintainer or owner.
func glabDeleteDiscussionNote(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	r, comment, remove, fail, ok := e.glabNoteTarget(c, m)
	if !ok {
		return fail
	}
	if !strings.EqualFold(comment.Author, e.viewer) && gitlabRoles[r.Merge.Permission] < gitlabRoles["maintainer"] {
		return glabForbidden()
	}
	remove()
	return response{status: http.StatusNoContent}
}
