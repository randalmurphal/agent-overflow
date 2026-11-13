package forgefake

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Reactions on comments: GitHub reactionGroups on every comment node and
// the addReaction / removeReaction mutations; GitLab award emoji, read
// through MRNoteReactions (GraphQL) and written through the REST award
// emoji routes.

// Reaction is one user's reaction on a comment. Name is GitLab's award
// emoji name; GitHub takes the eight it shares (thumbsup, thumbsdown,
// laughing, tada, confused, heart, rocket, eyes). ID is the GitLab award
// id, generated when omitted.
type Reaction struct {
	Name string `json:"name"`
	User string `json:"user"`
	ID   int64  `json:"id,omitempty"`
}

// githubReactionContents is GitHub's ReactionContent of each shared name,
// in the order GitHub answers reactionGroups.
var githubReactionContents = []struct{ name, content string }{
	{"thumbsup", "THUMBS_UP"}, {"thumbsdown", "THUMBS_DOWN"}, {"laughing", "LAUGH"}, {"tada", "HOORAY"},
	{"confused", "CONFUSED"}, {"heart", "HEART"}, {"rocket", "ROCKET"}, {"eyes", "EYES"},
}

func githubContentName(content string) string {
	for _, c := range githubReactionContents {
		if c.content == content {
			return c.name
		}
	}
	return ""
}

var gitlabAwardName = regexp.MustCompile(`^[a-z0-9_+-]{1,64}$`)

// normalizeReactions validates and fills every comment's reactions.
func (p *Pull) normalizeReactions(forge string, ids *idSource) error {
	check := func(where string, c *Comment) error {
		seen := map[string]bool{}
		for i := range c.Reactions {
			reaction := &c.Reactions[i]
			if reaction.User == "" {
				return fmt.Errorf("%s: reactions[%d]: user is required", where, i)
			}
			if forge == "github" && !slices.ContainsFunc(githubReactionContents, func(rc struct{ name, content string }) bool { return rc.name == reaction.Name }) {
				return fmt.Errorf("%s: reactions[%d]: GitHub has no reaction %q", where, i, reaction.Name)
			}
			if !gitlabAwardName.MatchString(reaction.Name) {
				return fmt.Errorf("%s: reactions[%d]: %q is not an emoji name", where, i, reaction.Name)
			}
			key := strings.ToLower(reaction.User) + " " + reaction.Name
			if seen[key] {
				return fmt.Errorf("%s: reactions[%d]: %s reacted %s twice", where, i, reaction.User, reaction.Name)
			}
			seen[key] = true
			if reaction.ID == 0 {
				reaction.ID = ids.next()
			}
		}
		if len(c.AppliedSuggestions) > 0 && forge != "gitlab" {
			return fmt.Errorf("%s: appliedSuggestions: GitHub does not record applied suggestions", where)
		}
		return nil
	}
	for i := range p.Comments {
		if err := check(fmt.Sprintf("comments[%d]", i), &p.Comments[i]); err != nil {
			return err
		}
		for j := range p.Comments[i].Replies {
			if err := check(fmt.Sprintf("comments[%d].replies[%d]", i, j), &p.Comments[i].Replies[j]); err != nil {
				return err
			}
		}
	}
	for i := range p.Threads {
		for j := range p.Threads[i].Comments {
			if err := check(fmt.Sprintf("threads[%d].comments[%d]", i, j), &p.Threads[i].Comments[j]); err != nil {
				return err
			}
		}
	}
	return nil
}

// githubReactionGroups answers reactionGroups as GitHub does: every
// content, each with its reactors' totalCount and whether the viewer is
// one.
func (e *Engine) githubReactionGroups(comment Comment) []map[string]any {
	out := make([]map[string]any, 0, len(githubReactionContents))
	for _, rc := range githubReactionContents {
		count, viewer := 0, false
		for _, reaction := range comment.Reactions {
			if reaction.Name == rc.name {
				count++
				viewer = viewer || strings.EqualFold(reaction.User, e.viewer)
			}
		}
		out = append(out, map[string]any{"content": rc.content, "viewerHasReacted": viewer, "reactors": map[string]any{"totalCount": count}})
	}
	return out
}

