package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitPR describes an open pull or merge request. Fields are shared across
// forges; per-forge wrappers map their native shapes onto this struct.
type GitPR struct {
	// ID is the PR's GitHub GraphQL node id, which ReadPRStates addresses
	// it by. Empty on GitLab, whose merge requests are read by iid.
	ID     string `json:"id,omitempty"`
	URL    string `json:"url"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	// BaseBranch is the branch the PR merges into.
	BaseBranch string `json:"baseRefName,omitempty"`
}

// githubForge implements Forge over the GitHub API through the Core's
// forge API transport: GraphQL for reads and thread resolution, REST for
// review writes, CI steps, logs and attachments. Only CreatePR runs gh.
type githubForge struct {
	core *Core
	// ciPermissions caches each login's permission on a repository and
	// ciArtifacts which completed runs keep artifacts (ci_actions_github.go).
	ciPermissions ciCapabilityCache[CIAction]
	ciArtifacts   ciCapabilityCache[bool]
	// readMemory keeps the PR read's shape (github_read_shape.go).
	readMemory githubReadMemory
	// facts holds the facts read of ref: and user: keys
	// (forge_facts_github.go).
	facts *factCache[ForgeFact]
}

// resetCaches forgets what the adapter remembers of each repository
// (Core.ResetForgeCaches).
func (f *githubForge) resetCaches() {
	f.ciPermissions.clear()
	f.ciArtifacts.clear()
	f.readMemory.clear()
	f.facts.clear()
}

func (f *githubForge) ID() string         { return "github" }
func (f *githubForge) BinaryName() string { return "gh" }

// CreatePR opens a pull request via GitHub CLI and returns the created URL.
// It stays a process: gh pushes the branch and may prompt.
// When draft is true the PR is opened as a draft (gh pr create --draft).
func (f *githubForge) CreatePR(ctx context.Context, cwd, title, body, base string, draft bool) (string, error) {
	if strings.TrimSpace(title) == "" {
		return "", errors.New("pull request title is required")
	}

	args := []string{"pr", "create", "--title", title, "--body", body}
	if base = strings.TrimSpace(base); base != "" {
		args = append(args, "--base", base)
	}
	if draft {
		args = append(args, "--draft")
	}
	// Interactive: `gh pr create` pushes the branch itself when it has no
	// upstream yet, and that nested `git push` inherits our environment.
	result, err := f.core.runBinaryInteractive(ctx, "gh", cwd, args...)
	if err != nil {
		if _, ok := errors.AsType[*exec.Error](err); ok || errors.Is(err, exec.ErrNotFound) {
			return "", forgeapi.MissingCLIError(forgeapi.ForgeGitHub, err)
		}
		return "", err
	}
	if result.exitCode != 0 {
		return "", commandFailure("gh pr create", result)
	}

	url := strings.TrimSpace(result.stdout)
	if url == "" {
		return "", errors.New("gh pr create returned empty URL")
	}
	return url, nil
}

// githubOriginRepo is the GitHub repository cwd's origin names, and the
// API client of its host.
func (f *githubForge) githubOriginRepo(cwd string) (client *forgeapi.Client, owner, repo string, err error) {
	forge, host, project, err := f.core.originCoordinates(cwd)
	if err != nil {
		return nil, "", "", err
	}
	if forge != "github" {
		return nil, "", "", &OriginUnknownError{Cwd: cwd, Reason: "the origin is not a GitHub repository"}
	}
	if owner, repo, err = splitGitHubProject(project); err != nil {
		return nil, "", "", err
	}
	if client, err = f.core.githubAPI(host); err != nil {
		return nil, "", "", err
	}
	return client, owner, repo, nil
}

// githubRepoParentRaw is a repository's node id and the repository it
// was forked from (null for one that is no fork). A checkout of a fork
// opens its pull requests in that parent, from a head in the fork.
type githubRepoParentRaw struct {
	ID     string `json:"id"`
	Parent *struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"parent"`
}

