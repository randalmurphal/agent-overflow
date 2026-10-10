package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// The GitHub PR read: one GraphQL operation, PRTick, per pump tick, with
// dedicated page operations for any connection longer than one page. The
// request document is the same every tick; the parts a caller does not
// want are left out with @include so their cost is not paid. Selection
// text is shared by Go constants rather than GraphQL fragments, so each
// operation is self-contained.

// githubReadPageSize is the page size of every connection the read
// selects, the GraphQL API's maximum.
const githubReadPageSize = 100

// githubReadMaxPages bounds the pages read of one connection in one read:
// review threads, conversation comments, one thread's comments and the
// rollup's check contexts each stop after this many pages
// (githubReadMaxPages * githubReadPageSize nodes). A PR past it shows the
// first nodes in the forge's order.
const githubReadMaxPages = 10

const (
	githubPageInfoFields = `pageInfo { hasNextPage endCursor }`
	githubActor          = `author { login ... on User { name } }`

	githubThreadCommentFields = `id databaseId ` + githubActor + ` body createdAt replyTo { id databaseId }`
	githubThreadFields        = `id isResolved isOutdated path line startLine diffSide startDiffSide subjectType ` +
		`comments(first: 100) { ` + githubPageInfoFields + ` nodes { ` + githubThreadCommentFields + ` } }`
	githubCommentFields = `id databaseId ` + githubActor + ` body createdAt isMinimized`
	githubContextFields = `__typename ` +
		`... on CheckRun { databaseId name status conclusion startedAt completedAt detailsUrl checkSuite { workflowRun { databaseId url workflow { name } } } } ` +
		`... on StatusContext { context state targetUrl createdAt description }`
	githubDetailFields = `title body state isDraft baseRefName headRefName headRefOid url ` +
		`additions deletions changedFiles mergeable mergeStateStatus reviewDecision ` + githubActor + ` ` +
		`latestReviews(first: 100) { nodes { ` + githubActor + ` body submittedAt state commit { oid } } }`
)

// githubPRTickQuery reads a PR's detail, review threads and conversation
// comments, and the head commit's check rollup. $wantDetail gates the
// detail and the viewer, $wantThreads the threads and comments, and
// $wantChecks the rollup, which both the detail's check summary and the
// CI pipeline are built from.
const githubPRTickQuery = `query PRTick($owner: String!, $name: String!, $number: Int!, $wantDetail: Boolean!, $wantThreads: Boolean!, $wantChecks: Boolean!) {
  viewer @include(if: $wantDetail) { login }
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      number
      ... @include(if: $wantDetail) { ` + githubDetailFields + ` }
      commits(last: 1) @include(if: $wantChecks) { nodes { commit { id statusCheckRollup { contexts(first: 100) { ` + githubPageInfoFields + ` nodes { ` + githubContextFields + ` } } } } } }
      reviewThreads(first: 100) @include(if: $wantThreads) { ` + githubPageInfoFields + ` nodes { ` + githubThreadFields + ` } }
      comments(first: 100) @include(if: $wantThreads) { ` + githubPageInfoFields + ` nodes { ` + githubCommentFields + ` } }
    }
  }
}`

const githubPRThreadsPageQuery = `query PRThreadsPage($owner: String!, $name: String!, $number: Int!, $after: String!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) { ` + githubPageInfoFields + ` nodes { ` + githubThreadFields + ` } }
    }
  }
}`

const githubPRCommentsPageQuery = `query PRCommentsPage($owner: String!, $name: String!, $number: Int!, $after: String!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      comments(first: 100, after: $after) { ` + githubPageInfoFields + ` nodes { ` + githubCommentFields + ` } }
    }
  }
}`

const githubThreadCommentsPageQuery = `query ThreadCommentsPage($threadID: ID!, $after: String!) {
  node(id: $threadID) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $after) { ` + githubPageInfoFields + ` nodes { ` + githubThreadCommentFields + ` } }
    }
  }
}`

