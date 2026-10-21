package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/forgeapi"
)

// The GitLab project read of the Pull requests page: the open merge
// request list, six GraphQL requests over the merge requests the viewer
// reviews and authored and the viewer's events (once per host per round),
// all concurrent (docs/architecture/forge-transport.md#project-lists).
// The list's basic view carries no pipeline and cannot say how the viewer
// reviewed, so the GraphQL requests fill those fields for the rows they
// cover. The review fields, the discussion and commit fields and the
// approval rules are separate requests because each costs GitLab about
// 1-2 s on 57 rows and side by side they cost the slowest one.

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

// gitlabPullsReviewFields are the approval fields of a merge request
// node: each reviewer's review state, who approved, the head pipeline.
const gitlabPullsReviewFields = `iid reviewers { nodes { username mergeRequestInteraction { reviewState } } } approvedBy { nodes { username } } headPipeline { status }`

// gitlabPullsActivityFields are the discussion and commit fields: the
// resolvable and resolved thread counts and the newest commit's time.
const gitlabPullsActivityFields = `iid resolvableDiscussionsCount resolvedDiscussionsCount commits(first: 1) { nodes { committedDate } }`

// gitlabPullsApprovalFields are the merge request's own approval rules,
// which already apply its target branch and any per-merge-request
// override. approvalsRequired and approvalsLeft would answer the count
// directly but cost GitLab about 2 s more on 57 rows. approvalState is an
// EE field: GitLab CE answers that it does not exist, and has no required
// approvals (gitlabNoApprovalState).
const gitlabPullsApprovalFields = `iid approvalState { rules { type approvalsRequired } }`

// gitlabPullsQuery is one GraphQL read of a project's open merge requests
// the viewer reviews (filter reviewerUsername) or authored
// (authorUsername), 100 per page, selecting fields.
func gitlabPullsQuery(operation, filter, fields string) string {
	return `query ` + operation + `($fullPath: ID!, $viewer: String!, $after: String) {
  project(fullPath: $fullPath) {
    mergeRequests(` + filter + `: $viewer, state: opened, sort: UPDATED_DESC, first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { ` + fields + ` }
    }
  }
}`
}

// The selections of the GraphQL reads.
const (
	gitlabPullsReview = iota
	gitlabPullsActivity
	gitlabPullsApprovals
)

// gitlabPullsReads are the six GraphQL reads, by operation name.
var gitlabPullsReads = []struct {
	operation, query string
	selection        int
}{
	{"PullsReviewing", gitlabPullsQuery("PullsReviewing", "reviewerUsername", gitlabPullsReviewFields), gitlabPullsReview},
	{"PullsAuthored", gitlabPullsQuery("PullsAuthored", "authorUsername", gitlabPullsReviewFields), gitlabPullsReview},
	{"PullsReviewingActivity", gitlabPullsQuery("PullsReviewingActivity", "reviewerUsername", gitlabPullsActivityFields), gitlabPullsActivity},
	{"PullsAuthoredActivity", gitlabPullsQuery("PullsAuthoredActivity", "authorUsername", gitlabPullsActivityFields), gitlabPullsActivity},
	{"PullsReviewingApprovals", gitlabPullsQuery("PullsReviewingApprovals", "reviewerUsername", gitlabPullsApprovalFields), gitlabPullsApprovals},
	{"PullsAuthoredApprovals", gitlabPullsQuery("PullsAuthoredApprovals", "authorUsername", gitlabPullsApprovalFields), gitlabPullsApprovals},
}

