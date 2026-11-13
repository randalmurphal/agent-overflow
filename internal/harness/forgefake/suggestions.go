package forgefake

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/mdsuggest"
)

// Suggested changes. A pull's Files are its head branch at HeadSHA. On
// GitHub the app reads a file through the contents API and commits the
// rewritten file with createCommitOnBranch (SuggestionCommit); on GitLab
// the discussions carry each note's suggestions, derived as GitLab
// derives them (the note's suggestion blocks over the thread's lines of
// Files), and PUT suggestions/:id/apply rewrites the file. Either commit
// moves the head to a new sha and makes the threads on the lines it
// changed outdated, so the next read shows a new head.

// gitlabSuggestionID is a GitLab suggestion's id: its note's id and its
// index in the note, so the fake can find it again without storing it.
func gitlabSuggestionID(noteID int64, index int) int64 { return noteID*1000 + int64(index) }

// fileLines splits content as the forges count lines: a trailing newline
// ends the last line, CRLF is one ending.
func fileLines(content string) []string {
	content = strings.TrimSuffix(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// gitlabSuggestionRange is a block's replaced new-side lines on thread:
// the anchor widened by a -N+M header.
func gitlabSuggestionRange(thread *Thread, block mdsuggest.Block) (from, to int) {
	to = *thread.Line
	from = to
	if thread.StartLine != nil {
		from = *thread.StartLine
	}
	return from - block.Above, to + block.Below
}

// gitlabNoteSuggestions answers a thread note's suggestions as GitLab
// does: one per suggestion block, its from_content the lines it replaces
// on the head branch, appliable while the merge request is open, the
// thread current and on the new side, the lines on the branch, and the
// suggestion not applied.
func gitlabNoteSuggestions(p *Pull, thread *Thread, comment Comment) []map[string]any {
	if thread.Side != "right" || thread.Line == nil {
		return []map[string]any{}
	}
	content, onBranch := p.Files[thread.Path]
	lines := fileLines(content)
	out := []map[string]any{}
	for i, block := range mdsuggest.Parse(comment.Body) {
		from, to := gitlabSuggestionRange(thread, block)
		fromContent := ""
		inFile := onBranch && from >= 1 && to <= len(lines) && from <= to
		if inFile {
			fromContent = strings.Join(lines[from-1:to], "\n") + "\n"
		}
		toContent := strings.Join(block.Lines, "\n")
		if len(block.Lines) > 0 {
			toContent += "\n"
		}
		applied := slices.Contains(comment.AppliedSuggestions, i)
		out = append(out, map[string]any{
			"id": gitlabSuggestionID(comment.ID, i), "from_line": from, "to_line": to,
			"appliable": p.State == "open" && !thread.Outdated && inFile && !applied, "applied": applied,
			"from_content": fromContent, "to_content": toContent,
		})
	}
	return out
}

// commitFile writes content to path on p's head branch: a new head sha,
// and every line thread on path whose lines the change touched becomes
// outdated.
func (e *Engine) commitFile(p *Pull, path, content string) string {
	before := fileLines(p.Files[path])
	after := fileLines(content)
	first := 0
	for first < len(before) && first < len(after) && before[first] == after[first] {
		first++
	}
	lastBefore, lastAfter := len(before), len(after)
	for lastBefore > first && lastAfter > first && before[lastBefore-1] == after[lastAfter-1] {
		lastBefore--
		lastAfter--
	}
	for i := range p.Threads {
		thread := &p.Threads[i]
		if thread.Path != path || thread.Line == nil || thread.Side != "right" {
			continue
		}
		start := *thread.Line
		if thread.StartLine != nil {
			start = *thread.StartLine
		}
		// Changed lines are first+1 .. lastBefore (1-based); a pure insert
		// touches the line it lands before.
		if start <= max(lastBefore, first+1) && *thread.Line >= first+1 {
			thread.Outdated = true
		}
	}
	if p.Files == nil {
		p.Files = map[string]string{}
	}
	p.Files[path] = content
	p.HeadSHA = e.ids.sha()
	return p.HeadSHA
}

// ghContents answers GET repos/O/R/contents/PATH?ref=SHA with the raw
// media type: the file's bytes on the head of a pull whose head
// repository is O/R (the repository itself, or a pull's headRepo fork)
// at that sha. The fake models no older commit: another ref is a 404.
func ghContents(e *Engine, c *call, m []string) response {
	if accept := c.flag("accept"); accept != "application/vnd.github.raw" {
		return unhandled("gh api contents answers the raw media type only, not %q", accept)
	}
	query, err := url.ParseQuery(m[3])
	if err != nil || len(query) != 1 || query.Get("ref") == "" {
		return unhandled("gh api contents takes ref alone: %q", m[3])
	}
	path, err := url.PathUnescape(m[2])
	if err != nil {
		return unhandled("contents path %q", m[2])
	}
	p := e.githubHeadPull(c, m[1], "", query.Get("ref"))
	if p == nil {
		return ghNotFound()
	}
	content, ok := p.Files[path]
	if !ok {
		return ghNotFound()
	}
	return response{stdout: []byte(content), header: http.Header{"Content-Type": {"application/vnd.github.raw"}}}
}

func ghNotFound() response {
	return response{stdout: []byte(`{"message":"Not Found","status":"404"}`), status: http.StatusNotFound, header: http.Header{"Content-Type": {"application/json"}}}
}

// githubHeadPull is the open or closed pull on the request's host whose
// head is repo (the repository itself or its headRepo fork), on branch
// when given and at sha when given.
func (e *Engine) githubHeadPull(c *call, repo, branch, sha string) *Pull {
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "github" || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		for i := range r.Pulls {
			p := &r.Pulls[i]
			head := cmpOr(p.HeadRepo, r.Project)
			if strings.EqualFold(head, repo) && (branch == "" || p.HeadRef == branch) && (sha == "" || p.HeadSHA == sha) {
				return p
			}
		}
	}
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ghSuggestionTarget answers SuggestionTarget: the viewer, the thread with
// its pull and the first page of its comments' ids, and the comment.
func ghSuggestionTarget(e *Engine, c *call, v graphQLVars) response {
	threadID, err := v.str("threadID")
	commentID, err1 := v.str("commentID")
	if err := firstErr(err, err1); err != nil {
		return unhandled("SuggestionTarget: %v", err)
	}
	data := map[string]any{"viewer": map[string]any{"login": e.viewer}, "thread": nil, "comment": nil}
	r, p, thread := e.githubThread(c, threadID)
	if thread == nil {
		return graphQLNotFound(data, []string{"thread"}, fmt.Sprintf("Could not resolve to a node with the global id of '%s'", threadID))
	}
	_, cp, ct, at, ok := e.githubCommentByNode(c, commentID)
	if !ok {
		return graphQLNotFound(data, []string{"comment"}, fmt.Sprintf("Could not resolve to a node with the global id of '%s'", commentID))
	}
	nodes := e.githubThreadNodes(r, p)
	var node map[string]any
	for _, candidate := range nodes {
		if candidate["id"] == thread.ID {
			node = candidate
		}
	}
	ids := []map[string]any{}
	for _, comment := range thread.Comments {
		ids = append(ids, map[string]any{"id": githubCommentNodeID("PRRC", comment.ID)})
	}
	var headRepo any = map[string]any{"nameWithOwner": cmpOr(p.HeadRepo, r.Project)}
	data["thread"] = map[string]any{
		"__typename": "PullRequestReviewThread", "id": thread.ID, "isResolved": thread.Resolved, "isOutdated": thread.Outdated,
		"path": thread.Path, "line": node["line"], "startLine": node["startLine"], "diffSide": node["diffSide"],
		"startDiffSide": node["startDiffSide"], "subjectType": node["subjectType"],
		"pullRequest": map[string]any{
			"number": p.Number, "state": strings.ToUpper(p.State), "headRefName": p.HeadRef, "headRefOid": p.HeadSHA,
			"repository": map[string]any{"nameWithOwner": r.Project}, "headRepository": headRepo,
		},
		"comments": graphQLConnection(ids, 0, githubGraphQLPageSize),
	}
	if ct < 0 {
		data["comment"] = map[string]any{"__typename": "IssueComment"}
	} else {
		comment := cp.Threads[ct].Comments[at]
		author := githubActor(comment.Author, comment.AuthorName)
		if comment.AuthorName != "" {
			author["databaseId"] = e.githubUserID(comment.Author)
		}
		data["comment"] = map[string]any{"__typename": "PullRequestReviewComment", "id": commentID, "body": comment.Body, "author": author}
	}
	return graphQLData(data)
}

// githubUserID is a user's stable fake databaseId.
func (e *Engine) githubUserID(login string) int64 {
	sum := int64(0)
	for _, b := range []byte(strings.ToLower(login)) {
		sum = sum*31 + int64(b)
	}
	return 1000 + sum%1_000_000
}

// ghSuggestionCommit answers SuggestionCommit (createCommitOnBranch with
// one file addition): the head branch of a pull on the host whose head
// repository is repository, refused as GitHub refuses a viewer who may
// not push, a head other than expectedHeadOid, and contents that are not
// base64. The file lands on the branch and the head moves.
func ghSuggestionCommit(e *Engine, c *call, v graphQLVars) response {
	const field = "createCommitOnBranch"
	repo, err := v.str("repository")
	branch, err1 := v.str("branch")
	head, err2 := v.str("expectedHeadOid")
	headline, err3 := v.str("headline")
	path, err4 := v.str("path")
	contents, err5 := v.str("contents")
	if err := firstErr(err, err1, err2, err3, err4, err5); err != nil {
		return unhandled("SuggestionCommit: %v", err)
	}
	if body, ok := v["body"]; ok && body != nil {
		if _, err := v.str("body"); err != nil {
			return unhandled("SuggestionCommit: %v", err)
		}
	}
	p := e.githubHeadPull(c, repo, branch, "")
	if p == nil {
		return graphQLMutationError(field, "NOT_FOUND", fmt.Sprintf("Could not resolve to a Ref with the name '%s' in %s.", branch, repo))
	}
	r := e.repoOfPull(p)
	decoded, err := base64.StdEncoding.DecodeString(contents)
	switch {
	case !e.mayPush(r, p):
		return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have permission to push to %s:%s.", e.viewer, repo, branch))
	case head != p.HeadSHA:
		return graphQLMutationError(field, "UNPROCESSABLE", fmt.Sprintf("Expected branch to point to %q but it did not. Pull and try again.", head))
	case strings.TrimSpace(headline) == "":
		return graphQLMutationError(field, "UNPROCESSABLE", "A commit message headline is required.")
	case err != nil || !utf8.Valid(decoded):
		return graphQLMutationError(field, "UNPROCESSABLE", "fileChanges.additions[0].contents is not valid base64.")
	}
	oid := e.commitFile(p, path, string(decoded))
	return graphQLData(map[string]any{field: map[string]any{"commit": map[string]any{"oid": oid}}})
}

// repoOfPull is the repository holding p.
func (e *Engine) repoOfPull(p *Pull) *Repo {
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		for i := range r.Pulls {
			if &r.Pulls[i] == p {
				return r
			}
		}
	}
	return nil
}

// glabApplySuggestion answers PUT suggestions/:id/apply: the suggestion
// of a note in a merge request of a GitLab project on the host. Below
// developer is GitLab's 403; one not appliable its 400; else the lines
// are rewritten on the head branch, the head moves and the suggestion
// reads applied.
func glabApplySuggestion(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("apply suggestion: %v", err)
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	noteID, index := id/1000, int(id%1000)
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "gitlab" || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		for i := range r.Pulls {
			p := &r.Pulls[i]
			for t := range p.Threads {
				thread := &p.Threads[t]
				at := slices.IndexFunc(thread.Comments, func(c Comment) bool { return c.ID == noteID })
				if at < 0 {
					continue
				}
				comment := &thread.Comments[at]
				suggestions := gitlabNoteSuggestions(p, thread, *comment)
				if index >= len(suggestions) {
					return glabNotFound("Suggestion")
				}
				s := suggestions[index]
				switch {
				case !e.mayPush(r, p):
					return glabForbidden()
				case s["appliable"] != true:
					return glabStatus(http.StatusBadRequest, "A suggestion is not applicable.")
				}
				lines := fileLines(p.Files[thread.Path])
				block := mdsuggest.Parse(comment.Body)[index]
				from, to := gitlabSuggestionRange(thread, block)
				next := append(append(slices.Clone(lines[:from-1]), block.Lines...), lines[to:]...)
				content := strings.Join(next, "\n")
				if len(next) > 0 && strings.HasSuffix(p.Files[thread.Path], "\n") {
					content += "\n"
				}
				comment.AppliedSuggestions = append(comment.AppliedSuggestions, index)
				e.commitFile(p, thread.Path, content)
				s["applied"], s["appliable"] = true, false
				return jsonResponse(s)
			}
		}
	}
	return glabNotFound("Suggestion")
}