// githubHeadRepoRaw is a pull request's head repository, null when it was
// deleted.
type githubHeadRepoRaw struct {
	ID string `json:"id"`
}

// githubPRsByHeadQuery finds pull requests by head branch name in the
// states named. The filter matches a fork's branch of the same name too,
// so each node says whose repository its head is in.
var githubPRsByHeadQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PRsByHead($owner: String!, $name: String!, $head: String!, $states: [PullRequestState!]!, $after: String) {
  repository(owner: $owner, name: $name) {
    id parent { owner { login } name }
    pullRequests(headRefName: $head, states: $states, first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { id url number title state isDraft baseRefName headRefName isCrossRepository headRepository { id } `+githubActor+` }
    }
  }
}`)

// githubHeadPullRaw is one node of githubPRsByHeadQuery.
type githubHeadPullRaw struct {
	GitPR
	IsDraft           bool               `json:"isDraft"`
	HeadRefName       string             `json:"headRefName"`
	Author            githubActorRaw     `json:"author"`
	IsCrossRepository bool               `json:"isCrossRepository"`
	HeadRepository    *githubHeadRepoRaw `json:"headRepository"`
}

// ListOpenPRs returns the open pull requests whose head is the given
// branch of cwd's origin repository itself (githubPRsByHead).
func (f *githubForge) ListOpenPRs(ctx context.Context, cwd, head string) ([]GitPR, error) {
	nodes, err := f.githubPRsByHead(ctx, cwd, head, []string{"OPEN"})
	if err != nil {
		return nil, err
	}
	pulls := make([]GitPR, 0, len(nodes))
	for _, node := range nodes {
		pulls = append(pulls, node.GitPR)
	}
	return pulls, nil
}

// ListPRsByHead returns the pull requests in any state whose head is the
// given branch of cwd's origin repository itself (githubPRsByHead).
func (f *githubForge) ListPRsByHead(ctx context.Context, cwd, head string) ([]PRFound, error) {
	nodes, err := f.githubPRsByHead(ctx, cwd, head, []string{"OPEN", "CLOSED", "MERGED"})
	if err != nil {
		return nil, err
	}
	found := make([]PRFound, 0, len(nodes))
	for _, node := range nodes {
		row, err := prFound(node.URL, node.Title, node.State, node.IsDraft, PRListAuthor{Login: node.Author.Login, Name: node.Author.Name}, node.HeadRefName)
		if err != nil {
			return nil, err
		}
		found = append(found, row)
	}
	return found, nil
}

// githubPRsByHead reads the pull requests in states whose head is the
// given branch of cwd's origin repository itself: those opened in the
// origin from its own branch, and, when the origin is a fork, those
// opened in its parent from the fork's branch. A pull request from
// another fork's branch of the same name is not one: it is someone
// else's work.
func (f *githubForge) githubPRsByHead(ctx context.Context, cwd, head string, states []string) ([]githubHeadPullRaw, error) {
	if strings.TrimSpace(head) == "" {
		return nil, errors.New("pull request head branch is required")
	}
	client, owner, repo, err := f.githubOriginRepo(cwd)
	if err != nil {
		return nil, err
	}
	origin, pulls, err := githubPRsByHeadIn(ctx, client, owner, repo, head, states, func(_ string, crossRepository bool) bool { return !crossRepository })
	if err != nil || origin.Parent == nil {
		return pulls, err
	}
	_, upstream, err := githubPRsByHeadIn(ctx, client, origin.Parent.Owner.Login, origin.Parent.Name, head, states, func(headRepo string, _ bool) bool { return headRepo == origin.ID })
	if err != nil {
		return nil, err
	}
	return append(pulls, upstream...), nil
}

// githubPRsByHeadIn reads owner/name's pull requests in states from a
// head branch named head, keeping those keep accepts by their head
// repository's node id and whether it is another repository, beside the
// repository's id and parent. States are normalized (NormalizePRState).
func githubPRsByHeadIn(ctx context.Context, client *forgeapi.Client, owner, name, head string, states []string, keep func(headRepo string, crossRepository bool) bool) (githubRepoParentRaw, []githubHeadPullRaw, error) {
	var repository githubRepoParentRaw
	pulls := []githubHeadPullRaw{}
	var after any
	for pages := 0; pages < githubReadMaxPages; pages++ {
		var raw struct {
			Repository *struct {
				githubRepoParentRaw
				PullRequests struct {
					PageInfo githubPageInfo      `json:"pageInfo"`
					Nodes    []githubHeadPullRaw `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": name, "head": head, "states": states, "after": after}
		if _, err := client.GraphQL(ctx, githubPRsByHeadQuery, vars, &raw); err != nil {
			return githubRepoParentRaw{}, nil, err
		}
		if raw.Repository == nil {
			return githubRepoParentRaw{}, nil, fmt.Errorf("GitHub answered no repository %s/%s", owner, name)
		}
		repository = raw.Repository.githubRepoParentRaw
		for _, node := range raw.Repository.PullRequests.Nodes {
			headRepo := ""
			if node.HeadRepository != nil {
				headRepo = node.HeadRepository.ID
			}
			if !keep(headRepo, node.IsCrossRepository) {
				continue
			}
			// GitHub spells states uppercase ("OPEN"); callers read the
			// canonical lowercase vocabulary.
			node.State = NormalizePRState(node.State)
			pulls = append(pulls, node)
		}
		page := raw.Repository.PullRequests.PageInfo
		if !page.HasNextPage || page.EndCursor == "" {
			break
		}
		after = page.EndCursor
	}
	return repository, pulls, nil
}