// ghReactionTarget answers ReactionTarget: the comment's type, its pull
// and repository, and its reaction groups.
func ghReactionTarget(e *Engine, c *call, v graphQLVars) response {
	id, err := v.str("id")
	if err != nil {
		return unhandled("ReactionTarget: %v", err)
	}
	r, p, thread, at, ok := e.githubCommentByNode(c, id)
	if !ok {
		return graphQLNodeNotFound(id)
	}
	typename, comment := "PullRequestReviewComment", Comment{}
	if thread < 0 {
		typename, comment = "IssueComment", p.Comments[at]
	} else {
		comment = p.Threads[thread].Comments[at]
	}
	return graphQLData(map[string]any{"node": map[string]any{
		"__typename":     typename,
		"pullRequest":    map[string]any{"number": p.Number, "repository": map[string]any{"nameWithOwner": r.Project}},
		"reactionGroups": e.githubReactionGroups(comment),
	}})
}

// ghReaction answers AddReaction (add) and RemoveReaction: the viewer's
// reaction is added or removed, an add of one the viewer has and a
// removal of one they have not changing nothing, as GitHub answers both.
func ghReaction(add bool) func(e *Engine, c *call, v graphQLVars) response {
	field := "removeReaction"
	if add {
		field = "addReaction"
	}
	return func(e *Engine, c *call, v graphQLVars) response {
		id, err := v.str("subjectID")
		content, err1 := v.str("content")
		if err := firstErr(err, err1); err != nil {
			return unhandled("%s: %v", field, err)
		}
		name := githubContentName(content)
		if name == "" {
			return glabGraphQLInvalid(fmt.Sprintf("Argument 'content' on InputObject has an invalid value (%q).", content))
		}
		_, p, thread, at, ok := e.githubCommentByNode(c, id)
		if !ok {
			return graphQLMutationError(field, "NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id))
		}
		comment := &p.Comments[at]
		if thread >= 0 {
			comment = &p.Threads[thread].Comments[at]
		}
		mine := slices.IndexFunc(comment.Reactions, func(r Reaction) bool { return r.Name == name && strings.EqualFold(r.User, e.viewer) })
		switch {
		case add && mine < 0:
			comment.Reactions = append(comment.Reactions, Reaction{Name: name, User: e.viewer, ID: e.ids.next()})
		case !add && mine >= 0:
			comment.Reactions = slices.Delete(comment.Reactions, mine, mine+1)
		}
		return graphQLData(map[string]any{field: map[string]any{"reaction": map[string]any{"content": content}}})
	}
}

// gitlabNoteByID finds a note of the merge request by id: a thread's, a
// conversation comment or one of its replies.
func gitlabNoteByID(p *Pull, id int64) *Comment {
	for i := range p.Threads {
		for j := range p.Threads[i].Comments {
			if p.Threads[i].Comments[j].ID == id {
				return &p.Threads[i].Comments[j]
			}
		}
	}
	for i := range p.Comments {
		if p.Comments[i].ID == id {
			return &p.Comments[i]
		}
		for j := range p.Comments[i].Replies {
			if p.Comments[i].Replies[j].ID == id {
				return &p.Comments[i].Replies[j]
			}
		}
	}
	return nil
}

// glabAwardNote resolves the note an award route names under its merge
// request; a note of another is GitLab's 404.
func (e *Engine) glabAwardNote(c *call, m []string) (*Comment, response, bool) {
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return nil, fail, false
	}
	id, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return nil, unhandled("note id %q", m[3]), false
	}
	note := gitlabNoteByID(p, id)
	if note == nil {
		return nil, glabNotFound("Note"), false
	}
	return note, response{}, true
}

func gitlabAwardJSON(note *Comment, reaction Reaction) map[string]any {
	return map[string]any{
		"id": reaction.ID, "name": reaction.Name, "user": gitlabUser(reaction.User, reaction.User),
		"awardable_id": note.ID, "awardable_type": "Note",
	}
}

// glabNoteAwards lists a note's awards, X-Next-Page while more remain.
func glabNoteAwards(e *Engine, c *call, m []string) response {
	note, fail, ok := e.glabAwardNote(c, m)
	if !ok {
		return fail
	}
	_, perPage, page, err := glabPage(m[4])
	if err != nil {
		return unhandled("%v", err)
	}
	all := make([]map[string]any, 0, len(note.Reactions))
	for _, reaction := range note.Reactions {
		all = append(all, gitlabAwardJSON(note, reaction))
	}
	items, next := paginate(all, perPage, page)
	return glabOK(jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page))
}

