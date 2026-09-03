package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitPR describes a pull request returned by a forge CLI. Fields are
// shared across forges (gh and glab both expose URL/number/title/state
// equivalents); per-forge wrappers map their native JSON shapes onto
// this struct.
type GitPR struct {
	URL    string `json:"url"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// githubViewerLoginTTL bounds how long the authenticated gh login is
// memoized. The login only changes when the user re-authenticates the
// gh CLI under a different account, but GetPRDetail runs on every PR
// update poll tick — without the cache each tick would cost an extra
// `gh api user` subprocess and network round-trip.
const githubViewerLoginTTL = 15 * time.Minute

// githubForge implements Forge using the gh CLI. All operations route
// through the owning Core's runBinary for timeout + size-cap discipline.
//
// # Rule for every `--json` field list below
//
// The field set is a contract with whatever `gh` the user happens to have
// installed, and `gh` has shipped fields that older releases silently omit
// — `headRepository.nameWithOwner`, added for fork matching, is absent
// before gh 2.47. A decoder that treats such a field as required turns
// "old gh" into "this PR does not exist": t3-code lost PR rows exactly this
// way, with no error anywhere because the omission is well-formed JSON.
//
// So, when adding a field here:
//
//  1. Decode it as optional. A zero value must degrade the feature that
//     wanted it, never drop the row.
//  2. Never widen a field list to satisfy a nested-object decode without
//     checking when `gh` started emitting that object's members.
//
// Today's lists are narrow (`url,number,title,state` and friends) and so
// immune by accident, not by design — this rule is what makes the next
// addition safe.
type githubForge struct {
	core *Core

	viewerMu        sync.Mutex
	viewerLogin     string
	viewerExpiresAt time.Time
}

func (f *githubForge) ID() string         { return "github" }
func (f *githubForge) BinaryName() string { return "gh" }

// CreatePR opens a pull request via GitHub CLI and returns the created URL.
// When draft is true the PR is opened as a draft (gh pr create --draft).
func (f *githubForge) CreatePR(cwd, title, body, base string, draft bool) (string, error) {
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
	result, err := f.core.runBinaryInteractive("gh", cwd, args...)
	if err != nil {
		return "", normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("gh pr create failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	url := strings.TrimSpace(result.stdout)
	if url == "" {
		return "", errors.New("gh pr create returned empty URL")
	}
	return url, nil
}

// ListOpenPRs returns open pull requests for the given head branch.
func (f *githubForge) ListOpenPRs(cwd, head string) ([]GitPR, error) {
	if strings.TrimSpace(head) == "" {
		return nil, errors.New("pull request head branch is required")
	}

	result, err := f.core.runBinary(
		"gh",
		cwd,
		"pr",
		"list",
		"--head",
		head,
		"--state",
		"open",
		"--json",
		"url,number,title,state",
	)
	if err != nil {
		return nil, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("gh pr list failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	stdout := strings.TrimSpace(result.stdout)
	if stdout == "" {
		return nil, nil
	}

	var pulls []GitPR
	if err := json.Unmarshal([]byte(stdout), &pulls); err != nil {
		return nil, fmt.Errorf("decode gh pr list output: %w", err)
	}
	// gh emits states uppercase ("OPEN" / "CLOSED" / "MERGED"); collapse
	// to the canonical lowercase vocabulary so callers don't have to
	// branch on forge.
	for i := range pulls {
		pulls[i].State = NormalizePRState(pulls[i].State)
	}
	return pulls, nil
}

// ListMergedPRHeads fetches recently merged PRs' head coordinates in
// one `gh pr list` call. Recency-bounded by limit — old merges fall off
// the window, which prune surfaces as "no merged PR found" rather than
// an error.
func (f *githubForge) ListMergedPRHeads(cwd string, limit int) ([]MergedPRHead, error) {
	if limit <= 0 {
		return nil, errors.New("merged PR list limit must be positive")
	}

	result, err := f.core.runBinary(
		"gh",
		cwd,
		"pr",
		"list",
		"--state",
		"merged",
		"--limit",
		strconv.Itoa(limit),
		"--json",
		"headRefName,headRefOid,url",
	)
	if err != nil {
		return nil, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("gh pr list failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	stdout := strings.TrimSpace(result.stdout)
	if stdout == "" {
		return nil, nil
	}

	var rows []struct {
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		return nil, fmt.Errorf("decode gh pr list output: %w", err)
	}
	heads := make([]MergedPRHead, 0, len(rows))
	for _, row := range rows {
		heads = append(heads, MergedPRHead{
			HeadRefName: row.HeadRefName,
			HeadOid:     row.HeadRefOid,
			URL:         row.URL,
		})
	}
	return heads, nil
}

// ViewPR fetches PR metadata via `gh pr view --json ...`. project is
// "owner/repo"; cwd may be empty when there is no local clone.
func (f *githubForge) ViewPR(cwd, project string, number int) (PRMetadata, error) {
	if strings.TrimSpace(project) == "" {
		return PRMetadata{}, errors.New("project (owner/repo) is required")
	}
	if number <= 0 {
		return PRMetadata{}, fmt.Errorf("PR number must be positive, got %d", number)
	}

	result, err := f.core.runBinary(
		"gh",
		cwd,
		"pr", "view",
		"--repo", project,
		strconv.Itoa(number),
		"--json", "title,body,headRefName,baseRefName,files,url,author,state",
	)
	if err != nil {
		return PRMetadata{}, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return PRMetadata{}, fmt.Errorf("gh pr view failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}

	var raw struct {
		Title       string   `json:"title"`
		Body        string   `json:"body"`
		HeadRefName string   `json:"headRefName"`
		BaseRefName string   `json:"baseRefName"`
		URL         string   `json:"url"`
		Files       []PRFile `json:"files"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
		return PRMetadata{}, fmt.Errorf("gh pr view returned malformed JSON: %w", err)
	}
	return PRMetadata{
		Title:       raw.Title,
		Body:        raw.Body,
		HeadRefName: raw.HeadRefName,
		BaseRefName: raw.BaseRefName,
		URL:         raw.URL,
		AuthorLogin: raw.Author.Login,
		State:       NormalizePRState(raw.State),
		Files:       raw.Files,
	}, nil
}