// githubMergedPRsQuery lists merged pull requests, most recently updated
// first, each with its head repository.
var githubMergedPRsQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query MergedPRs($owner: String!, $name: String!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    id parent { owner { login } name }
    pullRequests(states: [MERGED], first: $first, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { headRefName headRefOid url headRepository { id } }
    }
  }
}`)

// ListMergedPRHeads fetches up to limit recently merged PRs' head
// coordinates, 100 per request: the origin's, and when the origin is a
// fork, those among its parent's latest limit whose head was in the
// fork. Recency-bounded by limit: old merges fall off the window, which
// prune surfaces as "no merged PR found" rather than an error.
func (f *githubForge) ListMergedPRHeads(ctx context.Context, cwd string, limit int) ([]MergedPRHead, error) {
	if limit <= 0 {
		return nil, errors.New("merged PR list limit must be positive")
	}
	client, owner, repo, err := f.githubOriginRepo(cwd)
	if err != nil {
		return nil, err
	}
	origin, heads, err := githubMergedPRHeads(ctx, client, owner, repo, limit, func(string) bool { return true })
	if err != nil || origin.Parent == nil {
		return heads, err
	}
	_, upstream, err := githubMergedPRHeads(ctx, client, origin.Parent.Owner.Login, origin.Parent.Name, limit, func(headRepo string) bool { return headRepo == origin.ID })
	if err != nil {
		return heads, err
	}
	return append(heads, upstream...), nil
}

// githubMergedPRHeads reads owner/name's latest limit merged pull
// requests, newest first, keeping those keep accepts by their head
// repository's node id, beside the repository's id and parent.
func githubMergedPRHeads(ctx context.Context, client *forgeapi.Client, owner, name string, limit int, keep func(headRepo string) bool) (githubRepoParentRaw, []MergedPRHead, error) {
	var repository githubRepoParentRaw
	heads := make([]MergedPRHead, 0, min(limit, githubReadPageSize))
	var after any
	for read := 0; read < limit; {
		var raw struct {
			Repository *struct {
				githubRepoParentRaw
				PullRequests struct {
					PageInfo githubPageInfo `json:"pageInfo"`
					Nodes    []struct {
						HeadRefName    string             `json:"headRefName"`
						HeadRefOid     string             `json:"headRefOid"`
						URL            string             `json:"url"`
						HeadRepository *githubHeadRepoRaw `json:"headRepository"`
					} `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": name, "first": min(limit-read, githubReadPageSize), "after": after}
		if _, err := client.GraphQL(ctx, githubMergedPRsQuery, vars, &raw); err != nil {
			return githubRepoParentRaw{}, nil, err
		}
		if raw.Repository == nil {
			return githubRepoParentRaw{}, nil, fmt.Errorf("GitHub answered no repository %s/%s", owner, name)
		}
		repository = raw.Repository.githubRepoParentRaw
		nodes := raw.Repository.PullRequests.Nodes
		read += len(nodes)
		for _, row := range nodes {
			headRepo := ""
			if row.HeadRepository != nil {
				headRepo = row.HeadRepository.ID
			}
			if keep(headRepo) {
				heads = append(heads, MergedPRHead{HeadRefName: row.HeadRefName, HeadOid: row.HeadRefOid, URL: row.URL})
			}
		}
		page := raw.Repository.PullRequests.PageInfo
		if len(nodes) == 0 || !page.HasNextPage || page.EndCursor == "" {
			break
		}
		after = page.EndCursor
	}
	return repository, heads, nil
}