const githubRollupContextsPageQuery = `query RollupContextsPage($commitID: ID!, $after: String!) {
  node(id: $commitID) {
    ... on Commit {
      statusCheckRollup { contexts(first: 100, after: $after) { ` + githubPageInfoFields + ` nodes { ` + githubContextFields + ` } } }
    }
  }
}`

type githubPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// githubActorRaw is a GraphQL Actor selected as
// `author { login ... on User { name } }`. A Bot or Mannequin has no name
// key and a User without a display name answers null or ""; all decode
// to "". A deleted account is a null author, which decodes to "".
type githubActorRaw struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

type githubThreadCommentRaw struct {
	ID         string         `json:"id"`
	DatabaseID int64          `json:"databaseId"`
	Body       string         `json:"body"`
	CreatedAt  string         `json:"createdAt"`
	Author     githubActorRaw `json:"author"`
	ReplyTo    *struct {
		ID         string `json:"id"`
		DatabaseID int64  `json:"databaseId"`
	} `json:"replyTo"`
}

type githubThreadCommentsRaw struct {
	PageInfo githubPageInfo           `json:"pageInfo"`
	Nodes    []githubThreadCommentRaw `json:"nodes"`
}

type githubThreadRaw struct {
	ID            string                  `json:"id"`
	IsResolved    bool                    `json:"isResolved"`
	IsOutdated    bool                    `json:"isOutdated"`
	Path          string                  `json:"path"`
	Line          *int                    `json:"line"`
	StartLine     *int                    `json:"startLine"`
	DiffSide      string                  `json:"diffSide"`
	StartDiffSide string                  `json:"startDiffSide"`
	SubjectType   string                  `json:"subjectType"`
	Comments      githubThreadCommentsRaw `json:"comments"`
}

type githubThreadsRaw struct {
	PageInfo githubPageInfo    `json:"pageInfo"`
	Nodes    []githubThreadRaw `json:"nodes"`
}

type githubCommentRaw struct {
	ID          string         `json:"id"`
	DatabaseID  int64          `json:"databaseId"`
	Body        string         `json:"body"`
	CreatedAt   string         `json:"createdAt"`
	IsMinimized bool           `json:"isMinimized"`
	Author      githubActorRaw `json:"author"`
}

type githubCommentsRaw struct {
	PageInfo githubPageInfo     `json:"pageInfo"`
	Nodes    []githubCommentRaw `json:"nodes"`
}

// githubContextRaw is one StatusCheckRollupContext: a CheckRun or a
// StatusContext, told apart by __typename. Times are null when absent.
type githubContextRaw struct {
	Typename    string `json:"__typename"`
	DatabaseID  int64  `json:"databaseId"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
	DetailsURL  string `json:"detailsUrl"`
	CheckSuite  *struct {
		WorkflowRun *struct {
			DatabaseID int64  `json:"databaseId"`
			URL        string `json:"url"`
			Workflow   *struct {
				Name string `json:"name"`
			} `json:"workflow"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	Context   string `json:"context"`
	State     string `json:"state"`
	TargetURL string `json:"targetUrl"`
	CreatedAt string `json:"createdAt"`
}

type githubContextsRaw struct {
	PageInfo githubPageInfo     `json:"pageInfo"`
	Nodes    []githubContextRaw `json:"nodes"`
}

type githubRollupRaw struct {
	Contexts githubContextsRaw `json:"contexts"`
}

// githubReviewRaw is one latestReviews node.
type githubReviewRaw struct {
	Body        string         `json:"body"`
	SubmittedAt string         `json:"submittedAt"`
	State       string         `json:"state"`
	Author      githubActorRaw `json:"author"`
	Commit      struct {
		OID string `json:"oid"`
	} `json:"commit"`
}

