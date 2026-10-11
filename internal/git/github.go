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
	URL    string `json:"url"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// githubForge implements Forge over the GitHub API through the Core's
// forge API transport: GraphQL for reads and thread resolution, REST for
// review writes, CI steps, logs and attachments. Only CreatePR runs gh.
type githubForge struct {
	core *Core
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

// githubOpenPRsQuery finds open pull requests by head branch name alone,
// so a fork's branch resolves without OWNER:branch.
const githubOpenPRsQuery = `query OpenPRsByHead($owner: String!, $name: String!, $head: String!) {
  repository(owner: $owner, name: $name) {
    pullRequests(headRefName: $head, states: [OPEN], first: 10) { nodes { url number title state } }
  }
}`

// ListOpenPRs returns the open pull requests whose head is the given
// branch of cwd's origin repository.
func (f *githubForge) ListOpenPRs(ctx context.Context, cwd, head string) ([]GitPR, error) {
	if strings.TrimSpace(head) == "" {
		return nil, errors.New("pull request head branch is required")
	}
	client, owner, repo, err := f.githubOriginRepo(cwd)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Repository *struct {
			PullRequests struct {
				Nodes []GitPR `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	if _, err := client.GraphQL(ctx, "OpenPRsByHead", githubOpenPRsQuery, map[string]any{"owner": owner, "name": repo, "head": head}, &raw); err != nil {
		return nil, err
	}
	if raw.Repository == nil {
		return nil, fmt.Errorf("GitHub answered no repository %s/%s", owner, repo)
	}
	pulls := raw.Repository.PullRequests.Nodes
	// GitHub spells states uppercase ("OPEN"); callers read the canonical
	// lowercase vocabulary.
	for i := range pulls {
		pulls[i].State = NormalizePRState(pulls[i].State)
	}
	return pulls, nil
}

// githubMergedPRsQuery lists merged pull requests, most recently updated
// first.
const githubMergedPRsQuery = `query MergedPRs($owner: String!, $name: String!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: [MERGED], first: $first, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { headRefName headRefOid url }
    }
  }
}`

