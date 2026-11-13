package forgefake

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// GitHub's comment actions: review comments and replies posted on their
// own and conversation (issue) comments over REST, edits and deletes as
// GraphQL mutations on a comment's node id. A comment's author may edit
// it; its author, a maintainer or an admin may delete it, as on GitHub.

// githubCommentViewerFields answers `lastEditedAt url viewerCanUpdate
// viewerCanDelete` for one comment at url.
func (e *Engine) githubCommentViewerFields(r *Repo, comment Comment, url string) map[string]any {
	return map[string]any{
		"lastEditedAt":    nullable(comment.EditedAt),
		"url":             url,
		"viewerCanUpdate": e.githubMayEdit(comment),
		"viewerCanDelete": e.githubMayDelete(r, comment),
	}
}

func (e *Engine) githubMayEdit(comment Comment) bool {
	return strings.EqualFold(comment.Author, e.viewer)
}

func (e *Engine) githubMayDelete(r *Repo, comment Comment) bool {
	return e.githubMayEdit(comment) || githubRoles[r.Merge.Permission] >= githubRoles["MAINTAIN"]
}

// githubCommentByNode finds a comment by node id among the GitHub
// repositories on the request's host: a review comment (PRRC_) in its
// thread, or a conversation comment (IC_) of its pull. thread is -1 for a
// conversation comment. Callers hold mu.
func (e *Engine) githubCommentByNode(c *call, id string) (r *Repo, p *Pull, thread, at int, ok bool) {
	prefix, number, found := strings.Cut(id, "_ao")
	databaseID, err := strconv.ParseInt(number, 10, 64)
	if !found || err != nil || (prefix != "PRRC" && prefix != "IC") {
		return nil, nil, 0, 0, false
	}
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "github" || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		for i := range r.Pulls {
			p := &r.Pulls[i]
			if prefix == "IC" {
				if at := slices.IndexFunc(p.Comments, func(c Comment) bool { return c.ID == databaseID }); at >= 0 {
					return r, p, -1, at, true
				}
				continue
			}
			for t := range p.Threads {
				if at := slices.IndexFunc(p.Threads[t].Comments, func(c Comment) bool { return c.ID == databaseID }); at >= 0 {
					return r, p, t, at, true
				}
			}
		}
	}
	return nil, nil, 0, 0, false
}

// ghCommentOwner answers `node(id)` for a comment: its type and the pull
// request and repository it is on. An id no repository on the host has is
// GitHub's NOT_FOUND beside a null node.
func ghCommentOwner(e *Engine, c *call, v graphQLVars) response {
	id, err := v.str("id")
	if err != nil {
		return unhandled("CommentOwner: %v", err)
	}
	r, p, thread, _, ok := e.githubCommentByNode(c, id)
	if !ok {
		return graphQLNotFound(map[string]any{"node": nil}, []string{"node"}, fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id))
	}
	typename := "PullRequestReviewComment"
	if thread < 0 {
		typename = "IssueComment"
	}
	return graphQLData(map[string]any{"node": map[string]any{
		"__typename":  typename,
		"pullRequest": map[string]any{"number": p.Number, "repository": map[string]any{"nameWithOwner": r.Project}},
	}})
}