type githubPullRaw struct {
	Number           int            `json:"number"`
	Title            string         `json:"title"`
	Body             string         `json:"body"`
	State            string         `json:"state"`
	IsDraft          bool           `json:"isDraft"`
	BaseRefName      string         `json:"baseRefName"`
	HeadRefName      string         `json:"headRefName"`
	HeadRefOID       string         `json:"headRefOid"`
	URL              string         `json:"url"`
	Additions        int            `json:"additions"`
	Deletions        int            `json:"deletions"`
	ChangedFiles     int            `json:"changedFiles"`
	Mergeable        string         `json:"mergeable"`
	MergeStateStatus string         `json:"mergeStateStatus"`
	ReviewDecision   string         `json:"reviewDecision"`
	Author           githubActorRaw `json:"author"`
	LatestReviews    struct {
		Nodes []githubReviewRaw `json:"nodes"`
	} `json:"latestReviews"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				ID                string           `json:"id"`
				StatusCheckRollup *githubRollupRaw `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	ReviewThreads githubThreadsRaw  `json:"reviewThreads"`
	Comments      githubCommentsRaw `json:"comments"`
}

// githubTickRaw is PRTick's data.
type githubTickRaw struct {
	Viewer *struct {
		Login string `json:"login"`
	} `json:"viewer"`
	Repository *struct {
		PullRequest *githubPullRaw `json:"pullRequest"`
	} `json:"repository"`
}

// pull returns the tick's pull request, or an error when the answer has
// none (GitHub reports a missing one as a GraphQL error first).
func (t githubTickRaw) pull(ref PRReference) (*githubPullRaw, error) {
	if t.Repository == nil || t.Repository.PullRequest == nil {
		return nil, fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	return t.Repository.PullRequest, nil
}

// githubPullVars are the variables naming a pull request.
func githubPullVars(ref PRReference) (map[string]any, error) {
	owner, repo, err := splitGitHubProject(ref.Project())
	if err != nil {
		return nil, err
	}
	return map[string]any{"owner": owner, "name": repo, "number": ref.Number}, nil
}

// ReadPR answers the parts want names from one PRTick request, plus a
// page request for each connection past its first page and, for CI, the
// REST jobs lists the steps of stepsFor jobs need.
func (f *githubForge) ReadPR(ctx context.Context, ref PRReference, want PRReadParts, prev *CIPipeline, stepsFor []string) (PRRead, error) {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return PRRead{}, err
	}
	vars, err := githubPullVars(ref)
	if err != nil {
		return PRRead{}, err
	}
	vars["wantDetail"] = want.Detail
	vars["wantThreads"] = want.Threads
	vars["wantChecks"] = want.Detail || want.CI
	var raw githubTickRaw
	if _, err := client.GraphQL(ctx, "PRTick", githubPRTickQuery, vars, &raw); err != nil {
		return PRRead{}, err
	}
	pull, err := raw.pull(ref)
	if err != nil {
		return PRRead{}, err
	}
	var checks []CheckStatus
	if want.Detail || want.CI {
		contexts, err := f.readRollupContexts(ctx, client, pull)
		if err != nil {
			return PRRead{}, err
		}
		checks = githubCheckStatuses(contexts)
	}
	var out PRRead
	if want.Detail {
		viewer := ""
		if raw.Viewer != nil {
			viewer = raw.Viewer.Login
		}
		out.Detail = githubPRDetail(pull, viewer, checks)
	}
	if want.Threads {
		if out.Threads, err = f.readThreads(ctx, client, ref, pull); err != nil {
			return PRRead{}, err
		}
	}
	if want.CI {
		if out.CI, err = f.githubPipeline(ctx, client, ref, checks, prev, stepsFor); err != nil {
			return PRRead{}, err
		}
		out.HasCI = true
	}
	return out, nil
}