// SubmitReview posts the review with its line and range comments, then
// each file comment and each reply on its own after it: GitHub's review
// takes neither, so they are follow-ups under the partial-submit contract.
// The review and every file comment are pinned to review.HeadSHA, so a
// push since the reviewer loaded the diff moves none of them and approves
// nothing the reviewer did not see. A request with nothing for the review
// itself (a comment verdict with no body and no line comment) posts no
// review, which GitHub would refuse, and its first follow-up's error
// decides whether anything posted. Past the caller's deadline it starts
// no request (writeSteps): the follow-ups it did not start stay unposted.
func (f *githubForge) SubmitReview(ctx context.Context, ref PRReference, review SubmitReviewRequest) (SubmitReviewResult, error) {
	steps, done := writeSteps(ctx)
	defer done()
	number := ref.Number
	if _, _, err := splitGitHubProject(ref.Project()); err != nil {
		return SubmitReviewResult{}, err
	}
	if number <= 0 {
		return SubmitReviewResult{}, fmt.Errorf("PR number must be positive, got %d", number)
	}
	fileComments := make([]ReviewLineComment, 0)
	replies := make([]ReviewLineComment, 0)
	lineComments := make([]ReviewLineComment, 0, len(review.Comments))
	for _, comment := range review.Comments {
		switch {
		case comment.InReplyTo != nil:
			if comment.InReplyTo.RootDatabaseID <= 0 || strings.TrimSpace(comment.Body) == "" {
				return SubmitReviewResult{}, fmt.Errorf("reply to thread %s needs its root comment and a body", comment.InReplyTo.ThreadID)
			}
			replies = append(replies, comment)
		case strings.EqualFold(comment.Side, "file"):
			fileComments = append(fileComments, comment)
		default:
			lineComments = append(lineComments, comment)
		}
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	pulls := githubPullPath(ref)
	body, err := githubReviewRequestBody(review, lineComments)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	fileBodies := make([]githubFileCommentIn, 0, len(fileComments))
	for _, comment := range fileComments {
		in, err := githubFileCommentRequestBody(comment, review.HeadSHA)
		if err != nil {
			return SubmitReviewResult{}, err
		}
		fileBodies = append(fileBodies, in)
	}
	out := SubmitReviewResult{}
	followUps := len(fileComments) + len(replies)
	emptyReview := body.Event == "COMMENT" && strings.TrimSpace(body.Body) == "" && len(body.Comments) == 0
	if !emptyReview || followUps == 0 {
		// The only write before the review exists: its error, or after a
		// lost answer the viewer's reviews, decide whether it posted.
		before, err := readGitHubViewerReviews(steps, client, ref)
		if err != nil {
			return SubmitReviewResult{}, err
		}
		if err := stopped(steps); err != nil {
			return SubmitReviewResult{}, err
		}
		if err := postGitHubReview(ctx, client, ref, body); err != nil {
			if forgeapi.WriteNotApplied(err) {
				return SubmitReviewResult{}, err
			}
			if err := settleGitHubReview(ctx, client, ref, body, before, err); err != nil {
				return SubmitReviewResult{}, err
			}
		}
		out.PostedReview = true
	}
	// failed is a follow-up's failure: nothing posted yet makes it the
	// submit's own write error, else a partial submit.
	failed := func(err error, path, threadID string) error {
		if !out.PostedReview && out.PostedFileComments == 0 && out.PostedReplies == 0 {
			return submitWriteError(err)
		}
		// The follow-ups stop at the first failure: every reply from the
		// first not posted on is not on the PR.
		unposted := make([]int, 0, len(replies)-out.PostedReplies)
		for ordinal := out.PostedReplies; ordinal < len(replies); ordinal++ {
			unposted = append(unposted, ordinal)
		}
		return &PartialSubmitError{PostedReview: out.PostedReview, PostedFileComments: out.PostedFileComments, PostedReplies: out.PostedReplies, FailedPath: path, FailedThreadID: threadID, UnpostedReplies: unposted, Unconfirmed: !forgeapi.WriteNotApplied(err), Err: err}
	}
	for i, in := range fileBodies {
		if err := stopped(steps); err != nil {
			return out, failed(err, fileComments[i].Path, "")
		}
		if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: pulls + "/comments", Body: in}, nil); err != nil {
			return out, failed(err, fileComments[i].Path, "")
		}
		out.PostedFileComments++
	}
	for _, reply := range replies {
		if err := stopped(steps); err != nil {
			return out, failed(err, "", reply.InReplyTo.ThreadID)
		}
		if err := postGitHubReply(ctx, client, ref, *reply.InReplyTo, reply.Body); err != nil {
			return out, failed(err, "", reply.InReplyTo.ThreadID)
		}
		out.PostedReplies++
	}
	return out, nil
}

