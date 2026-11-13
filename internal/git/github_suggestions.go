package git

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/mdsuggest"
)

// GitHub's suggestions. GitHub reports none of its own: the read parses
// each review thread comment's body (mdsuggest.Parse) and anchors its
// blocks on the thread's new-side lines; the replaced text is not read
// (it would take each thread's diffHunk into the one tick request), so
// From is left unknown. GitHub has no apply endpoint: the apply reads the
// file at the head the pane loaded, rewrites the anchored lines and
// commits it to the head branch with createCommitOnBranch, as GitHub's
// own "Commit suggestion" does, then resolves the thread as GitHub does.

// githubSuggestionBlocks are the blocks GitHub renders as suggestions:
// the ones without GitLab's -N+M header, which GitHub reads as code.
func githubSuggestionBlocks(body string) []mdsuggest.Block {
	var out []mdsuggest.Block
	for _, block := range mdsuggest.Parse(body) {
		if !block.Widened {
			out = append(out, block)
		}
	}
	return out
}

// githubNewSideRange is the new-side lines a thread is anchored on, ok
// false for a file thread, an old-side line or range, or an outdated
// thread (whose current lines GitHub nulls).
func githubNewSideRange(thread ReviewThread) (start, end int, ok bool) {
	if thread.Side != "right" || thread.Line == nil || (thread.StartSide != "" && thread.StartSide != "right") {
		return 0, 0, false
	}
	start = *thread.Line
	if thread.StartLine != nil {
		start = *thread.StartLine
	}
	return start, *thread.Line, start > 0 && start <= *thread.Line
}

// githubMarkSuggestions fills each review thread comment's suggestions.
// A suggestion is appliable while the PR is open, the viewer may apply
// suggestions (viewerCanApplySuggestion, GitHub's own answer), and the
// thread is current, unresolved (GitHub's apply resolves it) and on new
// lines.
func githubMarkSuggestions(threads []ReviewThread, pull *githubPullRaw) {
	open := strings.EqualFold(pull.State, "OPEN")
	for i := range threads {
		thread := &threads[i]
		start, end, onNew := githubNewSideRange(*thread)
		appliable := open && pull.ViewerCanApplySuggestion && onNew && !thread.IsOutdated && !thread.IsResolved
		for j := range thread.Comments {
			comment := &thread.Comments[j]
			for index, block := range githubSuggestionBlocks(comment.Body) {
				suggestion := CommentSuggestion{Index: index, To: block.Lines, Appliable: appliable,
					BodyStartLine: block.StartLine, BodyEndLine: block.EndLine, BodyPlaced: true}
				if onNew {
					suggestion.StartLine, suggestion.EndLine = start, end
				}
				suggestion.Digest = SuggestionDigest("github", suggestion.StartLine, suggestion.EndLine, block.Lines)
				comment.Suggestions = append(comment.Suggestions, suggestion)
			}
		}
	}
}

// githubReactions maps reaction groups with a count to the shared
// vocabulary, in GitHub's order.
func githubReactions(groups []githubReactionGroupRaw) []CommentReaction {
	var out []CommentReaction
	for _, group := range groups {
		name := githubReactionName(group.Content)
		if name == "" || group.Reactors.TotalCount <= 0 {
			continue
		}
		out = append(out, CommentReaction{Name: name, Count: group.Reactors.TotalCount, ViewerReacted: group.ViewerHasReacted})
	}
	return out
}

// githubSuggestionTargetQuery reads what an apply checks and needs: the
// thread with its anchor, its PR (to check it is the caller's, open and
// at the head the caller saw) and its comments' ids (the comment must be
// one of them), the comment's body and author (the Co-authored-by
// trailer), and the viewer (no trailer for their own suggestion).
const githubSuggestionTargetQuery = `query SuggestionTarget($threadID: ID!, $commentID: ID!) {
  viewer { login }
  thread: node(id: $threadID) {
    __typename
    ... on PullRequestReviewThread {
      id isResolved isOutdated path line startLine diffSide startDiffSide subjectType
      pullRequest { number state headRefName headRefOid repository { nameWithOwner } headRepository { nameWithOwner } }
      comments(first: 100) { ` + githubPageInfoFields + ` nodes { id } }
    }
  }
  comment: node(id: $commentID) {
    __typename
    ... on PullRequestReviewComment { id body author { login ... on User { databaseId name } } }
  }
}`