// gitlabPullNodeRaw is one merge request node of either selection.
type gitlabPullNodeRaw struct {
	IID       string `json:"iid"`
	Reviewers *struct {
		Nodes []struct {
			Username    string `json:"username"`
			Interaction *struct {
				ReviewState string `json:"reviewState"`
			} `json:"mergeRequestInteraction"`
		} `json:"nodes"`
	} `json:"reviewers"`
	ApprovedBy *struct {
		Nodes []struct {
			Username string `json:"username"`
		} `json:"nodes"`
	} `json:"approvedBy"`
	HeadPipeline *struct {
		Status string `json:"status"`
	} `json:"headPipeline"`
	ResolvableDiscussionsCount *int `json:"resolvableDiscussionsCount"`
	ResolvedDiscussionsCount   *int `json:"resolvedDiscussionsCount"`
	Commits                    *struct {
		Nodes []struct {
			CommittedDate string `json:"committedDate"`
		} `json:"nodes"`
	} `json:"commits"`
	ApprovalState *struct {
		Rules []struct {
			Type              string `json:"type"`
			ApprovalsRequired int    `json:"approvalsRequired"`
		} `json:"rules"`
	} `json:"approvalState"`
}

// gitlabActivityDays is how far back the viewer's events are read, and
// gitlabActivityMaxPages the pages of 100 read of them. A busy viewer's
// 300 newest events can span only days: the window's floor
// (gitlabActivity.floor) says how far back they reach.
const (
	gitlabActivityDays     = 30
	gitlabActivityMaxPages = 3
)

// gitlabMRKey names a merge request across projects, as the events do.
type gitlabMRKey struct {
	project int64
	iid     int
}

// gitlabActivity is what the viewer's events say: the latest comment on
// or approval of each merge request (at), and the window's floor, the
// time before which the read saw nothing. The feed is a lower bound: a
// merge request the viewer reviewed with nothing in the window was
// reviewed before the floor.
type gitlabActivity struct {
	at    map[gitlabMRKey]string
	floor string
}

// ListProjectPRs reads the project's open merge requests, most recently
// updated first, 100 per page, at most prListMaxPages pages, beside the
// detail reads that fill the rows' review, approval, pipeline, thread,
// commit and viewer activity fields. The viewer is the login's cached
// account (readViewer), read first because the GraphQL filters and the
// events name it.
func (f *gitlabForge) ListProjectPRs(ctx context.Context, repo RepoReference) ([]PRListRow, error) {
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return nil, err
	}
	// Every attention state hangs on knowing who the viewer is, so an
	// unknown login fails this project's read instead of listing every
	// merge request as somebody else's.
	viewer, err := f.readViewer(ctx, client, repo.Host)
	if err != nil {
		return nil, fmt.Errorf("gitlab: list %s: %w", repo.Project(), err)
	}
	var (
		wg        sync.WaitGroup
		raw       []gitlabListMRRaw
		listErr   error
		nodes     = make([][]gitlabPullNodeRaw, len(gitlabPullsReads))
		nodeErrs  = make([]error, len(gitlabPullsReads))
		activity  gitlabActivity
		eventsErr error
	)
	wg.Go(func() { raw, listErr = readGitLabMRList(ctx, client, repo) })
	_, noApprovals := f.noApprovalState.get(repo.Host, f.core.nowFn())
	for i, read := range gitlabPullsReads {
		if read.selection == gitlabPullsApprovals && noApprovals {
			continue
		}
		wg.Go(func() {
			nodes[i], nodeErrs[i] = readGitLabPulls(ctx, client, repo, viewer.Username, read.operation, read.query)
		})
	}
	wg.Go(func() { activity, eventsErr = f.viewerActivity(ctx, client, repo.Host, viewer) })
	wg.Wait()
	if listErr != nil {
		return nil, listErr
	}
	for i, read := range gitlabPullsReads {
		if read.selection == gitlabPullsApprovals && gitlabNoApprovalState(nodeErrs[i]) {
			nodeErrs[i], noApprovals = nil, true
			f.noApprovalState.put(repo.Host, true, f.core.nowFn())
		}
	}
	rows := make([]PRListRow, 0, len(raw))
	byIID := make(map[int]int, len(raw))
	for _, mr := range raw {
		row := gitlabPRListRow(repo, mr, viewer.Username)
		row.ViewerActivityAt = activity.at[gitlabMRKey{project: mr.ProjectID, iid: mr.IID}]
		if row.ViewerActivityAt == "" {
			row.ViewerActivityFloor = activity.floor
		}
		byIID[mr.IID] = len(rows)
		rows = append(rows, row)
	}
	for i, read := range gitlabPullsReads {
		for _, node := range nodes[i] {
			iid, err := strconv.Atoi(node.IID)
			at, ok := byIID[iid]
			if err != nil || !ok {
				// Opened or closed between the list and this read.
				continue
			}
			switch read.selection {
			case gitlabPullsReview:
				gitlabFillReview(&rows[at], node, viewer.Username)
			case gitlabPullsActivity:
				gitlabFillActivity(&rows[at], node)
			case gitlabPullsApprovals:
				gitlabFillApprovals(&rows[at], node)
			}
		}
	}
	if noApprovals {
		// No approval rules: a merge request a review read covers needs
		// none.
		for i := range rows {
			if rows[i].ApprovalsGiven != nil && rows[i].ApprovalsRequired == nil {
				none := 0
				rows[i].ApprovalsRequired = &none
			}
		}
	}
	return rows, detailError(append(nodeErrs, eventsErr)...)
}