// githubPullPath is the REST path of ref's pull request.
func githubPullPath(ref PRReference) string {
	return fmt.Sprintf("%s/pulls/%d", githubRepoPath(ref.Repository()), ref.Number)
}

// postGitHubReview submits one review: SubmitReview's and Approve's
// request.
func postGitHubReview(ctx context.Context, client *forgeapi.Client, ref PRReference, review githubReviewIn) error {
	_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: githubPullPath(ref) + "/reviews", Body: review}, nil)
	return err
}

// githubReviewIn is the REST review submission body. CommitID pins the
// review to a head; GitHub reviews the current head when it is empty.
type githubReviewIn struct {
	Event    string                  `json:"event"`
	Body     string                  `json:"body,omitempty"`
	CommitID string                  `json:"commit_id,omitempty"`
	Comments []githubReviewCommentIn `json:"comments,omitempty"`
}

func githubReviewRequestBody(review SubmitReviewRequest, comments []ReviewLineComment) (githubReviewIn, error) {
	payload := githubReviewIn{
		Event:    githubReviewEvent(review.Verdict),
		Body:     review.Body,
		CommitID: review.HeadSHA,
	}
	for _, comment := range comments {
		in, err := githubReviewCommentBody(comment)
		if err != nil {
			return githubReviewIn{}, err
		}
		payload.Comments = append(payload.Comments, in)
	}
	return payload, nil
}

