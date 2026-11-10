package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PRListRow is one open PR/MR of a project as the project read answers
// it (docs/architecture/forge-transport.md#project-lists): what a list
// row and its attention state need, read with a fixed set of requests per
// project and never one per row. A field the forge does not carry is
// zero, as each field says; a field a detail request fills is nil or ""
// when that request failed or does not cover the row.
type PRListRow struct {
	Ref PRReference `json:"ref"`
	// NodeID is the GitHub GraphQL node id (ReadPRStates addresses a PR
	// by it); empty on GitLab.
	NodeID string       `json:"nodeID,omitempty"`
	URL    string       `json:"url"`
	Title  string       `json:"title"`
	Draft  bool         `json:"draft"`
	Author PRListAuthor `json:"author"`
	// CreatedAt and UpdatedAt are RFC 3339, as the forge spells them.
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	HeadSHA     string `json:"headSHA"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	// CrossRepository is a head in another repository (a fork).
	CrossRepository bool `json:"crossRepository"`
	// Comments counts the human comments: GitHub totalCommentsCount
	// (conversation and review comments), GitLab user_notes_count.
	Comments int `json:"comments"`
	// Commits is GitHub's commit count, 0 on GitLab. LastCommitAt (RFC
	// 3339) is the head commit's time: GitHub's list carries it, GitLab's
	// activity request fills it for the rows it covers.
	Commits      int    `json:"commits"`
	LastCommitAt string `json:"lastCommitAt"`
	// ViewerIsAuthor is the login opened the PR. ViewerIsReviewer is a
	// review requested from the login, or (GitHub) an approval or change
	// request from it standing. ReviewRequested is a review requested from
	// the login: GitLab's list does not say whether a reviewer has
	// reviewed, so there it is ViewerIsReviewer. A viewer the forge could
	// not name reads false on all three.
	ViewerIsAuthor   bool `json:"viewerIsAuthor"`
	ViewerIsReviewer bool `json:"viewerIsReviewer"`
	ReviewRequested  bool `json:"reviewRequested"`
	// ViewerReview is the login's latest review (GitHub); zero on GitLab
	// and when the login has not reviewed.
	ViewerReview PRListReview `json:"viewerReview"`
	// ViewerReviewState is the login's own review in GitLab's reviewer
	// vocabulary (unreviewed, review_started, reviewed, approved,
	// requested_changes, unapproved). GitLab: the reviewer request's
	// reviewState, "" when the login is not a reviewer or the request
	// failed. GitHub: unreviewed while a review is requested from the
	// login, else its standing opinion (approved, requested_changes) or
	// reviewed for a latest review without one, "" when it never reviewed.
	ViewerReviewState string `json:"viewerReviewState,omitempty"`
	// ViewerActivityAt (RFC 3339) is the login's latest activity on the
	// PR: GitHub its latest review; GitLab its latest comment or approval
	// in the window of its events read (at most the last
	// gitlabActivityDays), "" when there is none or the events could not
	// be read.
	ViewerActivityAt string `json:"viewerActivityAt,omitempty"`
	// AuthorActivityAt (RFC 3339) is the author's latest conversation
	// comment or submitted review, a review thread reply included, among
	// the PR's last githubListActivityDepth of each (GitHub); "" on GitLab
	// and when none of those is the author's.
	AuthorActivityAt string `json:"authorActivityAt,omitempty"`
	// ReviewDecision is GitHub's reviewDecision (APPROVED,
	// CHANGES_REQUESTED, REVIEW_REQUIRED or ""); GitLab's is
	// REVIEW_REQUIRED for not_approved and CHANGES_REQUESTED for
	// requested_changes, else "".
	ReviewDecision string `json:"reviewDecision"`
	// MergeStatus is GitHub's mergeStateStatus lowercased or GitLab's
	// detailed_merge_status, as each forge words it.
	MergeStatus string `json:"mergeStatus"`
	Conflicts   bool   `json:"conflicts"`
	// UnresolvedBlocking is GitLab's !blocking_discussions_resolved: a
	// thread the project requires resolved is open. Always false on
	// GitHub, whose list does not say.
	UnresolvedBlocking bool `json:"unresolvedBlocking"`
	// ChecksState is GitHub's head commit rollup: "success", "failure",
	// "pending" or "" with no checks. GitLab's list carries no pipeline,
	// so it is "" there.
	ChecksState string `json:"checksState"`

	// The detail fields: the app projects them onto its wire row
	// (approvals, unresolved, CI), so they are not encoded here.
	// AuthorReplies counts the author's comments and reviews after
	// ViewerActivityAt among the last githubListActivityDepth of each
	// (GitHub; 0 on GitLab). ApprovalsGiven and ApprovalsRequired are the
	// approvals standing and the approvals the base or project requires;
	// Unresolved the resolvable threads not resolved (GitHub: nil for a
	// PR with more than 100 threads); PipelineStatus the head pipeline's
	// normalized CI status ("" with none). ProjectID is GitLab's numeric
	// project id, which the viewer's events name. ViewerActivityFloor
	// (GitLab, RFC 3339) is the events window's floor, set when the events
	// were read and ViewerActivityAt is empty or older than it: the
	// login's latest activity on the PR may be any time up to it, since a
	// feed cut at its page cap hides what is older than its oldest event.
	AuthorReplies       int    `json:"-"`
	ApprovalsGiven      *int   `json:"-"`
	ApprovalsRequired   *int   `json:"-"`
	Unresolved          *int   `json:"-"`
	PipelineStatus      string `json:"-"`
	ProjectID           int64  `json:"-"`
	ViewerActivityFloor string `json:"-"`
}

// PRListDetailError is a project read whose list landed while some of
// its detail requests failed. ListProjectPRs returns it beside the rows,
// whose fields those requests fill read as unknown; any other error
// fails the read as a whole and comes with no rows.
type PRListDetailError struct {
	Errs []error
}

func (e *PRListDetailError) Error() string {
	return errors.Join(e.Errs...).Error()
}

func (e *PRListDetailError) Unwrap() []error { return e.Errs }

// detailError is the PRListDetailError of errs, nil when none failed.
func detailError(errs ...error) error {
	var failed []error
	for _, err := range errs {
		if err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return &PRListDetailError{Errs: failed}
}

// PRListAuthor is a PR's author: the login and, when the forge reports
// one, the display name.
type PRListAuthor struct {
	Login string `json:"login"`
	Name  string `json:"name,omitempty"`
}

// PRListReview is the login's latest review of a PR: GitHub's review
// state (APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED) and when it
// was submitted (RFC 3339).
type PRListReview struct {
	State       string `json:"state,omitempty"`
	SubmittedAt string `json:"submittedAt,omitempty"`
}

// prListMaxPages bounds the pages of 100 one project list read reads:
// GitHub search answers at most 1000 results, and GitLab's list stops at
// the same count, most recently updated first.
const prListMaxPages = 10

// ListProjectPRs reads the open PRs/MRs of repo with the forge's fixed
// request set, its requests concurrent
// (docs/architecture/forge-transport.md#project-lists). It fails as a
// whole only when the list fails; a failed detail request answers the
// rows with a *PRListDetailError.
func (c *Core) ListProjectPRs(ctx context.Context, repo RepoReference) ([]PRListRow, error) {
	if err := repo.Validate(); err != nil {
		return nil, err
	}
	return c.ForgeByID(repo.Forge).ListProjectPRs(ctx, repo)
}

func (nullForge) ListProjectPRs(context.Context, RepoReference) ([]PRListRow, error) {
	return nil, ErrUnsupportedForge
}

// githubListActivityDepth bounds the latest conversation comments and
// reviews a list node reads for the author's activity (AuthorActivityAt):
// each connection costs one GraphQL point per page of 100 PRs, whatever
// its depth up to 100.
const githubListActivityDepth = "10"

// githubPullsSearchQuery lists a repository's open pull requests through
// search, the one connection that reads every field a row shows for 100
// PRs in one request, so no row needs a read of its own. The viewer's own
// review comes from viewerLatestReview and its standing opinion from
// latestOpinionatedReviews, since the login is not known before the
// answer; the author's latest replies from the last comments and reviews
// (a reply in a review thread is a review of its own). The required
// approvals come from the base's protection and rulesets as the detail
// read takes them (refUpdateRule, readable without admin, and rules).
const githubPullsSearchQuery = `query PullsSearch($query: String!, $after: String) {
  viewer { login }
  search(type: ISSUE, query: $query, first: 100, after: $after) {
    ` + githubPageInfoFields + `
    nodes {
      ... on PullRequest {
        id number url title isDraft createdAt updatedAt headRefOid headRefName baseRefName isCrossRepository
        ` + githubActor + `
        totalCommentsCount reviewDecision mergeStateStatus mergeable viewerDidAuthor
        reviewRequests(first: 20) { nodes { requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } } } }
        latestOpinionatedReviews(first: 100) { nodes { state viewerDidAuthor } }
        viewerLatestReview { state submittedAt }
        reviewThreads(first: 100) { pageInfo { hasNextPage } nodes { isResolved } }
        baseRef { refUpdateRule { requiredApprovingReviewCount } rules(first: 100) { nodes { type parameters { __typename ... on PullRequestParameters { requiredApprovingReviewCount } } } } }
        commits(last: 1) { totalCount nodes { commit { committedDate statusCheckRollup { state } } } }
        comments(last: ` + githubListActivityDepth + `) { nodes { author { login } createdAt } }
        reviews(last: ` + githubListActivityDepth + `) { nodes { author { login } submittedAt } }
      }
    }
  }
}`

type githubSearchPullRaw struct {
	ID                 string         `json:"id"`
	Number             int            `json:"number"`
	URL                string         `json:"url"`
	Title              string         `json:"title"`
	IsDraft            bool           `json:"isDraft"`
	CreatedAt          string         `json:"createdAt"`
	UpdatedAt          string         `json:"updatedAt"`
	HeadRefOID         string         `json:"headRefOid"`
	HeadRefName        string         `json:"headRefName"`
	BaseRefName        string         `json:"baseRefName"`
	IsCrossRepository  bool           `json:"isCrossRepository"`
	Author             githubActorRaw `json:"author"`
	TotalCommentsCount int            `json:"totalCommentsCount"`
	ReviewDecision     string         `json:"reviewDecision"`
	MergeStateStatus   string         `json:"mergeStateStatus"`
	Mergeable          string         `json:"mergeable"`
	ViewerDidAuthor    bool           `json:"viewerDidAuthor"`
	ReviewRequests     struct {
		Nodes []struct {
			RequestedReviewer *struct {
				Login string `json:"login"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	LatestOpinionatedReviews struct {
		Nodes []struct {
			State           string `json:"state"`
			ViewerDidAuthor bool   `json:"viewerDidAuthor"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	ViewerLatestReview *PRListReview `json:"viewerLatestReview"`
	ReviewThreads      struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []struct {
			IsResolved bool `json:"isResolved"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	BaseRef *githubBaseRefRaw `json:"baseRef"`
	Commits struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Commit struct {
				CommittedDate     string `json:"committedDate"`
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Comments struct {
		Nodes []struct {
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
			CreatedAt string `json:"createdAt"`
		} `json:"nodes"`
	} `json:"comments"`
	Reviews struct {
		Nodes []struct {
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
			SubmittedAt string `json:"submittedAt"`
		} `json:"nodes"`
	} `json:"reviews"`
}

// ListProjectPRs reads the repository's open pull requests through
// search, 100 per request, at most prListMaxPages requests.
func (f *githubForge) ListProjectPRs(ctx context.Context, repo RepoReference) ([]PRListRow, error) {
	owner, name, err := splitGitHubProject(repo.Project())
	if err != nil {
		return nil, err
	}
	client, err := f.core.githubAPI(repo.Host)
	if err != nil {
		return nil, err
	}
	query := "is:pr is:open repo:" + owner + "/" + name
	rows := []PRListRow{}
	var after any
	for pages := 0; pages < prListMaxPages; pages++ {
		var raw struct {
			Viewer *struct {
				Login string `json:"login"`
			} `json:"viewer"`
			Search *struct {
				PageInfo githubPageInfo        `json:"pageInfo"`
				Nodes    []githubSearchPullRaw `json:"nodes"`
			} `json:"search"`
		}
		if _, err := client.GraphQL(ctx, "PullsSearch", githubPullsSearchQuery, map[string]any{"query": query, "after": after}, &raw); err != nil {
			return nil, err
		}
		if raw.Search == nil {
			return nil, fmt.Errorf("GitHub answered no search for %s on %s", repo.Project(), repo.Host)
		}
		viewer := ""
		if raw.Viewer != nil {
			viewer = raw.Viewer.Login
		}
		for _, node := range raw.Search.Nodes {
			if node.Number <= 0 {
				// A search result that is not a pull request.
				continue
			}
			rows = append(rows, githubPRListRow(repo, node, viewer))
		}
		page := raw.Search.PageInfo
		if !page.HasNextPage || page.EndCursor == "" {
			break
		}
		after = page.EndCursor
	}
	return rows, nil
}

func githubPRListRow(repo RepoReference, node githubSearchPullRaw, viewer string) PRListRow {
	row := PRListRow{
		Ref:             PRReference{Forge: repo.Forge, Host: repo.Host, Namespace: repo.Namespace, Repo: repo.Repo, Number: node.Number},
		NodeID:          node.ID,
		URL:             node.URL,
		Title:           node.Title,
		Draft:           node.IsDraft,
		Author:          PRListAuthor{Login: node.Author.Login, Name: node.Author.Name},
		CreatedAt:       node.CreatedAt,
		UpdatedAt:       node.UpdatedAt,
		HeadSHA:         node.HeadRefOID,
		HeadRefName:     node.HeadRefName,
		BaseRefName:     node.BaseRefName,
		CrossRepository: node.IsCrossRepository,
		Comments:        node.TotalCommentsCount,
		Commits:         node.Commits.TotalCount,
		ViewerIsAuthor:  node.ViewerDidAuthor,
		ReviewDecision:  node.ReviewDecision,
		MergeStatus:     strings.ToLower(node.MergeStateStatus),
		Conflicts:       node.Mergeable == "CONFLICTING",
	}
	if node.ViewerLatestReview != nil {
		row.ViewerReview = *node.ViewerLatestReview
	}
	if len(node.Commits.Nodes) > 0 {
		commit := node.Commits.Nodes[0].Commit
		row.LastCommitAt = commit.CommittedDate
		if commit.StatusCheckRollup != nil {
			row.ChecksState = githubRollupState(commit.StatusCheckRollup.State)
		}
	}
	row.PipelineStatus = githubChecksCIStatus(row.ChecksState)
	for _, request := range node.ReviewRequests.Nodes {
		if viewer != "" && request.RequestedReviewer != nil && strings.EqualFold(request.RequestedReviewer.Login, viewer) {
			row.ReviewRequested = true
		}
	}
	row.ViewerIsReviewer = row.ReviewRequested
	given, opinion := 0, ""
	for _, review := range node.LatestOpinionatedReviews.Nodes {
		row.ViewerIsReviewer = row.ViewerIsReviewer || review.ViewerDidAuthor
		if strings.EqualFold(review.State, "APPROVED") {
			given++
		}
		if review.ViewerDidAuthor {
			opinion = strings.ToUpper(review.State)
		}
	}
	required := githubMergeInputs{pull: &githubPullRaw{BaseRef: node.BaseRef}}.baseRules().approvals
	row.ApprovalsGiven, row.ApprovalsRequired = &given, &required
	// Past the first 100 threads the count would be a guess: unknown.
	if !node.ReviewThreads.PageInfo.HasNextPage {
		unresolved := 0
		for _, thread := range node.ReviewThreads.Nodes {
			if !thread.IsResolved {
				unresolved++
			}
		}
		row.Unresolved = &unresolved
	}
	row.ViewerActivityAt = row.ViewerReview.SubmittedAt
	switch {
	case row.ReviewRequested:
		// A review requested again after the login reviewed is its turn.
		row.ViewerReviewState = "unreviewed"
	case opinion == "APPROVED":
		row.ViewerReviewState = "approved"
	case opinion == "CHANGES_REQUESTED":
		row.ViewerReviewState = "requested_changes"
	case row.ViewerReview.SubmittedAt != "":
		row.ViewerReviewState = "reviewed"
	}
	byAuthor := func(author *struct {
		Login string `json:"login"`
	}) bool {
		return author != nil && author.Login != "" && strings.EqualFold(author.Login, node.Author.Login)
	}
	authorActed := func(at string) {
		row.AuthorActivityAt = laterRFC3339(row.AuthorActivityAt, at)
		if row.ViewerActivityAt != "" && rfc3339After(at, row.ViewerActivityAt) {
			row.AuthorReplies++
		}
	}
	for _, comment := range node.Comments.Nodes {
		if byAuthor(comment.Author) {
			authorActed(comment.CreatedAt)
		}
	}
	for _, review := range node.Reviews.Nodes {
		if byAuthor(review.Author) {
			authorActed(review.SubmittedAt)
		}
	}
	return row
}

// githubChecksCIStatus is the rollup state (githubRollupState) in the
// normalized CI vocabulary.
func githubChecksCIStatus(checks string) string {
	switch checks {
	case "success":
		return CIStatusSuccess
	case "failure":
		return CIStatusFailed
	case "pending":
		return CIStatusPending
	}
	return ""
}

// rfc3339After reports whether a is a time after b; an unreadable time is
// not after anything.
func rfc3339After(a, b string) bool {
	at, errA := time.Parse(time.RFC3339, a)
	bt, errB := time.Parse(time.RFC3339, b)
	return errA == nil && errB == nil && at.After(bt)
}

// laterRFC3339 is the later of two RFC 3339 times; an unreadable or empty
// one loses.
func laterRFC3339(a, b string) string {
	at, errA := time.Parse(time.RFC3339, a)
	bt, errB := time.Parse(time.RFC3339, b)
	switch {
	case errB != nil:
		return a
	case errA != nil || bt.After(at):
		return b
	}
	return a
}

// githubRollupState maps a StatusState onto the list's checks vocabulary.
func githubRollupState(state string) string {
	switch state {
	case "SUCCESS":
		return "success"
	case "FAILURE", "ERROR":
		return "failure"
	case "PENDING", "EXPECTED":
		return "pending"
	default:
		return ""
	}
}
