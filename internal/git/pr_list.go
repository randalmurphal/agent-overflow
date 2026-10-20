package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// PRListRow is one open PR/MR of a project as the forge's list read
// answers it: what a list row and its attention state need, without the
// per-PR reads the review pane makes. A field the forge's list does not
// carry is zero, as each field says.
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
	// Commits and LastCommitAt (RFC 3339) are GitHub's; GitLab's list
	// carries neither, so they are 0 and "".
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

// ListProjectPRs reads the open PRs/MRs of repo, one list read per
// project (docs/architecture/forge-transport.md#project-lists).
func (c *Core) ListProjectPRs(ctx context.Context, repo RepoReference) ([]PRListRow, error) {
	if err := repo.Validate(); err != nil {
		return nil, err
	}
	return c.ForgeByID(repo.Forge).ListProjectPRs(ctx, repo)
}

func (nullForge) ListProjectPRs(context.Context, RepoReference) ([]PRListRow, error) {
	return nil, ErrUnsupportedForge
}

// githubPullsSearchQuery lists a repository's open pull requests through
// search, the one connection that reads reviewDecision, review requests
// and the rollup state of every PR in one request. The viewer's own
// review comes from viewerLatestReview and its standing opinion from
// latestOpinionatedReviews, since the login is not known before the
// answer.
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
        latestOpinionatedReviews(first: 100) { nodes { viewerDidAuthor } }
        viewerLatestReview { state submittedAt }
        commits(last: 1) { totalCount nodes { commit { committedDate statusCheckRollup { state } } } }
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
			ViewerDidAuthor bool `json:"viewerDidAuthor"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	ViewerLatestReview *PRListReview `json:"viewerLatestReview"`
	Commits            struct {
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
	for _, request := range node.ReviewRequests.Nodes {
		if viewer != "" && request.RequestedReviewer != nil && strings.EqualFold(request.RequestedReviewer.Login, viewer) {
			row.ReviewRequested = true
		}
	}
	row.ViewerIsReviewer = row.ReviewRequested
	for _, review := range node.LatestOpinionatedReviews.Nodes {
		row.ViewerIsReviewer = row.ViewerIsReviewer || review.ViewerDidAuthor
	}
	return row
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

type gitlabListMRRaw struct {
	IID                         int               `json:"iid"`
	ProjectID                   int64             `json:"project_id"`
	SourceProjectID             int64             `json:"source_project_id"`
	Title                       string            `json:"title"`
	WebURL                      string            `json:"web_url"`
	Draft                       bool              `json:"draft"`
	WorkInProgress              bool              `json:"work_in_progress"`
	Author                      gitlabAuthorRaw   `json:"author"`
	Reviewers                   []gitlabAuthorRaw `json:"reviewers"`
	CreatedAt                   string            `json:"created_at"`
	UpdatedAt                   string            `json:"updated_at"`
	SHA                         string            `json:"sha"`
	SourceBranch                string            `json:"source_branch"`
	TargetBranch                string            `json:"target_branch"`
	UserNotesCount              int               `json:"user_notes_count"`
	DetailedMergeStatus         string            `json:"detailed_merge_status"`
	HasConflicts                bool              `json:"has_conflicts"`
	BlockingDiscussionsResolved bool              `json:"blocking_discussions_resolved"`
}

// ListProjectPRs reads the project's open merge requests, most recently
// updated first, 100 per page, at most prListMaxPages pages. The viewer
// is the login's cached username (viewerLogin).
func (f *gitlabForge) ListProjectPRs(ctx context.Context, repo RepoReference) ([]PRListRow, error) {
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return nil, err
	}
	request := forgeapi.Request{
		Path: gitlabProjectPath(repo.Project()) + "/merge_requests",
		Query: url.Values{
			"state": {"opened"}, "per_page": {strconv.Itoa(gitlabPageSize)},
			"order_by": {"updated_at"}, "sort": {"desc"},
		},
	}
	var raw []gitlabListMRRaw
	page, pages := 1, 0
	err = client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var pageMRs []gitlabListMRRaw
		if err := json.Unmarshal(resp.Body, &pageMRs); err != nil {
			return false, fmt.Errorf("GitLab merge requests of %s: decode response: %w", repo.Project(), err)
		}
		raw = append(raw, pageMRs...)
		pages++
		return len(pageMRs) == gitlabPageSize && pages < prListMaxPages && advanceGitLabPage(resp, &page), nil
	})
	if err != nil {
		return nil, err
	}
	viewer := f.viewerLogin(ctx, client, repo.Host)
	rows := make([]PRListRow, 0, len(raw))
	for _, mr := range raw {
		rows = append(rows, gitlabPRListRow(repo, mr, viewer))
	}
	return rows, nil
}

func gitlabPRListRow(repo RepoReference, mr gitlabListMRRaw, viewer string) PRListRow {
	row := PRListRow{
		Ref:                PRReference{Forge: repo.Forge, Host: repo.Host, Namespace: repo.Namespace, Repo: repo.Repo, Number: mr.IID},
		URL:                mr.WebURL,
		Title:              mr.Title,
		Draft:              mr.Draft || mr.WorkInProgress,
		Author:             PRListAuthor{Login: mr.Author.Username, Name: mr.Author.Name},
		CreatedAt:          mr.CreatedAt,
		UpdatedAt:          mr.UpdatedAt,
		HeadSHA:            mr.SHA,
		HeadRefName:        mr.SourceBranch,
		BaseRefName:        mr.TargetBranch,
		CrossRepository:    mr.SourceProjectID != mr.ProjectID,
		Comments:           mr.UserNotesCount,
		ViewerIsAuthor:     viewer != "" && strings.EqualFold(viewer, mr.Author.Username),
		ReviewDecision:     gitlabListReviewDecision(mr.DetailedMergeStatus),
		MergeStatus:        mr.DetailedMergeStatus,
		Conflicts:          mr.HasConflicts,
		UnresolvedBlocking: !mr.BlockingDiscussionsResolved,
	}
	for _, reviewer := range mr.Reviewers {
		if viewer != "" && strings.EqualFold(viewer, reviewer.Username) {
			row.ViewerIsReviewer = true
		}
	}
	row.ReviewRequested = row.ViewerIsReviewer
	return row
}

// gitlabListReviewDecision reads the review half of
// detailed_merge_status in GitHub's reviewDecision vocabulary.
func gitlabListReviewDecision(status string) string {
	switch status {
	case "not_approved":
		return "REVIEW_REQUIRED"
	case "requested_changes":
		return "CHANGES_REQUESTED"
	default:
		return ""
	}
}