// ListMergedPRHeads fetches up to limit recently merged PRs' head
// coordinates, 100 per request. Recency-bounded by limit: old merges fall
// off the window, which prune surfaces as "no merged PR found" rather
// than an error.
func (f *githubForge) ListMergedPRHeads(ctx context.Context, cwd string, limit int) ([]MergedPRHead, error) {
	if limit <= 0 {
		return nil, errors.New("merged PR list limit must be positive")
	}
	client, owner, repo, err := f.githubOriginRepo(cwd)
	if err != nil {
		return nil, err
	}
	heads := make([]MergedPRHead, 0, min(limit, githubReadPageSize))
	var after any
	for len(heads) < limit {
		var raw struct {
			Repository *struct {
				PullRequests struct {
					PageInfo githubPageInfo `json:"pageInfo"`
					Nodes    []struct {
						HeadRefName string `json:"headRefName"`
						HeadRefOid  string `json:"headRefOid"`
						URL         string `json:"url"`
					} `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": repo, "first": min(limit-len(heads), githubReadPageSize), "after": after}
		if _, err := client.GraphQL(ctx, "MergedPRs", githubMergedPRsQuery, vars, &raw); err != nil {
			return nil, err
		}
		if raw.Repository == nil {
			return nil, fmt.Errorf("GitHub answered no repository %s/%s", owner, repo)
		}
		for _, row := range raw.Repository.PullRequests.Nodes {
			heads = append(heads, MergedPRHead{HeadRefName: row.HeadRefName, HeadOid: row.HeadRefOid, URL: row.URL})
		}
		page := raw.Repository.PullRequests.PageInfo
		if !page.HasNextPage || page.EndCursor == "" {
			break
		}
		after = page.EndCursor
	}
	return heads, nil
}

func (f *githubForge) SubmitReview(ctx context.Context, ref PRReference, review SubmitReviewRequest) (SubmitReviewResult, error) {
	number := ref.Number
	if _, _, err := splitGitHubProject(ref.Project()); err != nil {
		return SubmitReviewResult{}, err
	}
	if number <= 0 {
		return SubmitReviewResult{}, fmt.Errorf("PR number must be positive, got %d", number)
	}
	headSHA := ""
	fileComments := make([]ReviewLineComment, 0)
	lineComments := make([]ReviewLineComment, 0, len(review.Comments))
	for _, comment := range review.Comments {
		if strings.EqualFold(comment.Side, "file") {
			fileComments = append(fileComments, comment)
			continue
		}
		lineComments = append(lineComments, comment)
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	if len(fileComments) > 0 {
		read, err := f.ReadPR(ctx, ref, PRReadParts{Detail: true}, nil, nil)
		if err != nil {
			return SubmitReviewResult{}, err
		}
		headSHA = read.Detail.HeadSHA
	}
	pulls := fmt.Sprintf("%s/pulls/%d", githubRepoPath(ref), number)
	body, err := githubReviewRequestBody(review, lineComments)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: pulls + "/reviews", Body: body}, nil); err != nil {
		return SubmitReviewResult{}, err
	}
	out := SubmitReviewResult{PostedReview: true}
	for _, comment := range fileComments {
		body, err := githubFileCommentRequestBody(comment, headSHA)
		if err != nil {
			return out, err
		}
		if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: pulls + "/comments", Body: body}, nil); err != nil {
			return out, &PartialSubmitError{PostedReview: true, PostedFileComments: out.PostedFileComments, FailedPath: comment.Path, Err: err}
		}
		out.PostedFileComments++
	}
	return out, nil
}

// githubReviewIn is the REST review submission body.
type githubReviewIn struct {
	Event    string                  `json:"event"`
	Body     string                  `json:"body,omitempty"`
	Comments []githubReviewCommentIn `json:"comments,omitempty"`
}

func githubReviewRequestBody(review SubmitReviewRequest, comments []ReviewLineComment) (githubReviewIn, error) {
	payload := githubReviewIn{
		Event: githubReviewEvent(review.Verdict),
		Body:  review.Body,
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
		Path:      comment.Path,
		Body:      comment.Body,
		Line:      *comment.Line,
		Side:      side,
		StartLine: comment.StartLine,
	}
	if comment.StartLine != nil {
		out.StartSide = side
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

func (f *githubForge) ReplyToThread(ctx context.Context, ref PRReference, _ string, databaseID int64, body string) error {
	if _, _, err := splitGitHubProject(ref.Project()); err != nil {
		return err
	}
	if ref.Number <= 0 {
		return fmt.Errorf("PR number must be positive, got %d", ref.Number)
	}
	if databaseID <= 0 {
		return errors.New("GitHub review reply requires the root comment databaseID")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("reply body is required")
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/pulls/%d/comments/%d/replies", githubRepoPath(ref), ref.Number, databaseID)
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: path, Body: map[string]string{"body": body}}, nil)
	return err
}

// githubSetThreadResolvedMutation runs resolveReviewThread or
// unresolveReviewThread on one thread, chosen by $resolved, with the
// thread's state after it.
const githubSetThreadResolvedMutation = `mutation SetThreadResolved($threadID: ID!, $resolved: Boolean!) {
  resolve: resolveReviewThread(input: {threadId: $threadID}) @include(if: $resolved) { thread { isResolved } }
  unresolve: unresolveReviewThread(input: {threadId: $threadID}) @skip(if: $resolved) { thread { isResolved } }
}`

// SetThreadResolved flips one review thread's resolved state. The thread
// NODE id is unique on its host, so only the reference's host is used
// here, the mirror of ReplyToThread, which needs the REST coordinates and
// ignores the node id.
//
// The mutation's answer is read back rather than trusted: a 200 that
// resolved nothing would otherwise reach the user as a success and leave
// the pane showing a state the forge does not have.
func (f *githubForge) SetThreadResolved(ctx context.Context, ref PRReference, threadID string, resolved bool) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("GitHub thread resolution requires a review thread id")
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	type payload struct {
		Thread *struct {
			IsResolved bool `json:"isResolved"`
		} `json:"thread"`
	}
	var raw struct {
		Resolve   *payload `json:"resolve"`
		Unresolve *payload `json:"unresolve"`
	}
	if _, err := client.GraphQL(ctx, "SetThreadResolved", githubSetThreadResolvedMutation, map[string]any{"threadID": threadID, "resolved": resolved}, &raw); err != nil {
		return err
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
// remote is missing or its host is not a recognised forge.
func (c *Core) CreatePR(ctx context.Context, cwd, title, body, base string, draft bool) (string, error) {
	return c.forgeFor(cwd).CreatePR(ctx, cwd, title, body, base, draft)
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