const githubSuggestionCommitMutation = `mutation SuggestionCommit($repository: String!, $branch: String!, $expectedHeadOid: GitObjectID!, $headline: String!, $body: String, $path: String!, $contents: Base64String!) {
  createCommitOnBranch(input: {branch: {repositoryNameWithOwner: $repository, branchName: $branch}, expectedHeadOid: $expectedHeadOid, message: {headline: $headline, body: $body}, fileChanges: {additions: [{path: $path, contents: $contents}]}}) { commit { oid } }
}`

type githubSuggestionTargetRaw struct {
	Viewer *struct {
		Login string `json:"login"`
	} `json:"viewer"`
	Thread *struct {
		Typename      string `json:"__typename"`
		ID            string `json:"id"`
		IsResolved    bool   `json:"isResolved"`
		IsOutdated    bool   `json:"isOutdated"`
		Path          string `json:"path"`
		Line          *int   `json:"line"`
		StartLine     *int   `json:"startLine"`
		DiffSide      string `json:"diffSide"`
		StartDiffSide string `json:"startDiffSide"`
		SubjectType   string `json:"subjectType"`
		PullRequest   *struct {
			Number     int    `json:"number"`
			State      string `json:"state"`
			HeadRef    string `json:"headRefName"`
			HeadOID    string `json:"headRefOid"`
			Repository struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"repository"`
			HeadRepository *struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"headRepository"`
		} `json:"pullRequest"`
		Comments githubThreadCommentsRaw `json:"comments"`
	} `json:"thread"`
	Comment *struct {
		Typename string `json:"__typename"`
		ID       string `json:"id"`
		Body     string `json:"body"`
		Author   *struct {
			Login      string `json:"login"`
			DatabaseID int64  `json:"databaseId"`
			Name       string `json:"name"`
		} `json:"author"`
	} `json:"comment"`
}