type githubReviewCommentIn struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
	StartLine *int   `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
}

func githubReviewCommentBody(comment ReviewLineComment) (githubReviewCommentIn, error) {
	if strings.TrimSpace(comment.Path) == "" {
		return githubReviewCommentIn{}, errors.New("review comment path is required")
	}
	if strings.TrimSpace(comment.Body) == "" {
		return githubReviewCommentIn{}, errors.New("review comment body is required")
	}
	if comment.Line == nil {
		return githubReviewCommentIn{}, fmt.Errorf("review comment for %s is missing a line", comment.Path)
	}
	side := githubLineSide(comment.Side)
	if side == "" {
		return githubReviewCommentIn{}, fmt.Errorf("review comment for %s has invalid side %q", comment.Path, comment.Side)
	}
	out := githubReviewCommentIn{
		Path: comment.Path,
		Body: comment.Body,
		Line: *comment.Line,
		Side: side,
	}
	if start := comment.Start; start != nil {
		startSide := githubLineSide(start.Side)
		if start.Line <= 0 || startSide == "" {
			return githubReviewCommentIn{}, fmt.Errorf("review comment for %s has an invalid range start", comment.Path)
		}
		line := start.Line
		out.StartLine, out.StartSide = &line, startSide
	}
	return out, nil
}

// githubFileCommentIn is the REST body of a file-level review comment.
type githubFileCommentIn struct {
	Body        string `json:"body"`
	CommitID    string `json:"commit_id"`
	Path        string `json:"path"`
	SubjectType string `json:"subject_type"`
}

func githubFileCommentRequestBody(comment ReviewLineComment, headSHA string) (githubFileCommentIn, error) {
	if strings.TrimSpace(comment.Path) == "" {
		return githubFileCommentIn{}, errors.New("file-level comment path is required")
	}
	if strings.TrimSpace(comment.Body) == "" {
		return githubFileCommentIn{}, errors.New("file-level comment body is required")
	}
	if strings.TrimSpace(headSHA) == "" {
		return githubFileCommentIn{}, errors.New("file-level comment requires PR head SHA")
	}
	return githubFileCommentIn{
		Body:        comment.Body,
		CommitID:    headSHA,
		Path:        comment.Path,
		SubjectType: "file",
	}, nil
}

func githubReviewEvent(verdict string) string {
	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case ReviewVerdictApprove:
		return "APPROVE"
	case ReviewVerdictRequestChanges:
		return "REQUEST_CHANGES"
	default:
		return "COMMENT"
	}
}

func githubLineSide(side string) string {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "right", "new":
		return "RIGHT"
	case "left", "old":
		return "LEFT"
	default:
		return ""
	}
}

func (f *githubForge) ReplyToThread(ctx context.Context, ref PRReference, threadID string, databaseID int64, body string) error {
	if _, _, err := splitGitHubProject(ref.Project()); err != nil {
		return err
	}
	if ref.Number <= 0 {
		return fmt.Errorf("PR number must be positive, got %d", ref.Number)
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	return commentWriteError(postGitHubReply(ctx, client, ref, ReviewThreadReply{ThreadID: threadID, RootDatabaseID: databaseID}, body))
}

// githubSetThreadResolvedMutation runs resolveReviewThread or
// unresolveReviewThread on one thread, chosen by $resolved, with the
// thread's state after it.
var githubSetThreadResolvedMutation = forgeapi.NewDocument(forgeapi.ForgeGitHub, `mutation SetThreadResolved($threadID: ID!, $resolved: Boolean!) {
  resolve: resolveReviewThread(input: {threadId: $threadID}) @include(if: $resolved) { thread { isResolved } }
  unresolve: unresolveReviewThread(input: {threadId: $threadID}) @skip(if: $resolved) { thread { isResolved } }
}`)

// githubThreadOwnerQuery reads which pull request a review thread's node
// id belongs to: the id is global on its host, so the mutation alone names
// nothing about the PR the caller named.
var githubThreadOwnerQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query ThreadOwner($id: ID!) {
  node(id: $id) {
    __typename
    ... on PullRequestReviewThread { pullRequest { number repository { nameWithOwner } } }
  }
}`)