// gitlabNoApprovalState reports whether err is GitLab CE's answer to a
// selection of approvalState: the field does not exist on its schema.
func gitlabNoApprovalState(err error) bool {
	var gqlErr *forgeapi.GraphQLError
	if !errors.As(err, &gqlErr) {
		return false
	}
	for _, problem := range gqlErr.Problems {
		if strings.Contains(problem.Message, "Field 'approvalState' doesn't exist") {
			return true
		}
	}
	return false
}

// readGitLabMRList is the open merge request list.
func readGitLabMRList(ctx context.Context, client *forgeapi.Client, repo RepoReference) ([]gitlabListMRRaw, error) {
	request := forgeapi.Request{
		Path: gitlabProjectPath(repo.Project()) + "/merge_requests",
		Query: url.Values{
			"state": {"opened"}, "per_page": {strconv.Itoa(gitlabPageSize)},
			"order_by": {"updated_at"}, "sort": {"desc"},
		},
	}
	var raw []gitlabListMRRaw
	page, pages := 1, 0
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
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
	return raw, nil
}

// readGitLabPulls reads one GraphQL selection's pages, at most
// prListMaxPages.
func readGitLabPulls(ctx context.Context, client *forgeapi.Client, repo RepoReference, viewer, operation, query string) ([]gitlabPullNodeRaw, error) {
	var out []gitlabPullNodeRaw
	var after any
	for range prListMaxPages {
		var raw struct {
			Project *struct {
				MergeRequests *struct {
					PageInfo githubPageInfo      `json:"pageInfo"`
					Nodes    []gitlabPullNodeRaw `json:"nodes"`
				} `json:"mergeRequests"`
			} `json:"project"`
		}
		vars := map[string]any{"fullPath": repo.Project(), "viewer": viewer, "after": after}
		if _, err := client.GraphQL(ctx, operation, query, vars, &raw); err != nil {
			return nil, err
		}
		if raw.Project == nil || raw.Project.MergeRequests == nil {
			return nil, fmt.Errorf("GitLab answered no merge requests for %s on %s (%s)", repo.Project(), repo.Host, operation)
		}
		out = append(out, raw.Project.MergeRequests.Nodes...)
		page := raw.Project.MergeRequests.PageInfo
		if !page.HasNextPage || page.EndCursor == "" {
			break
		}
		after = page.EndCursor
	}
	return out, nil
}