// GetPRDetail fetches the review-pane PR detail via gh's JSON view plus
// a tiny authenticated-user probe for own-PR verdict gating.
func (f *githubForge) GetPRDetail(cwd, project string, number int) (PRDetail, error) {
	if strings.TrimSpace(project) == "" {
		return PRDetail{}, errors.New("project (owner/repo) is required")
	}
	if number <= 0 {
		return PRDetail{}, fmt.Errorf("PR number must be positive, got %d", number)
	}
	result, err := f.core.runBinary(
		"gh",
		cwd,
		"pr", "view",
		"--repo", project,
		strconv.Itoa(number),
		"--json", strings.Join(githubPRDetailFields, ","),
	)
	if err != nil {
		return PRDetail{}, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return PRDetail{}, githubCommandFailure("gh pr view failed", result)
	}
	detail, err := parseGitHubPRDetail(result.stdout)
	if err != nil {
		return PRDetail{}, fmt.Errorf("gh pr view returned malformed JSON: %w", err)
	}
	viewer, err := f.githubViewerLogin(cwd)
	if err != nil {
		return PRDetail{}, err
	}
	detail.ViewerIsAuthor = viewer != "" && strings.EqualFold(viewer, detail.AuthorLogin)
	return detail, nil
}

func (f *githubForge) githubViewerLogin(cwd string) (string, error) {
	now := f.core.nowFn()
	f.viewerMu.Lock()
	if f.viewerLogin != "" && f.viewerExpiresAt.After(now) {
		login := f.viewerLogin
		f.viewerMu.Unlock()
		return login, nil
	}
	f.viewerMu.Unlock()

	result, err := f.core.runBinary("gh", cwd, "api", "user", "--jq", ".login")
	if err != nil {
		return "", normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return "", githubCommandFailure("gh api user failed", result)
	}
	login := strings.TrimSpace(result.stdout)
	if login != "" {
		f.viewerMu.Lock()
		f.viewerLogin = login
		f.viewerExpiresAt = now.Add(githubViewerLoginTTL)
		f.viewerMu.Unlock()
	}
	return login, nil
}