// ghCommentMutation is one of the four comment mutations: field is its
// payload, issue whether it names a conversation comment, update whether
// it edits (else deletes).
func ghCommentMutation(e *Engine, c *call, v graphQLVars, field, operation string, issue, update bool) response {
	id, err := v.str("id")
	body := ""
	if update {
		var err1 error
		body, err1 = v.str("body")
		err = firstErr(err, err1)
	}
	if err != nil {
		return unhandled("%s: %v", field, err)
	}
	r, p, thread, at, ok := e.githubCommentByNode(c, id)
	if !ok || (thread < 0) != issue {
		return graphQLMutationError(field, "NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id))
	}
	comments := &p.Comments
	if thread >= 0 {
		comments = &p.Threads[thread].Comments
	}
	comment := &(*comments)[at]
	allowed := e.githubMayDelete(r, *comment)
	if update {
		allowed = e.githubMayEdit(*comment)
	}
	if !allowed {
		return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `%s`", e.viewer, operation))
	}
	if !update {
		*comments = slices.Delete(*comments, at, at+1)
		// A thread whose only comment is deleted goes with it.
		if thread >= 0 && len(p.Threads[thread].Comments) == 0 {
			p.Threads = slices.Delete(p.Threads, thread, thread+1)
		}
		return graphQLData(map[string]any{field: map[string]any{"clientMutationId": nil}})
	}
	if strings.TrimSpace(body) == "" {
		return graphQLMutationError(field, "UNPROCESSABLE", "Body can't be blank")
	}
	comment.Body, comment.EditedAt = body, githubNow()
	node := map[string]string{"updatePullRequestReviewComment": "pullRequestReviewComment", "updateIssueComment": "issueComment"}[field]
	return graphQLData(map[string]any{field: map[string]any{node: map[string]any{"id": id}}})
}

func ghUpdateReviewComment(e *Engine, c *call, v graphQLVars) response {
	return ghCommentMutation(e, c, v, "updatePullRequestReviewComment", "UpdatePullRequestReviewComment", false, true)
}

func ghUpdateIssueComment(e *Engine, c *call, v graphQLVars) response {
	return ghCommentMutation(e, c, v, "updateIssueComment", "UpdateIssueComment", true, true)
}

func ghDeleteReviewComment(e *Engine, c *call, v graphQLVars) response {
	return ghCommentMutation(e, c, v, "deletePullRequestReviewComment", "DeletePullRequestReviewComment", false, false)
}

func ghDeleteIssueComment(e *Engine, c *call, v graphQLVars) response {
	return ghCommentMutation(e, c, v, "deleteIssueComment", "DeleteIssueComment", true, false)
}

// ghRESTPull resolves the pull a REST path names on the request's host.
func (e *Engine) ghRESTPull(c *call, project, number string) (*Repo, *Pull, bool) {
	r := e.repoOn("github", c.http.host, project)
	if r == nil {
		return nil, nil, false
	}
	n, _ := strconv.Atoi(number)
	p := r.pull(n)
	return r, p, p != nil
}

// ghCreated is a 201 with the created comment.
func ghCreated(value map[string]any) response {
	resp := jsonResponse(value)
	resp.status = http.StatusCreated
	return resp
}

// ghPullComment posts one review comment on its own: `{body, commit_id,
// path}` with `line, side[, start_line, start_side]` for a line or range,
// or `subject_type: "file"` for the file. It opens a thread of the
// viewer's. commit_id must be the pull's head: the fake models no older
// commit of the pull, where GitHub would accept one and place the comment
// against it.
func ghPullComment(e *Engine, c *call, m []string) response {
	var in struct {
		Body        string `json:"body"`
		CommitID    string `json:"commit_id"`
		Path        string `json:"path"`
		Line        int    `json:"line"`
		Side        string `json:"side"`
		StartLine   *int   `json:"start_line"`
		StartSide   string `json:"start_side"`
		SubjectType string `json:"subject_type"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	if in.SubjectType != "" && in.SubjectType != "file" {
		return unhandled("subject_type %q is not file", in.SubjectType)
	}
	r, p, ok := e.ghRESTPull(c, m[1], m[2])
	if !ok {
		return ghHTTPNotFound()
	}
	if in.CommitID != p.HeadSHA {
		return ghUnprocessable("commit_id is not part of the pull request")
	}
	var thread Thread
	if in.SubjectType == "file" {
		if in.Line != 0 || in.Side != "" || in.StartLine != nil || in.StartSide != "" {
			return ghUnprocessable("a file comment has no line")
		}
		if strings.TrimSpace(in.Path) == "" || strings.TrimSpace(in.Body) == "" {
			return ghUnprocessable("path and body are required for a review comment")
		}
		thread = Thread{ID: fmt.Sprintf("PRRT_ao%d", e.ids.next()), Path: in.Path, Side: "file",
			Comments: []Comment{{ID: e.ids.next(), Author: e.viewer, Body: in.Body, CreatedAt: githubNow()}}}
	} else {
		var reason string
		thread, reason = ghReviewLineComment{Path: in.Path, Body: in.Body, Line: in.Line, Side: in.Side, StartLine: in.StartLine, StartSide: in.StartSide}.thread(e)
		if reason != "" {
			return ghUnprocessable(reason)
		}
	}
	p.Threads = append(p.Threads, thread)
	comment := thread.Comments[0]
	return ghCreated(map[string]any{
		"id": comment.ID, "node_id": githubCommentNodeID("PRRC", comment.ID), "path": thread.Path, "body": comment.Body,
		"commit_id": in.CommitID, "user": map[string]any{"login": comment.Author}, "created_at": comment.CreatedAt,
		"html_url": fmt.Sprintf("%s#discussion_r%d", githubPullURL(r, p), comment.ID),
	})
}

// ghPullCommentReply replies to a review thread, named by its first
// comment's id (GitHub takes no reply to a reply); another id is a 404.
func ghPullCommentReply(e *Engine, c *call, m []string) response {
	var in struct {
		Body string `json:"body"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	r, p, ok := e.ghRESTPull(c, m[1], m[2])
	if !ok {
		return ghHTTPNotFound()
	}
	root, _ := strconv.ParseInt(m[3], 10, 64)
	at := slices.IndexFunc(p.Threads, func(t Thread) bool { return len(t.Comments) > 0 && t.Comments[0].ID == root })
	if at < 0 {
		return ghHTTPNotFound()
	}
	if strings.TrimSpace(in.Body) == "" {
		return ghUnprocessable("body is required")
	}
	comment := Comment{ID: e.ids.next(), Author: e.viewer, Body: in.Body, CreatedAt: githubNow()}
	p.Threads[at].Comments = append(p.Threads[at].Comments, comment)
	return ghCreated(map[string]any{
		"id": comment.ID, "node_id": githubCommentNodeID("PRRC", comment.ID), "body": comment.Body, "in_reply_to_id": root,
		"user": map[string]any{"login": comment.Author}, "created_at": comment.CreatedAt,
		"html_url": fmt.Sprintf("%s#discussion_r%d", githubPullURL(r, p), comment.ID),
	})
}

// ghIssueComment posts a conversation comment on the pull.
func ghIssueComment(e *Engine, c *call, m []string) response {
	var in struct {
		Body string `json:"body"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	r, p, ok := e.ghRESTPull(c, m[1], m[2])
	if !ok {
		return ghHTTPNotFound()
	}
	if strings.TrimSpace(in.Body) == "" {
		return ghUnprocessable("body is required")
	}
	comment := Comment{ID: e.ids.next(), Author: e.viewer, Body: in.Body, CreatedAt: githubNow()}
	p.Comments = append(p.Comments, comment)
	return ghCreated(map[string]any{
		"id": comment.ID, "node_id": githubCommentNodeID("IC", comment.ID), "body": comment.Body,
		"user": map[string]any{"login": comment.Author}, "created_at": comment.CreatedAt,
		"html_url": fmt.Sprintf("%s#issuecomment-%d", githubPullURL(r, p), comment.ID),
	})
}