// ApplySuggestion commits one suggestion to the head branch and resolves
// its thread. Everything is checked before the commit: the thread and
// comment belong to ref and to each other, the PR is open at req.HeadSHA,
// the thread is current, unresolved and on new lines, and the comment
// holds the suggestion the caller was shown (its digest). A failed resolve after the commit is a
// PartialWriteError; the commit is never sent twice.
func (f *githubForge) ApplySuggestion(ctx context.Context, ref PRReference, req SuggestionApply) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	var raw githubSuggestionTargetRaw
	if _, err := client.GraphQL(ctx, "SuggestionTarget", githubSuggestionTargetQuery, map[string]any{"threadID": req.Target.ThreadID, "commentID": req.Target.CommentID}, &raw); err != nil {
		var gql *forgeapi.GraphQLError
		if errors.As(err, &gql) && graphQLNotFound(gql) {
			return &CommentNotOnPRError{CommentID: req.Target.CommentID}
		}
		return err
	}
	thread, comment := raw.Thread, raw.Comment
	elsewhere := &CommentNotOnPRError{CommentID: req.Target.CommentID}
	if thread == nil || thread.Typename != "PullRequestReviewThread" || thread.PullRequest == nil ||
		thread.PullRequest.Number != ref.Number || !strings.EqualFold(thread.PullRequest.Repository.NameWithOwner, ref.Namespace+"/"+ref.Repo) ||
		comment == nil || comment.Typename != "PullRequestReviewComment" {
		return elsewhere
	}
	members := githubThreadRaw{ID: thread.ID, Comments: thread.Comments}
	if err := f.readThreadComments(ctx, client, &members, false); err != nil {
		return err
	}
	found := false
	for _, node := range members.Comments.Nodes {
		found = found || node.ID == comment.ID
	}
	if !found {
		if members.Comments.PageInfo.HasNextPage {
			return &SuggestionNotAppliedError{Reason: "the thread has more comments than the app reads, so the comment could not be found in it"}
		}
		return elsewhere
	}
	pull := thread.PullRequest
	view := ReviewThread{Side: strings.ToLower(thread.DiffSide), StartSide: strings.ToLower(thread.StartDiffSide), Line: thread.Line, StartLine: thread.StartLine}
	if strings.EqualFold(thread.SubjectType, "FILE") {
		view.Side = "file"
	}
	start, end, onNew := githubNewSideRange(view)
	blocks := githubSuggestionBlocks(comment.Body)
	switch {
	case !strings.EqualFold(pull.State, "OPEN"):
		return &SuggestionNotAppliedError{Reason: "the pull request is not open"}
	case !strings.EqualFold(pull.HeadOID, req.HeadSHA):
		return &HeadMovedError{Want: req.HeadSHA, Have: pull.HeadOID}
	case thread.IsOutdated:
		return &SuggestionNotAppliedError{Reason: "the lines it changes have changed since it was written"}
	case thread.IsResolved:
		return &SuggestionNotAppliedError{Reason: "its thread is resolved"}
	case !onNew:
		return &SuggestionNotAppliedError{Reason: "the comment is not on lines of the changed file"}
	case req.Index >= len(blocks) || SuggestionDigest("github", start, end, blocks[req.Index].Lines) != req.Digest:
		// The comment was edited, a block came or went before it, or the
		// anchor moved since the pane read it.
		return &SuggestionChangedError{}
	case pull.HeadRepository == nil:
		return &SuggestionNotAppliedError{Reason: "the head branch's repository is gone"}
	}
	headRepo := pull.HeadRepository.NameWithOwner
	content, err := readGitHubFileAt(ctx, client, headRepo, thread.Path, pull.HeadOID)
	if err != nil {
		return err
	}
	rewritten, err := applySuggestionToFile(content, start, end, blocks[req.Index].Lines)
	if err != nil {
		return err
	}
	vars := map[string]any{
		"repository": headRepo, "branch": pull.HeadRef, "expectedHeadOid": pull.HeadOID,
		"headline": SuggestionCommitHeadline, "path": thread.Path, "contents": base64.StdEncoding.EncodeToString(rewritten),
	}
	viewer := ""
	if raw.Viewer != nil {
		viewer = raw.Viewer.Login
	}
	if trailer := githubCoAuthor(comment.Author, viewer); trailer != "" {
		vars["body"] = trailer
	}
	var commit struct {
		CreateCommitOnBranch *struct {
			Commit *struct {
				OID string `json:"oid"`
			} `json:"commit"`
		} `json:"createCommitOnBranch"`
	}
	if _, err := client.GraphQL(ctx, "SuggestionCommit", githubSuggestionCommitMutation, vars, &commit); err != nil {
		return writeOutcome(err)
	}
	if commit.CreateCommitOnBranch == nil || commit.CreateCommitOnBranch.Commit == nil || commit.CreateCommitOnBranch.Commit.OID == "" {
		return &UnconfirmedWriteError{Err: errors.New("GitHub answered the suggestion's commit with no commit")}
	}
	if err := f.SetThreadResolved(ctx, ref, thread.ID, true); err != nil {
		return &PartialWriteError{Done: "committed the suggestion to the head branch", Next: "resolving its thread", Err: err}
	}
	return nil
}

// githubCoAuthor is the Co-authored-by trailer crediting a suggestion's
// author, as GitHub's own commit does: a user's no-reply address,
// ID+login@users.noreply.github.com. A bot, a deleted account, an author
// without an id, or the viewer themself gets none.
func githubCoAuthor(author *struct {
	Login      string `json:"login"`
	DatabaseID int64  `json:"databaseId"`
	Name       string `json:"name"`
}, viewer string) string {
	if author == nil || author.Login == "" || author.DatabaseID <= 0 || strings.EqualFold(author.Login, viewer) {
		return ""
	}
	name := strings.TrimSpace(author.Name)
	if name == "" || strings.ContainsAny(name, "<>\r\n") {
		name = author.Login
	}
	return fmt.Sprintf("Co-authored-by: %s <%d+%s@users.noreply.github.com>", name, author.DatabaseID, author.Login)
}

// githubSuggestionFileQuery reads the tree entry at path on the head
// commit: what the apply may rewrite is a regular file's blob, which the
// entry's mode, type and blob size decide before any content is read.
const githubSuggestionFileQuery = `query SuggestionFile($owner: String!, $name: String!, $oid: GitObjectID!, $path: String!) {
  repository(owner: $owner, name: $name) {
    object(oid: $oid) {
      __typename
      ... on Commit { file(path: $path) { mode type oid object { __typename ... on Blob { byteSize isBinary } } } }
    }
  }
}`