// readRollupContexts returns the head commit's check contexts, paging
// past the first page. A PR whose head has no checks answers none.
func (f *githubForge) readRollupContexts(ctx context.Context, client *forgeapi.Client, pull *githubPullRaw) ([]githubContextRaw, error) {
	if len(pull.Commits.Nodes) == 0 || pull.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		return nil, nil
	}
	commit := pull.Commits.Nodes[0].Commit
	contexts := commit.StatusCheckRollup.Contexts
	nodes := contexts.Nodes
	page := contexts.PageInfo
	for pages := 1; page.HasNextPage && pages < githubReadMaxPages; pages++ {
		if commit.ID == "" || page.EndCursor == "" {
			return nil, errors.New("GitHub check rollup has another page but no commit id or cursor to read it by")
		}
		var raw struct {
			Node *struct {
				StatusCheckRollup *githubRollupRaw `json:"statusCheckRollup"`
			} `json:"node"`
		}
		if _, err := client.GraphQL(ctx, "RollupContextsPage", githubRollupContextsPageQuery, map[string]any{"commitID": commit.ID, "after": page.EndCursor}, &raw); err != nil {
			return nil, err
		}
		if raw.Node == nil || raw.Node.StatusCheckRollup == nil {
			return nil, fmt.Errorf("GitHub answered no check rollup for commit %s", commit.ID)
		}
		nodes = append(nodes, raw.Node.StatusCheckRollup.Contexts.Nodes...)
		page = raw.Node.StatusCheckRollup.Contexts.PageInfo
	}
	return nodes, nil
}

// readThreads returns the review threads, each with all its comments, then
// the conversation comments as path-less single-comment threads, paging
// every connection past its first page. GitHub keeps PR conversation
// comments (issue comments) flat, outside review threads; the review pane
// lists them with the threads.
func (f *githubForge) readThreads(ctx context.Context, client *forgeapi.Client, ref PRReference, pull *githubPullRaw) ([]ReviewThread, error) {
	vars, err := githubPullVars(ref)
	if err != nil {
		return nil, err
	}
	threads := pull.ReviewThreads.Nodes
	page := pull.ReviewThreads.PageInfo
	for pages := 1; page.HasNextPage && pages < githubReadMaxPages; pages++ {
		var raw githubTickRaw
		if _, err := client.GraphQL(ctx, "PRThreadsPage", githubPRThreadsPageQuery, withAfter(vars, page.EndCursor), &raw); err != nil {
			return nil, err
		}
		next, err := raw.pull(ref)
		if err != nil {
			return nil, err
		}
		threads = append(threads, next.ReviewThreads.Nodes...)
		page = next.ReviewThreads.PageInfo
	}
	for i := range threads {
		if err := f.readThreadComments(ctx, client, &threads[i]); err != nil {
			return nil, err
		}
	}

	comments := pull.Comments.Nodes
	page = pull.Comments.PageInfo
	for pages := 1; page.HasNextPage && pages < githubReadMaxPages; pages++ {
		var raw githubTickRaw
		if _, err := client.GraphQL(ctx, "PRCommentsPage", githubPRCommentsPageQuery, withAfter(vars, page.EndCursor), &raw); err != nil {
			return nil, err
		}
		next, err := raw.pull(ref)
		if err != nil {
			return nil, err
		}
		comments = append(comments, next.Comments.Nodes...)
		page = next.Comments.PageInfo
	}
	out := githubReviewThreads(threads)
	return append(out, githubConversationThreads(comments)...), nil
}

// readThreadComments completes one thread's comments past their first
// page.
func (f *githubForge) readThreadComments(ctx context.Context, client *forgeapi.Client, thread *githubThreadRaw) error {
	page := thread.Comments.PageInfo
	for pages := 1; page.HasNextPage && pages < githubReadMaxPages; pages++ {
		if page.EndCursor == "" {
			return fmt.Errorf("GitHub review thread %s has another page of comments but no cursor", thread.ID)
		}
		var raw struct {
			Node *struct {
				Comments *githubThreadCommentsRaw `json:"comments"`
			} `json:"node"`
		}
		if _, err := client.GraphQL(ctx, "ThreadCommentsPage", githubThreadCommentsPageQuery, map[string]any{"threadID": thread.ID, "after": page.EndCursor}, &raw); err != nil {
			return err
		}
		if raw.Node == nil || raw.Node.Comments == nil {
			return fmt.Errorf("GitHub answered no review thread %s", thread.ID)
		}
		thread.Comments.Nodes = append(thread.Comments.Nodes, raw.Node.Comments.Nodes...)
		page = raw.Node.Comments.PageInfo
	}
	return nil
}