// gitlabFillReview fills a row from an approval selection node: the
// viewer's review state among the reviewers, the approvals standing and
// the head pipeline's normalized status ("" with none).
func gitlabFillReview(row *PRListRow, node gitlabPullNodeRaw, viewer string) {
	if node.Reviewers != nil {
		for _, reviewer := range node.Reviewers.Nodes {
			if reviewer.Interaction != nil && strings.EqualFold(reviewer.Username, viewer) {
				row.ViewerReviewState = strings.ToLower(reviewer.Interaction.ReviewState)
			}
		}
	}
	if node.ApprovedBy != nil {
		given := len(node.ApprovedBy.Nodes)
		row.ApprovalsGiven = &given
	}
	row.PipelineStatus = ""
	if node.HeadPipeline != nil {
		row.PipelineStatus = NormalizeCIStatus(node.HeadPipeline.Status, "")
	}
}

// gitlabFillActivity fills a row from an activity selection node: the
// unresolved thread count and the newest commit's time.
func gitlabFillActivity(row *PRListRow, node gitlabPullNodeRaw) {
	if node.ResolvableDiscussionsCount != nil && node.ResolvedDiscussionsCount != nil {
		unresolved := max(*node.ResolvableDiscussionsCount-*node.ResolvedDiscussionsCount, 0)
		row.Unresolved = &unresolved
	}
	if node.Commits != nil && len(node.Commits.Nodes) > 0 {
		row.LastCommitAt = node.Commits.Nodes[0].CommittedDate
	}
}

// gitlabFillApprovals fills the approvals a merge request requires from
// its approval rules: the rules other than the any-approver rule, summed,
// or the any-approver rule's count when it asks for more, since an
// approval from a rule's member also counts toward any approver. Checked
// on gitlab.com against approvalsRequired for 56 merge requests with
// any-approver, regular and code owner rules.
func gitlabFillApprovals(row *PRListRow, node gitlabPullNodeRaw) {
	if node.ApprovalState == nil {
		return
	}
	anyApprover, rules := 0, 0
	for _, rule := range node.ApprovalState.Rules {
		if rule.Type == "ANY_APPROVER" {
			anyApprover += max(rule.ApprovalsRequired, 0)
		} else {
			rules += max(rule.ApprovalsRequired, 0)
		}
	}
	required := max(anyApprover, rules)
	row.ApprovalsRequired = &required
}

// gitlabEventRaw is one entry of the viewer's events feed.
type gitlabEventRaw struct {
	ProjectID  int64  `json:"project_id"`
	ActionName string `json:"action_name"`
	TargetType string `json:"target_type"`
	TargetIID  *int   `json:"target_iid"`
	CreatedAt  string `json:"created_at"`
	Note       *struct {
		NoteableType string `json:"noteable_type"`
		NoteableIID  *int   `json:"noteable_iid"`
	} `json:"note"`
}

// readGitLabViewerActivity reads the viewer's events of the last
// gitlabActivityDays, at most gitlabActivityMaxPages pages of 100, and
// keeps its latest comment on (a note, diff note or discussion note) or
// approval of each merge request. The floor is the start of the day
// after the after date when the feed ended within the pages (GitLab
// answers events created after that date), else the oldest event read.
func readGitLabViewerActivity(ctx context.Context, client *forgeapi.Client, viewer gitlabViewer, now time.Time) (gitlabActivity, error) {
	if viewer.ID <= 0 {
		return gitlabActivity{}, fmt.Errorf("gitlab: the login %q has no account id to read its events by", viewer.Username)
	}
	after := now.UTC().AddDate(0, 0, -gitlabActivityDays)
	request := forgeapi.Request{
		Path: "users/" + strconv.FormatInt(viewer.ID, 10) + "/events",
		Query: url.Values{
			"per_page": {strconv.Itoa(gitlabPageSize)},
			"after":    {after.Format(time.DateOnly)},
		},
	}
	out := gitlabActivity{at: map[gitlabMRKey]string{}}
	oldest, ended := "", false
	page, pages := 1, 0
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var events []gitlabEventRaw
		if err := json.Unmarshal(resp.Body, &events); err != nil {
			return false, fmt.Errorf("GitLab events of %s: decode response: %w", viewer.Username, err)
		}
		for _, event := range events {
			oldest = earlierRFC3339(oldest, event.CreatedAt)
			iid := 0
			switch {
			case event.ActionName == "commented on" && event.Note != nil && event.Note.NoteableType == "MergeRequest" && event.Note.NoteableIID != nil:
				iid = *event.Note.NoteableIID
			case event.ActionName == "approved" && event.TargetType == "MergeRequest" && event.TargetIID != nil:
				iid = *event.TargetIID
			default:
				continue
			}
			key := gitlabMRKey{project: event.ProjectID, iid: iid}
			out.at[key] = laterRFC3339(out.at[key], event.CreatedAt)
		}
		pages++
		more := len(events) == gitlabPageSize && advanceGitLabPage(resp, &page)
		ended = !more
		return more && pages < gitlabActivityMaxPages, nil
	})
	if err != nil {
		return gitlabActivity{}, err
	}
	out.floor = oldest
	if ended || oldest == "" {
		day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, time.UTC)
		out.floor = day.AddDate(0, 0, 1).Format(time.RFC3339)
	}
	return out, nil
}