type githubSuggestionFileRaw struct {
	Repository *struct {
		Object *struct {
			Typename string `json:"__typename"`
			File     *struct {
				Mode   int    `json:"mode"`
				Type   string `json:"type"`
				OID    string `json:"oid"`
				Object *struct {
					Typename string `json:"__typename"`
					ByteSize int64  `json:"byteSize"`
					IsBinary *bool  `json:"isBinary"`
				} `json:"object"`
			} `json:"file"`
		} `json:"object"`
	} `json:"repository"`
}

// Git tree entry modes, as octal numbers.
const (
	gitModeFile       = 0o100644
	gitModeExecutable = 0o100755
	gitModeSymlink    = 0o120000
	gitModeSubmodule  = 0o160000
	gitModeTree       = 0o040000
)

// gitTreeMode reads TreeEntry.mode, an Int: the mode's value, or its
// octal digits read as a decimal number (100644), whichever the answer
// is; the two never collide for the modes Git writes.
func gitTreeMode(mode int) int {
	if octal, err := strconv.ParseInt(strconv.Itoa(mode), 8, 32); err == nil {
		switch int(octal) {
		case gitModeFile, gitModeExecutable, gitModeSymlink, gitModeSubmodule, gitModeTree:
			return int(octal)
		}
	}
	return mode
}

// readGitHubFileAt reads the regular file at path in repo at the commit
// sha: its tree entry first (SuggestionFile), refusing what the apply
// cannot rewrite before any content is read (no entry, a symbolic link,
// a submodule, a directory, an executable file, whose mode the commit
// API cannot keep, a blob past suggestionFileMaxBytes or one GitHub
// calls binary), then the blob by its id
// (repos/R/git/blobs/OID, raw, bounded), checked against that id.
func readGitHubFileAt(ctx context.Context, client *forgeapi.Client, repo, path, sha string) ([]byte, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var raw githubSuggestionFileRaw
	if _, err := client.GraphQL(ctx, "SuggestionFile", githubSuggestionFileQuery, map[string]any{"owner": owner, "name": name, "oid": sha, "path": path}, &raw); err != nil {
		return nil, err
	}
	refuse := func(reason string) ([]byte, error) { return nil, &SuggestionNotAppliedError{Reason: reason} }
	if raw.Repository == nil || raw.Repository.Object == nil || raw.Repository.Object.Typename != "Commit" {
		return refuse("the head commit is not in the head branch's repository")
	}
	entry := raw.Repository.Object.File
	if entry == nil {
		return refuse("the file is not on the head commit")
	}
	mode := gitTreeMode(entry.Mode)
	switch {
	case mode == gitModeSymlink:
		return refuse("the file is a symbolic link")
	case mode == gitModeSubmodule || entry.Type == "commit":
		return refuse("the path is a submodule")
	case mode == gitModeTree || entry.Type == "tree":
		return refuse("the path is a directory")
	case mode == gitModeExecutable:
		return refuse("the file is executable and GitHub's commit API cannot keep that; apply it from a checkout")
	case mode != gitModeFile || entry.Type != "blob" || entry.Object == nil || entry.Object.Typename != "Blob":
		return refuse("the path is not a regular file")
	case entry.Object.ByteSize > suggestionFileMaxBytes:
		return refuse(fmt.Sprintf("the file is larger than %d MiB", suggestionFileMaxBytes>>20))
	case entry.Object.IsBinary != nil && *entry.Object.IsBinary:
		return refuse("the file is not text")
	case len(entry.OID) != sha1.Size*2:
		return refuse("the repository's object format is not supported")
	}
	resp, err := client.Do(ctx, forgeapi.Request{
		Path:         "repos/" + repo + "/git/blobs/" + url.PathEscape(entry.OID),
		Header:       http.Header{"Accept": {"application/vnd.github.raw+json"}},
		MaxBodyBytes: suggestionFileMaxBytes,
	})
	if errors.Is(err, forgeapi.ErrBodyTooLarge) {
		return refuse(fmt.Sprintf("the file is larger than %d MiB", suggestionFileMaxBytes>>20))
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(gitBlobID(resp.Body), entry.OID) {
		return refuse("the file read is not the file on the head commit")
	}
	return resp.Body, nil
}

// gitBlobID is the SHA-1 object id Git gives content as a blob.
func gitBlobID(content []byte) string {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}