// withAfter copies vars with the page cursor set.
func withAfter(vars map[string]any, after string) map[string]any {
	out := make(map[string]any, len(vars)+1)
	for k, v := range vars {
		out[k] = v
	}
	out["after"] = after
	return out
}

func githubPRDetail(pull *githubPullRaw, viewer string, checks []CheckStatus) PRDetail {
	return PRDetail{
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		AuthorLogin:    pull.Author.Login,
		AuthorName:     pull.Author.Name,
		State:          NormalizePRState(pull.State),
		Draft:          pull.IsDraft,
		HeadRefName:    pull.HeadRefName,
		BaseRefName:    pull.BaseRefName,
		HeadSHA:        pull.HeadRefOID,
		URL:            pull.URL,
		Additions:      pull.Additions,
		Deletions:      pull.Deletions,
		ChangedFiles:   pull.ChangedFiles,
		ViewerIsAuthor: viewer != "" && strings.EqualFold(viewer, pull.Author.Login),
		ReviewDecision: pull.ReviewDecision,
		LatestReviews:  githubLatestReviews(pull.LatestReviews.Nodes),
		Checks:         githubCheckSummary(checks),
		Mergeability:   normalizeGitHubMergeability(pull.Mergeable, pull.MergeStateStatus),
	}
}

// githubLatestReviews maps latestReviews, GitHub's own latest review per
// author, skipping an entry with no author or state.
func githubLatestReviews(reviews []githubReviewRaw) []ReviewVerdict {
	out := make([]ReviewVerdict, 0, len(reviews))
	for _, review := range reviews {
		if review.Author.Login == "" || review.State == "" {
			continue
		}
		out = append(out, ReviewVerdict{
			AuthorLogin: review.Author.Login,
			AuthorName:  review.Author.Name,
			State:       review.State,
			SubmittedAt: review.SubmittedAt,
			Body:        review.Body,
			CommitSHA:   review.Commit.OID,
		})
	}
	return out
}

// githubCheckStatuses maps rollup contexts to checks. A CheckRun's
// workflow is its check suite's workflow run's; a StatusContext's start
// is its creation. A context of another type is skipped.
func githubCheckStatuses(contexts []githubContextRaw) []CheckStatus {
	out := make([]CheckStatus, 0, len(contexts))
	for _, item := range contexts {
		check := CheckStatus{Kind: item.Typename}
		switch item.Typename {
		case "CheckRun":
			check.Name = item.Name
			if suite := item.CheckSuite; suite != nil && suite.WorkflowRun != nil && suite.WorkflowRun.Workflow != nil {
				check.Workflow = suite.WorkflowRun.Workflow.Name
			}
			check.Status = item.Status
			check.Conclusion = item.Conclusion
			check.DetailsURL = item.DetailsURL
			check.StartedAt = item.StartedAt
			check.CompletedAt = item.CompletedAt
		case "StatusContext":
			check.Name = item.Context
			check.Status = item.State
			check.DetailsURL = item.TargetURL
			check.StartedAt = item.CreatedAt
		default:
			continue
		}
		out = append(out, check)
	}
	return out
}

func githubCheckSummary(checks []CheckStatus) CheckSummary {
	summary := CheckSummary{Checks: checks}
	for _, check := range checks {
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

func githubReviewThreads(nodes []githubThreadRaw) []ReviewThread {
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
				AuthorName:  comment.Author.Name,
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
	return threads
}

// githubConversationThreads maps conversation comments to path-less
// single-comment threads, skipping minimized ones.
func githubConversationThreads(nodes []githubCommentRaw) []ReviewThread {
	threads := make([]ReviewThread, 0, len(nodes))
	for _, node := range nodes {
		if node.IsMinimized {
			continue
		}
		threads = append(threads, ReviewThread{
			ID: node.ID,
			Comments: []ReviewComment{{
				AuthorLogin: node.Author.Login,
				AuthorName:  node.Author.Name,
				Body:        node.Body,
				CreatedAt:   node.CreatedAt,
				DatabaseID:  node.DatabaseID,
			}},
		})
	}
	return threads
}