// earlierRFC3339 is the earlier of two RFC 3339 times; an unreadable or
// empty one loses to the other.
func earlierRFC3339(a, b string) string {
	at, errA := time.Parse(time.RFC3339, a)
	bt, errB := time.Parse(time.RFC3339, b)
	switch {
	case errB != nil:
		return a
	case errA != nil || bt.Before(at):
		return b
	}
	return a
}

// prListRoundKey carries a list round in a context (WithPRListRound).
type prListRoundKey struct{}

var prListRounds atomic.Uint64

// WithPRListRound marks ctx as one round of project reads, such as one
// tick of the Pull requests page: the reads made under it share what is
// read once per host (GitLab's viewer events), and the next round reads
// it again. Reads under no round read it each time.
func WithPRListRound(ctx context.Context) context.Context {
	return context.WithValue(ctx, prListRoundKey{}, prListRounds.Add(1))
}

func prListRound(ctx context.Context) uint64 {
	round, _ := ctx.Value(prListRoundKey{}).(uint64)
	return round
}

// gitlabActivityRounds holds each host's viewer activity for the round
// that read it: a read of the same round joins it (in flight or done),
// a later round replaces it. One entry per host.
type gitlabActivityRounds struct {
	mu    sync.Mutex
	hosts map[string]*gitlabActivityRead
}

type gitlabActivityRead struct {
	round    uint64
	viewer   gitlabViewer
	done     chan struct{}
	activity gitlabActivity
	err      error
}

// viewerActivity is the viewer's activity on host for ctx's round.
func (f *gitlabForge) viewerActivity(ctx context.Context, client *forgeapi.Client, host string, viewer gitlabViewer) (gitlabActivity, error) {
	round := prListRound(ctx)
	read := func() (gitlabActivity, error) {
		return readGitLabViewerActivity(ctx, client, viewer, f.core.nowFn())
	}
	if round == 0 {
		return read()
	}
	rounds := &f.activity
	rounds.mu.Lock()
	entry := rounds.hosts[host]
	if entry != nil && entry.round == round && entry.viewer == viewer {
		rounds.mu.Unlock()
		select {
		case <-entry.done:
			return entry.activity, entry.err
		case <-ctx.Done():
			return gitlabActivity{}, ctx.Err()
		}
	}
	entry = &gitlabActivityRead{round: round, viewer: viewer, done: make(chan struct{})}
	if rounds.hosts == nil {
		rounds.hosts = map[string]*gitlabActivityRead{}
	}
	rounds.hosts[host] = entry
	rounds.mu.Unlock()
	entry.activity, entry.err = read()
	close(entry.done)
	return entry.activity, entry.err
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
		ProjectID:          mr.ProjectID,
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