// SetThreadResolved flips one review thread's resolved state, after
// reading that the thread is one of ref's pull request and refusing,
// before any write, one that is not (*ThreadNotOnPRError).
func (f *githubForge) SetThreadResolved(ctx context.Context, ref PRReference, threadID string, resolved bool) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("GitHub thread resolution requires a review thread id")
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	var raw struct {
		Node *struct {
			Typename    string `json:"__typename"`
			PullRequest *struct {
				Number     int `json:"number"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"pullRequest"`
		} `json:"node"`
	}
	if _, err := client.GraphQL(ctx, githubThreadOwnerQuery, map[string]any{"id": threadID}, &raw); err != nil && !graphQLNotFoundError(err) {
		return err
	}
	node := raw.Node
	if node == nil || node.Typename != "PullRequestReviewThread" || node.PullRequest == nil || node.PullRequest.Number != ref.Number ||
		!strings.EqualFold(node.PullRequest.Repository.NameWithOwner, ref.Namespace+"/"+ref.Repo) {
		return &ThreadNotOnPRError{ThreadID: threadID}
	}
	return setGitHubThreadResolved(ctx, client, threadID, resolved)
}

// graphQLNotFoundError reports a GraphQL answer whose only problem is an
// id that resolves to nothing for this login.
func graphQLNotFoundError(err error) bool {
	graphQL, ok := errors.AsType[*forgeapi.GraphQLError](err)
	return ok && graphQLNotFound(graphQL)
}

// setGitHubThreadResolved runs the mutation on a thread the caller knows
// is the PR's. The mutation's answer is read back rather than trusted: a
// 200 that resolved nothing would otherwise reach the user as a success
// and leave the pane showing a state the forge does not have. A failure
// the forge may have applied is an *UnconfirmedWriteError.
func setGitHubThreadResolved(ctx context.Context, client *forgeapi.Client, threadID string, resolved bool) error {
	type payload struct {
		Thread *struct {
			IsResolved bool `json:"isResolved"`
		} `json:"thread"`
	}
	var raw struct {
		Resolve   *payload `json:"resolve"`
		Unresolve *payload `json:"unresolve"`
	}
	if _, err := client.GraphQL(ctx, githubSetThreadResolvedMutation, map[string]any{"threadID": threadID, "resolved": resolved}, &raw); err != nil {
		return writeOutcome(err)
	}
	answer := raw.Unresolve
	if resolved {
		answer = raw.Resolve
	}
	if answer == nil || answer.Thread == nil {
		return fmt.Errorf("GitHub reported no state for review thread %s after requesting isResolved=%t", threadID, resolved)
	}
	if answer.Thread.IsResolved != resolved {
		return fmt.Errorf("GitHub reported review thread %s as isResolved=%t after requesting %t", threadID, answer.Thread.IsResolved, resolved)
	}
	return nil
}

func splitGitHubProject(project string) (string, string, error) {
	namespace, repo, err := SplitProjectForForge("github", project)
	if err != nil {
		return "", "", err
	}
	return namespace, repo, nil
}

// CreatePR is a thin wrapper that dispatches to the forge detected for
// cwd. Returns ErrUnsupportedForge (via nullForge) when the origin
// remote is missing or its host is not a recognised forge. The CLI pushes
// before it opens the PR, so it runs to its own timeout once started.
func (c *Core) CreatePR(ctx context.Context, cwd, title, body, base string, draft bool) (string, error) {
	return c.forgeFor(cwd).CreatePR(forgeWrite(ctx), cwd, title, body, base, draft)
}

// ListOpenPRs is a thin wrapper that dispatches to the forge detected
// for cwd. See CreatePR for the dispatch model.
func (c *Core) ListOpenPRs(ctx context.Context, cwd, head string) ([]GitPR, error) {
	return c.forgeFor(cwd).ListOpenPRs(ctx, cwd, head)
}

// ListMergedPRHeads is a thin wrapper that dispatches to the forge
// detected for cwd. See CreatePR for the dispatch model.
func (c *Core) ListMergedPRHeads(ctx context.Context, cwd string, limit int) ([]MergedPRHead, error) {
	return c.forgeFor(cwd).ListMergedPRHeads(ctx, cwd, limit)
}