// githubPRDetailFields is the widest `--json` list this package asks for.
// Before adding to it, read the version-drift rule on githubForge.
var githubPRDetailFields = []string{
	"title", "body", "author", "state", "baseRefName", "headRefName",
	"headRefOid", "reviews", "statusCheckRollup", "isDraft", "mergeable",
	"number", "url", "additions", "deletions", "changedFiles",
	"mergeStateStatus", "reviewDecision",
}

func parseGitHubPRDetail(stdout string) (PRDetail, error) {
	var raw struct {
		Number            int               `json:"number"`
		Title             string            `json:"title"`
		Body              string            `json:"body"`
		State             string            `json:"state"`
		IsDraft           bool              `json:"isDraft"`
		BaseRefName       string            `json:"baseRefName"`
		HeadRefName       string            `json:"headRefName"`
		HeadRefOID        string            `json:"headRefOid"`
		URL               string            `json:"url"`
		Additions         int               `json:"additions"`
		Deletions         int               `json:"deletions"`
		ChangedFiles      int               `json:"changedFiles"`
		Mergeable         string            `json:"mergeable"`
		MergeStateStatus  string            `json:"mergeStateStatus"`
		ReviewDecision    string            `json:"reviewDecision"`
		StatusCheckRollup []json.RawMessage `json:"statusCheckRollup"`
		Author            struct {
			Login string `json:"login"`
		} `json:"author"`
		Reviews []githubReviewRaw `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return PRDetail{}, err
	}
	return PRDetail{
		Number:         raw.Number,
		Title:          raw.Title,
		Body:           raw.Body,
		AuthorLogin:    raw.Author.Login,
		State:          NormalizePRState(raw.State),
		Draft:          raw.IsDraft,
		HeadRefName:    raw.HeadRefName,
		BaseRefName:    raw.BaseRefName,
		HeadSHA:        raw.HeadRefOID,
		URL:            raw.URL,
		Additions:      raw.Additions,
		Deletions:      raw.Deletions,
		ChangedFiles:   raw.ChangedFiles,
		ReviewDecision: raw.ReviewDecision,
		LatestReviews:  latestGitHubReviews(raw.Reviews),
		Checks:         parseGitHubCheckSummary(raw.StatusCheckRollup),
		Mergeability:   normalizeGitHubMergeability(raw.Mergeable, raw.MergeStateStatus),
	}, nil
}

type githubReviewRaw struct {
	Body        string `json:"body"`
	SubmittedAt string `json:"submittedAt"`
	State       string `json:"state"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	Commit struct {
		OID string `json:"oid"`
	} `json:"commit"`
}

func latestGitHubReviews(reviews []githubReviewRaw) []ReviewVerdict {
	latest := make(map[string]ReviewVerdict)
	order := make([]string, 0, len(reviews))
	for _, review := range reviews {
		if review.Author.Login == "" || review.State == "" {
			continue
		}
		if _, ok := latest[review.Author.Login]; !ok {
			order = append(order, review.Author.Login)
		}
		latest[review.Author.Login] = ReviewVerdict{
			AuthorLogin: review.Author.Login,
			State:       review.State,
			SubmittedAt: review.SubmittedAt,
			Body:        review.Body,
			CommitSHA:   review.Commit.OID,
		}
	}
	out := make([]ReviewVerdict, 0, len(latest))
	for _, login := range order {
		out = append(out, latest[login])
	}
	return out
}

func parseGitHubCheckSummary(items []json.RawMessage) CheckSummary {
	var summary CheckSummary
	for _, item := range items {
		var tag struct {
			Type string `json:"__typename"`
		}
		if err := json.Unmarshal(item, &tag); err != nil {
			continue
		}
		check := CheckStatus{Kind: tag.Type}
		switch tag.Type {
		case "CheckRun":
			var raw struct {
				CompletedAt  string `json:"completedAt"`
				Conclusion   string `json:"conclusion"`
				DetailsURL   string `json:"detailsUrl"`
				Name         string `json:"name"`
				StartedAt    string `json:"startedAt"`
				Status       string `json:"status"`
				WorkflowName string `json:"workflowName"`
			}
			if err := json.Unmarshal(item, &raw); err != nil {
				continue
			}
			check.Name = raw.Name
			check.Workflow = raw.WorkflowName
			check.Status = raw.Status
			check.Conclusion = raw.Conclusion
			check.DetailsURL = raw.DetailsURL
			check.StartedAt = zeroTimeToEmpty(raw.StartedAt)
			check.CompletedAt = zeroTimeToEmpty(raw.CompletedAt)
		case "StatusContext":
			var raw struct {
				Context   string `json:"context"`
				StartedAt string `json:"startedAt"`
				State     string `json:"state"`
				TargetURL string `json:"targetUrl"`
			}
			if err := json.Unmarshal(item, &raw); err != nil {
				continue
			}
			check.Name = raw.Context
			check.Status = raw.State
			check.DetailsURL = raw.TargetURL
			check.StartedAt = zeroTimeToEmpty(raw.StartedAt)
		default:
			continue
		}
		summary.Checks = append(summary.Checks, check)
		summary.Total++
		addCheckBucket(&summary, check)
	}
	return summary
}

func addCheckBucket(summary *CheckSummary, check CheckStatus) {
	switch strings.ToUpper(firstNonEmpty(check.Conclusion, check.Status)) {
	case "SUCCESS":
		summary.Success++
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		summary.Failure++
	case "CANCELLED", "CANCELED":
		summary.Canceled++
	case "SKIPPED", "NEUTRAL":
		summary.Skipped++
	default:
		summary.Pending++
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func zeroTimeToEmpty(value string) string {
	if strings.HasPrefix(value, "0001-01-01T") {
		return ""
	}
	return value
}

func normalizeGitHubMergeability(mergeable, mergeStateStatus string) string {
	switch strings.ToUpper(strings.TrimSpace(mergeable)) {
	case "CONFLICTING":
		return MergeabilityConflicts
	case "UNKNOWN", "":
		return MergeabilityChecking
	case "MERGEABLE":
		if strings.EqualFold(mergeStateStatus, "DIRTY") {
			return MergeabilityConflicts
		}
		return MergeabilityClean
	default:
		return MergeabilityChecking
	}
}

// ListReviewThreads fetches GitHub review threads through GraphQL because
// gh porcelain and REST comments cannot expose nullable current anchors
// and databaseId in one grouped response. PR conversation comments
// (issue comments — GitHub keeps them flat, outside review threads) are
// appended as path-less single-comment threads so the review pane can
// list the whole discussion.
func (f *githubForge) ListReviewThreads(cwd, project string, number int) ([]ReviewThread, error) {
	owner, repo, err := splitGitHubProject(project)
	if err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("PR number must be positive, got %d", number)
	}
	var all []ReviewThread
	var after string
	for {
		query := githubReviewThreadsQuery(owner, repo, number, after)
		result, err := f.core.runBinary("gh", cwd, "api", "graphql", "-f", "query="+query)
		if err != nil {
			return nil, normalizeGitHubCLIError(err)
		}
		if result.exitCode != 0 {
			return nil, githubCommandFailure("gh api graphql reviewThreads failed", result)
		}
		threads, pageInfo, err := parseGitHubReviewThreads(result.stdout)
		if err != nil {
			return nil, fmt.Errorf("gh api graphql reviewThreads returned malformed JSON: %w", err)
		}
		all = append(all, threads...)
		if !pageInfo.HasNextPage {
			break
		}
		after = pageInfo.EndCursor
	}
	conversation, err := f.listPRConversationThreads(cwd, owner, repo, number)
	if err != nil {
		return nil, err
	}
	return append(all, conversation...), nil
}

func (f *githubForge) listPRConversationThreads(cwd, owner, repo string, number int) ([]ReviewThread, error) {
	var all []ReviewThread
	var after string
	for {
		query := githubPRCommentsQuery(owner, repo, number, after)
		result, err := f.core.runBinary("gh", cwd, "api", "graphql", "-f", "query="+query)
		if err != nil {
			return nil, normalizeGitHubCLIError(err)
		}
		if result.exitCode != 0 {
			return nil, githubCommandFailure("gh api graphql pullRequest comments failed", result)
		}
		threads, pageInfo, err := parseGitHubPRComments(result.stdout)
		if err != nil {
			return nil, fmt.Errorf("gh api graphql pullRequest comments returned malformed JSON: %w", err)
		}
		all = append(all, threads...)
		if !pageInfo.HasNextPage {
			return all, nil
		}
		after = pageInfo.EndCursor
	}
}

func parseGitHubPRComments(stdout string) ([]ReviewThread, githubPageInfo, error) {
	var raw struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					Comments struct {
						PageInfo githubPageInfo `json:"pageInfo"`
						Nodes    []struct {
							ID          string `json:"id"`
							DatabaseID  int64  `json:"databaseId"`
							Body        string `json:"body"`
							CreatedAt   string `json:"createdAt"`
							IsMinimized bool   `json:"isMinimized"`
							Author      struct {
								Login string `json:"login"`
							} `json:"author"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return nil, githubPageInfo{}, err
	}
	nodes := raw.Data.Repository.PullRequest.Comments.Nodes
	threads := make([]ReviewThread, 0, len(nodes))
	for _, node := range nodes {
		if node.IsMinimized {
			continue
		}
		threads = append(threads, ReviewThread{
			ID: node.ID,
			Comments: []ReviewComment{{
				AuthorLogin: node.Author.Login,
				Body:        node.Body,
				CreatedAt:   node.CreatedAt,
				DatabaseID:  node.DatabaseID,
			}},
		})
	}
	return threads, raw.Data.Repository.PullRequest.Comments.PageInfo, nil
}

func githubPRCommentsQuery(owner, repo string, number int, after string) string {
	afterClause := ""
	if after != "" {
		afterClause = fmt.Sprintf(`, after: %q`, after)
	}
	return fmt.Sprintf(`query {
  repository(owner: %q, name: %q) {
    pullRequest(number: %d) {
      comments(first: 50%s) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          databaseId
          author { login }
          body
          createdAt
          isMinimized
        }
      }
    }
  }
}`, owner, repo, number, afterClause)
}

type githubPageInfo struct {
	HasNextPage bool
	EndCursor   string
}

func parseGitHubReviewThreads(stdout string) ([]ReviewThread, githubPageInfo, error) {
	var raw struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo githubPageInfo `json:"pageInfo"`
						Nodes    []struct {
							ID            string `json:"id"`
							IsResolved    bool   `json:"isResolved"`
							IsOutdated    bool   `json:"isOutdated"`
							Path          string `json:"path"`
							Line          *int   `json:"line"`
							StartLine     *int   `json:"startLine"`
							DiffSide      string `json:"diffSide"`
							StartDiffSide string `json:"startDiffSide"`
							SubjectType   string `json:"subjectType"`
							Comments      struct {
								Nodes []struct {
									DatabaseID int64  `json:"databaseId"`
									Body       string `json:"body"`
									CreatedAt  string `json:"createdAt"`
									Author     struct {
										Login string `json:"login"`
									} `json:"author"`
									ReplyTo *struct {
										ID         string `json:"id"`
										DatabaseID int64  `json:"databaseId"`
									} `json:"replyTo"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		return nil, githubPageInfo{}, err
	}
	nodes := raw.Data.Repository.PullRequest.ReviewThreads.Nodes
	threads := make([]ReviewThread, 0, len(nodes))
	for _, node := range nodes {
		side := strings.ToLower(node.DiffSide)
		if strings.EqualFold(node.SubjectType, "FILE") {
			side = "file"
		}
		thread := ReviewThread{
			ID:           node.ID,
			Path:         node.Path,
			Line:         node.Line,
			StartLine:    node.StartLine,
			Side:         side,
			IsResolvable: true,
			IsResolved:   node.IsResolved,
			IsOutdated:   node.IsOutdated,
			Comments:     make([]ReviewComment, 0, len(node.Comments.Nodes)),
		}
		for _, comment := range node.Comments.Nodes {
			out := ReviewComment{
				AuthorLogin: comment.Author.Login,
				Body:        comment.Body,
				CreatedAt:   comment.CreatedAt,
				DatabaseID:  comment.DatabaseID,
			}
			if comment.ReplyTo != nil {
				out.ReplyTo = &ReviewReplyTo{ID: comment.ReplyTo.ID, DatabaseID: comment.ReplyTo.DatabaseID}
			}
			thread.Comments = append(thread.Comments, out)
		}
		threads = append(threads, thread)
	}
	return threads, raw.Data.Repository.PullRequest.ReviewThreads.PageInfo, nil
}

func githubReviewThreadsQuery(owner, repo string, number int, after string) string {
	afterClause := ""
	if after != "" {
		afterClause = fmt.Sprintf(`, after: %q`, after)
	}
	return fmt.Sprintf(`query {
  repository(owner: %q, name: %q) {
    pullRequest(number: %d) {
      reviewThreads(first: 50%s) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          startLine
          diffSide
          startDiffSide
          subjectType
          comments(first: 50) {
            nodes {
              id
              databaseId
              author { login }
              body
              createdAt
              replyTo { id databaseId }
            }
          }
        }
      }
    }
  }
}`, owner, repo, number, afterClause)
}

func (f *githubForge) SubmitReview(cwd, project string, number int, review SubmitReviewRequest) (SubmitReviewResult, error) {
	owner, repo, err := splitGitHubProject(project)
	if err != nil {
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
	if len(fileComments) > 0 {
		detail, err := f.GetPRDetail(cwd, project, number)
		if err != nil {
			return SubmitReviewResult{}, err
		}
		headSHA = detail.HeadSHA
	}
	endpoint := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, repo, number)
	body, err := githubReviewRequestBody(review, lineComments)
	if err != nil {
		return SubmitReviewResult{}, err
	}
	result, err := f.core.runBinaryInput("gh", cwd, string(body), "api", endpoint, "-X", "POST", "--input", "-")
	if err != nil {
		return SubmitReviewResult{}, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return SubmitReviewResult{}, githubCommandFailure("gh api submit review failed", result)
	}
	out := SubmitReviewResult{PostedReview: true}
	for _, comment := range fileComments {
		body, err := githubFileCommentRequestBody(comment, headSHA)
		if err != nil {
			return out, err
		}
		result, err := f.core.runBinaryInput(
			"gh",
			cwd,
			string(body),
			"api",
			fmt.Sprintf("repos/%s/%s/pulls/%d/comments", owner, repo, number),
			"-X", "POST",
			"--input", "-",
		)
		if err != nil {
			return out, &PartialSubmitError{PostedReview: true, PostedFileComments: out.PostedFileComments, FailedPath: comment.Path, Err: normalizeGitHubCLIError(err)}
		}
		if result.exitCode != 0 {
			return out, &PartialSubmitError{PostedReview: true, PostedFileComments: out.PostedFileComments, FailedPath: comment.Path, Err: githubCommandFailure("gh api file-level comment failed", result)}
		}
		out.PostedFileComments++
	}
	return out, nil
}

func githubReviewRequestBody(review SubmitReviewRequest, comments []ReviewLineComment) ([]byte, error) {
	payload := struct {
		Event    string                  `json:"event"`
		Body     string                  `json:"body,omitempty"`
		Comments []githubReviewCommentIn `json:"comments,omitempty"`
	}{
		Event: githubReviewEvent(review.Verdict),
		Body:  review.Body,
	}
	for _, comment := range comments {
		in, err := githubReviewCommentBody(comment)
		if err != nil {
			return nil, err
		}
		payload.Comments = append(payload.Comments, in)
	}
	return json.Marshal(payload)
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

func githubFileCommentRequestBody(comment ReviewLineComment, headSHA string) ([]byte, error) {
	if strings.TrimSpace(comment.Path) == "" {
		return nil, errors.New("file-level comment path is required")
	}
	if strings.TrimSpace(comment.Body) == "" {
		return nil, errors.New("file-level comment body is required")
	}
	if strings.TrimSpace(headSHA) == "" {
		return nil, errors.New("file-level comment requires PR head SHA")
	}
	payload := struct {
		Body        string `json:"body"`
		CommitID    string `json:"commit_id"`
		Path        string `json:"path"`
		SubjectType string `json:"subject_type"`
	}{
		Body:        comment.Body,
		CommitID:    headSHA,
		Path:        comment.Path,
		SubjectType: "file",
	}
	return json.Marshal(payload)
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

func (f *githubForge) ReplyToThread(cwd, project string, number int, _ string, databaseID int64, body string) error {
	owner, repo, err := splitGitHubProject(project)
	if err != nil {
		return err
	}
	if number <= 0 {
		return fmt.Errorf("PR number must be positive, got %d", number)
	}
	if databaseID <= 0 {
		return errors.New("GitHub review reply requires the root comment databaseID")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("reply body is required")
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	result, err := f.core.runBinaryInput(
		"gh",
		cwd,
		string(payload),
		"api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, number, databaseID),
		"-X", "POST",
		"--input", "-",
	)
	if err != nil {
		return normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return githubCommandFailure("gh api reply failed", result)
	}
	return nil
}

// SetThreadResolved flips one review thread's resolved state through the
// GraphQL resolveReviewThread / unresolveReviewThread mutations. The
// thread NODE id is the whole address — it is globally unique — so
// project and number are unused here, the mirror of ReplyToThread, which
// needs the REST coordinates and ignores the node id.
//
// The mutation's answer is read back rather than trusted: `gh` exits
// non-zero on a GraphQL error, but a 200 that resolved nothing would
// otherwise reach the user as a success and leave the pane showing a
// state the forge does not have.
func (f *githubForge) SetThreadResolved(cwd, _ string, _ int, threadID string, resolved bool) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("GitHub thread resolution requires a review thread id")
	}
	mutation := githubResolveMutation(resolved)
	result, err := f.core.runBinary("gh", cwd, "api", "graphql", "-f", "query="+githubSetThreadResolvedQuery(mutation, threadID))
	if err != nil {
		return normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return githubCommandFailure("gh api graphql "+mutation+" failed", result)
	}
	var raw struct {
		Data struct {
			Thread struct {
				Thread struct {
					IsResolved bool `json:"isResolved"`
				} `json:"thread"`
			} `json:"thread"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
		return fmt.Errorf("gh api graphql %s returned malformed JSON: %w", mutation, err)
	}
	if raw.Data.Thread.Thread.IsResolved != resolved {
		return fmt.Errorf("GitHub reported review thread %s as isResolved=%t after requesting %t", threadID, raw.Data.Thread.Thread.IsResolved, resolved)
	}
	return nil
}

func githubResolveMutation(resolved bool) string {
	if resolved {
		return "resolveReviewThread"
	}
	return "unresolveReviewThread"
}

// githubSetThreadResolvedQuery aliases either mutation to `thread` so one
// decode shape reads both answers.
func githubSetThreadResolvedQuery(mutation, threadID string) string {
	return fmt.Sprintf(`mutation {
  thread: %s(input: {threadId: %q}) {
    thread { isResolved }
  }
}`, mutation, threadID)
}

func splitGitHubProject(project string) (string, string, error) {
	namespace, repo, err := SplitProjectForForge("github", project)
	if err != nil {
		return "", "", err
	}
	return namespace, repo, nil
}

// Diff returns the unified diff for a PR via `gh pr diff`.
func (f *githubForge) Diff(cwd, project string, number int) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", errors.New("project (owner/repo) is required")
	}
	if number <= 0 {
		return "", fmt.Errorf("PR number must be positive, got %d", number)
	}

	result, err := f.core.runBinaryWithLimit(
		"gh",
		cwd,
		maxPRDiffBytes,
		"pr", "diff",
		"--repo", project,
		strconv.Itoa(number),
	)
	if err != nil {
		return "", normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("gh pr diff failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}
	return result.stdout, nil
}

// CreatePR is a thin wrapper that dispatches to the forge detected for
// cwd. Returns ErrUnsupportedForge (via nullForge) when the origin
// remote is missing or its host is not a recognised forge.
func (c *Core) CreatePR(cwd, title, body, base string, draft bool) (string, error) {
	return c.forgeFor(cwd).CreatePR(cwd, title, body, base, draft)
}

// ListOpenPRs is a thin wrapper that dispatches to the forge detected
// for cwd. See CreatePR for the dispatch model.
func (c *Core) ListOpenPRs(cwd, head string) ([]GitPR, error) {
	return c.forgeFor(cwd).ListOpenPRs(cwd, head)
}

// ListMergedPRHeads is a thin wrapper that dispatches to the forge
// detected for cwd. See CreatePR for the dispatch model.
func (c *Core) ListMergedPRHeads(cwd string, limit int) ([]MergedPRHead, error) {
	return c.forgeFor(cwd).ListMergedPRHeads(cwd, limit)
}

func normalizeGitHubCLIError(err error) error {
	if _, ok := errors.AsType[*exec.Error](err); ok || errors.Is(err, exec.ErrNotFound) {
		return &ForgeSetupError{
			Forge:   "github",
			Binary:  "gh",
			Kind:    "missing",
			Message: "GitHub CLI (`gh`) is not installed or not on PATH. Install from https://cli.github.com and run 'gh auth login' to continue",
			Err:     err,
		}
	}
	return err
}

func githubCommandFailure(prefix string, result commandResult) error {
	message := commandOutputMessage(result.stdout, result.stderr)
	if isGitHubAuthMessage(message) {
		return &ForgeSetupError{
			Forge:   "github",
			Binary:  "gh",
			Kind:    "unauthenticated",
			Message: "GitHub CLI (`gh`) is not authenticated. Run 'gh auth login' to continue",
		}
	}
	return fmt.Errorf("%s: %s", prefix, message)
}

func isGitHubAuthMessage(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "gh auth login") ||
		strings.Contains(lower, "not logged") ||
		strings.Contains(lower, "authentication required") ||
		strings.Contains(lower, "requires authentication")
}

func commandOutputMessage(stdout, stderr string) string {
	if message := strings.TrimSpace(stderr); message != "" {
		return message
	}
	if message := strings.TrimSpace(stdout); message != "" {
		return message
	}
	return "command failed"
}