// glabCreateNoteAward awards name as the viewer: a blank name is GitLab's
// 400, one the viewer already gave its 404.
func glabCreateNoteAward(e *Engine, c *call, m []string) response {
	note, fail, ok := e.glabAwardNote(c, m)
	if !ok {
		return fail
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("create award: %v", err)
	}
	switch {
	case strings.TrimSpace(in.Name) == "":
		resp := jsonResponse(map[string]any{"message": map[string]any{"name": []string{"can't be blank"}}})
		resp.status = http.StatusBadRequest
		return resp
	case !gitlabAwardName.MatchString(in.Name):
		return glabStatus(http.StatusNotFound, "404 Award Emoji Name is not a valid emoji name Not Found")
	case slices.ContainsFunc(note.Reactions, func(r Reaction) bool { return r.Name == in.Name && strings.EqualFold(r.User, e.viewer) }):
		return glabStatus(http.StatusNotFound, "404 Award Emoji Name has already been taken Not Found")
	}
	reaction := Reaction{Name: in.Name, User: e.viewer, ID: e.ids.next()}
	note.Reactions = append(note.Reactions, reaction)
	resp := jsonResponse(gitlabAwardJSON(note, reaction))
	resp.status = http.StatusCreated
	return resp
}

// glabDeleteNoteAward removes an award: its author's only (GitLab lets an
// administrator too, which the fixture has none of), 404 for an unknown
// id.
func glabDeleteNoteAward(e *Engine, c *call, m []string) response {
	note, fail, ok := e.glabAwardNote(c, m)
	if !ok {
		return fail
	}
	if err := noRESTBody(c); err != nil {
		return unhandled("delete award: %v", err)
	}
	id, _ := strconv.ParseInt(m[4], 10, 64)
	at := slices.IndexFunc(note.Reactions, func(r Reaction) bool { return r.ID == id })
	switch {
	case at < 0:
		return glabNotFound("Award Emoji")
	case !strings.EqualFold(note.Reactions[at].User, e.viewer):
		return glabForbidden()
	}
	note.Reactions = slices.Delete(note.Reactions, at, at+1)
	return response{status: http.StatusNoContent}
}

// glabNoteReactions answers MRNoteReactions: every note of the merge
// request (its threads', its conversation comments and replies, its
// system notes) in pages of 100 from after, each with its awards in one
// page of 100. A server that lacks Note.awardEmoji (ServerLacks
// "awardEmoji") refuses the document as such a server does.
func glabNoteReactions(e *Engine, c *call, v graphQLVars) response {
	r, p, fail, ok := e.glabGraphQLMR(c, v, "fullPath")
	if !ok {
		return fail
	}
	if r.lacks(lacksAwardEmoji) {
		return glabGraphQLInvalid("Field 'awardEmoji' doesn't exist on type 'Note'")
	}
	offset, err := v.cursor("after")
	if err != nil {
		return unhandled("MRNoteReactions: %v", err)
	}
	node := func(kind string, note Comment) map[string]any {
		awards := make([]map[string]any, 0, len(note.Reactions))
		for _, reaction := range note.Reactions {
			awards = append(awards, map[string]any{"name": reaction.Name, "user": map[string]any{"username": reaction.User}})
		}
		return map[string]any{"id": fmt.Sprintf("gid://gitlab/%s/%d", kind, note.ID), "awardEmoji": graphQLConnection(awards, 0, githubGraphQLPageSize)}
	}
	var notes []map[string]any
	for _, thread := range p.Threads {
		for _, comment := range thread.Comments {
			notes = append(notes, node("DiffNote", comment))
		}
	}
	for _, comment := range p.Comments {
		notes = append(notes, node("Note", comment))
		for _, reply := range comment.Replies {
			notes = append(notes, node("DiscussionNote", reply))
		}
	}
	for _, event := range p.Events {
		notes = append(notes, node("Note", Comment{ID: event.ID}))
	}
	return graphQLData(map[string]any{"project": map[string]any{"mergeRequest": map[string]any{
		"notes": graphQLConnection(notes, offset, githubGraphQLPageSize),
	}}})
}
