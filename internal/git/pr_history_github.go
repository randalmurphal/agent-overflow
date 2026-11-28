package git

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
)

// A GitHub repository's history is one stream: the pull request list by
// update time, 50 a page, each with its acts:
//
//   - its COMMENTED and CHANGES_REQUESTED reviews, newest first, each a
//     comment;
//   - its APPROVED reviews, oldest first, each an approval, and a comment
//     too, at the same submit time, when it carries line comments (their
//     totalCount);
//   - its conversation comments, newest first.
//
// A dismissed review counts for nothing, a pending one is not submitted,
// and an author that is not a User (a bot) or none (a deleted account)
// is no one. A line comment outside an approval belongs to a COMMENTED
// review of its writer (a reply is one of its own), so the reviews name
// everyone who commented on the code; and a review or a reply moves the
// pull request's updatedAt, so the list reads it again. GitHub documents
// neither (forge-transport.md#pull-request-history).
//
// A pull request the list finds with the update time the store holds is
// unchanged: its page's acts are added and it costs no follow-up page.
// One that changed, or that the store does not hold, reads on whatever
// its page could not hold: newest first down to a little before the read
// that last saw it, or to the start; its approvals to their end. Those
// follow-up pages are stretches in the walk (githubMoreWalk), read with
// the read's requests and moved only as their pages are read.

// githubHistoryPageSize is the pull requests a history page holds. A page
// costs 1 + 50 + 50 + 50 + 50 * 3 connection requests, 3 points (GitHub's
// GraphQL rate limit divides by 100), and a follow-up page 1. Its answer
// is bounded by githubHistoryMaxBody, the transport's ceiling: 8 MiB holds
// 50 pull requests of 167 KB, 103 nested items of 1.6 KB each beside the
// pull request's own fields. Each item carries a time, a login, a display
// name and, for an approval, a count.
const (
	githubHistoryPageSize = 50
	githubHistoryMaxBody  = 8 << 20
)

// githubHistorySeenMargin is how far before the read that last saw a pull
// request its newest-first follow-up reads, for a forge clock behind this
// computer's.
const githubHistorySeenMargin = 5 * time.Minute

const githubHistoryActor = `author { __typename login ... on User { name } }`

const (
	githubHistoryBack      = `pageInfo { hasPreviousPage startCursor }`
	githubHistoryOn        = `pageInfo { hasNextPage endCursor }`
	githubHistoryReviews   = `nodes { submittedAt ` + githubHistoryActor + ` }`
	githubHistoryApprovals = `nodes { submittedAt ` + githubHistoryActor + ` comments(first: 1) { totalCount } }`
	githubHistoryComments  = `nodes { createdAt ` + githubHistoryActor + ` }`
)

const githubHistoryPullFields = `number url title state isDraft headRefName headRepository { id } createdAt updatedAt mergedAt closedAt ` + githubHistoryActor

// githubPullHistoryQuery is one page of a repository's pull requests,
// most recently updated first, each with its newest 50 commenting
// reviews, its first 3 approvals and its newest 50 conversation
// comments, beside the repository's node id and parent and the login of
// the account that asks.
var githubPullHistoryQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PullHistory($owner: String!, $name: String!, $after: String) {
  viewer { login }
  repository(owner: $owner, name: $name) {
    id parent { owner { login } name }
    pullRequests(first: 50, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        `+githubHistoryPullFields+`
        reviews(last: 50, states: [COMMENTED, CHANGES_REQUESTED]) { `+githubHistoryBack+` `+githubHistoryReviews+` }
        approvals: reviews(first: 3, states: [APPROVED]) { `+githubHistoryOn+` `+githubHistoryApprovals+` }
        comments(last: 50) { `+githubHistoryBack+` `+githubHistoryComments+` }
      }
    }
  }
}`).WithMaxBody(githubHistoryMaxBody)

// githubPullHistoryMoreQuery is one follow-up page of a pull request's
// acts, 100 of the kind its want variable names, from its cursor.
var githubPullHistoryMoreQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PullHistoryMore($owner: String!, $name: String!, $number: Int!, $wantReviews: Boolean!, $reviewsBefore: String, $wantApprovals: Boolean!, $approvalsAfter: String, $wantComments: Boolean!, $commentsBefore: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(last: 100, before: $reviewsBefore, states: [COMMENTED, CHANGES_REQUESTED]) @include(if: $wantReviews) { `+githubHistoryBack+` `+githubHistoryReviews+` }
      approvals: reviews(first: 100, after: $approvalsAfter, states: [APPROVED]) @include(if: $wantApprovals) { `+githubHistoryOn+` `+githubHistoryApprovals+` }
      comments(last: 100, before: $commentsBefore) @include(if: $wantComments) { `+githubHistoryBack+` `+githubHistoryComments+` }
    }
  }
}`).WithMaxBody(githubHistoryMaxBody)

type githubHistoryActorRaw struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
	Name     string `json:"name"`
}

type githubHistoryActsRaw struct {
	PageInfo struct {
		HasNextPage     bool   `json:"hasNextPage"`
		EndCursor       string `json:"endCursor"`
		HasPreviousPage bool   `json:"hasPreviousPage"`
		StartCursor     string `json:"startCursor"`
	} `json:"pageInfo"`
	Nodes []struct {
		SubmittedAt string                 `json:"submittedAt"`
		CreatedAt   string                 `json:"createdAt"`
		Author      *githubHistoryActorRaw `json:"author"`
		Comments    *struct {
			TotalCount int `json:"totalCount"`
		} `json:"comments"`
	} `json:"nodes"`
}

type githubHistoryPullRaw struct {
	Number         int64                  `json:"number"`
	URL            string                 `json:"url"`
	Title          string                 `json:"title"`
	State          string                 `json:"state"`
	IsDraft        bool                   `json:"isDraft"`
	HeadRefName    string                 `json:"headRefName"`
	HeadRepository *githubHeadRepoRaw     `json:"headRepository"`
	CreatedAt      string                 `json:"createdAt"`
	UpdatedAt      string                 `json:"updatedAt"`
	MergedAt       string                 `json:"mergedAt"`
	ClosedAt       string                 `json:"closedAt"`
	Author         *githubHistoryActorRaw `json:"author"`
	Reviews        githubHistoryActsRaw   `json:"reviews"`
	Approvals      githubHistoryActsRaw   `json:"approvals"`
	Comments       githubHistoryActsRaw   `json:"comments"`
}

// The kinds of a pull request's acts a follow-up page reads.
const (
	githubActsReviews   = "reviews"
	githubActsApprovals = "approvals"
	githubActsComments  = "comments"
)

// githubHistoryWalk is where a GitHub repository's history read stands:
// its pull request list and the stretches of acts still to read.
type githubHistoryWalk struct {
	List listWalk         `json:"list"`
	More []githubMoreWalk `json:"more,omitempty"`
}

// githubMoreWalk is a stretch of one pull request's acts still to read:
// its reviews or conversation comments before Cursor, newest first, down
// to Stop (Unix ms; 0 is to the start); its approvals after Cursor.
type githubMoreWalk struct {
	Number int64  `json:"n"`
	Kind   string `json:"kind"`
	Cursor string `json:"cursor"`
	Stop   int64  `json:"stop,omitempty"`
}

// ReadPRHistory reads at most PRHistoryRequests requests: the list from
// the newest down to where the last read reached, then the follow-up
// pages, then the list's gap with what they leave, and the follow-up
// pages the gap's pages added with what that leaves. The follow-ups go
// first so that the stretches the walk holds stay those of the gap pages
// of one read: the gap walks on only once they are read.
func (f *githubForge) ReadPRHistory(ctx context.Context, repo RepoReference, in PRHistoryInput) (PRHistory, error) {
	owner, name, err := splitGitHubProject(repo.Project())
	if err != nil {
		return PRHistory{}, err
	}
	client, err := f.core.githubAPI(repo.Host)
	if err != nil {
		return PRHistory{}, err
	}
	var w githubHistoryWalk
	if len(in.Walk) > 0 {
		if err := json.Unmarshal(in.Walk, &w); err != nil {
			return PRHistory{}, fmt.Errorf("GitHub history of %s: read the walk: %w", repo.Key(), err)
		}
	}
	r := githubHistoryRead{ctx: ctx, client: client, owner: owner, name: name, repo: repo, stored: in.Stored,
		clock: f.core.nowFn, more: slices.Clone(w.More)}
	b := &historyBudget{left: PRHistoryRequests}
	floor := in.WindowStart.UnixMilli()
	err = func() error {
		if err := w.List.head(b, floor, r.page); err != nil {
			return err
		}
		if err := r.followLeft(b); err != nil {
			return err
		}
		if err := w.List.walk(b, floor, r.page); err != nil {
			return err
		}
		return r.followLeft(b)
	}()
	w.More = r.more
	out := PRHistory{OwnHead: r.ownHead, Parent: r.parent, Viewer: r.viewer, PRs: r.prs.prs, Acts: earliestActs(r.acts)}
	walk, encodeErr := json.Marshal(w)
	if encodeErr != nil {
		return out, errors.Join(err, fmt.Errorf("encode the history walk: %w", encodeErr))
	}
	out.Walk = walk
	at, ok := w.List.reach()
	setReach(&out, streamReach{at, ok})
	out.ActsOwed = len(w.More) > 0
	out.Owed = w.List.Gap || out.ActsOwed
	return out, err
}

// githubHistoryRead is one read's requests and what they found.
type githubHistoryRead struct {
	ctx         context.Context
	client      *forgeapi.Client
	owner, name string
	repo        RepoReference
	stored      func([]int64) (map[int64]StoredPR, error)
	clock       func() time.Time
	ownHead     string
	viewer      string
	parent      *RepoReference
	prs         historyPRs
	acts        []PRHistoryAct
	// more are the stretches of acts left to read: the walk's at the
	// start, each moved or removed as its pages are read, and those this
	// read's pages add.
	more []githubMoreWalk
}

// page is the list's pager: the page after cursor, keeping the pull
// requests updated at or after cutoff with their acts, and adding the
// stretches of acts a changed or new one leaves.
func (r *githubHistoryRead) page(cursor string, cutoff int64) (listPage, error) {
	var raw struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Repository *struct {
			githubRepoParentRaw
			PullRequests struct {
				PageInfo githubPageInfo         `json:"pageInfo"`
				Nodes    []githubHistoryPullRaw `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	seen := r.clock()
	vars := map[string]any{"owner": r.owner, "name": r.name, "after": nullable(cursor)}
	if _, err := r.client.GraphQL(r.ctx, githubPullHistoryQuery, vars, &raw); err != nil {
		return listPage{}, err
	}
	if raw.Repository == nil {
		return listPage{}, fmt.Errorf("GitHub answered no repository %s", r.repo.Project())
	}
	r.ownHead, r.viewer = raw.Repository.ID, raw.Viewer.Login
	r.parent = nil
	if p := raw.Repository.Parent; p != nil {
		r.parent = &RepoReference{Forge: r.repo.Forge, Host: r.repo.Host, Namespace: p.Owner.Login, Repo: p.Name}
	}
	var page listPage
	var prs []PRHistoryPR
	var nodes []githubHistoryPullRaw
	for _, node := range raw.Repository.PullRequests.Nodes {
		pr, err := r.pull(node, seen)
		if err != nil {
			return page, err
		}
		if pr.Updated.UnixMilli() < cutoff {
			page.below = true
			break
		}
		prs, nodes = append(prs, pr), append(nodes, node)
	}
	numbers := make([]int64, 0, len(prs))
	for _, pr := range prs {
		numbers = append(numbers, pr.Number)
	}
	stored := map[int64]StoredPR{}
	if len(numbers) > 0 {
		var err error
		if stored, err = r.stored(numbers); err != nil {
			return page, err
		}
	}
	for i, pr := range prs {
		node := nodes[i]
		r.prs.keep(pr)
		page.updated = append(page.updated, pr.Updated.UnixMilli())
		r.acts = append(r.acts, githubHistoryActs(pr.Number, githubActsReviews, node.Reviews)...)
		r.acts = append(r.acts, githubHistoryActs(pr.Number, githubActsApprovals, node.Approvals)...)
		r.acts = append(r.acts, githubHistoryActs(pr.Number, githubActsComments, node.Comments)...)
		prior, known := stored[pr.Number]
		if known && prior.Updated.Equal(pr.Updated) {
			continue
		}
		stop := int64(0)
		if known {
			stop = prior.Seen.Add(-githubHistorySeenMargin).UnixMilli()
		}
		r.leave(pr.Number, githubActsReviews, node.Reviews, stop)
		r.leave(pr.Number, githubActsApprovals, node.Approvals, 0)
		r.leave(pr.Number, githubActsComments, node.Comments, stop)
	}
	info := raw.Repository.PullRequests.PageInfo
	page.cursor, page.more = info.EndCursor, info.HasNextPage && info.EndCursor != ""
	return page, nil
}

// leave adds the stretch of pull request number's acts of kind that page
// left unread, if any: older ones down to stop, newer approvals. A
// pull request already waiting for its approvals keeps its stretch,
// which is further on.
func (r *githubHistoryRead) leave(number int64, kind string, page githubHistoryActsRaw, stop int64) {
	m, ok := githubHistoryLeft(number, kind, page, stop)
	if !ok {
		return
	}
	if kind == githubActsApprovals && slices.ContainsFunc(r.more, func(o githubMoreWalk) bool { return o.Number == number && o.Kind == kind }) {
		return
	}
	r.more = append(r.more, m)
}

// githubHistoryLeft is the stretch of acts page leaves unread: of
// approvals, the next page's; of reviews or comments, read newest first,
// the older page's unless page reached stop.
func githubHistoryLeft(number int64, kind string, page githubHistoryActsRaw, stop int64) (githubMoreWalk, bool) {
	info := page.PageInfo
	if kind == githubActsApprovals {
		return githubMoreWalk{Number: number, Kind: kind, Cursor: info.EndCursor}, info.HasNextPage && info.EndCursor != ""
	}
	if !info.HasPreviousPage || info.StartCursor == "" || len(page.Nodes) == 0 {
		return githubMoreWalk{}, false
	}
	oldest := page.Nodes[0]
	at, err := time.Parse(time.RFC3339, cmp.Or(oldest.CreatedAt, oldest.SubmittedAt))
	if err == nil && at.UnixMilli() <= stop {
		return githubMoreWalk{}, false
	}
	return githubMoreWalk{Number: number, Kind: kind, Cursor: info.StartCursor, Stop: stop}, true
}

// pull is node as history keeps it, seen at seen.
func (r *githubHistoryRead) pull(node githubHistoryPullRaw, seen time.Time) (PRHistoryPR, error) {
	times := make([]time.Time, 4)
	for i, raw := range []string{node.CreatedAt, node.UpdatedAt, node.MergedAt, node.ClosedAt} {
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return PRHistoryPR{}, fmt.Errorf("GitHub pull request %s#%d: time %q: %w", r.repo.Project(), node.Number, raw, err)
		}
		times[i] = t
	}
	pr := PRHistoryPR{
		Number: node.Number, URL: node.URL, Title: node.Title, HeadRef: node.HeadRefName,
		State: githubHistoryState(node.State, node.IsDraft), Created: times[0], Updated: times[1], Merged: times[2], Closed: times[3], Seen: seen,
	}
	if node.HeadRepository != nil {
		pr.HeadRepo = node.HeadRepository.ID
	}
	if node.Author != nil {
		pr.Author = PRListAuthor{Login: node.Author.Login, Name: node.Author.Name}
	}
	if pr.Updated.IsZero() || pr.Number <= 0 {
		return PRHistoryPR{}, fmt.Errorf("GitHub pull request %s#%d has no update time", r.repo.Project(), node.Number)
	}
	return pr, nil
}

// followLeft reads the stretches of acts left, each to its end, as far as
// b goes; each moves, or leaves, only as its pages are read. A stretch of
// a pull request the store no longer holds, which the window has passed,
// and that this read has not read, is dropped, as is one of a pull
// request the forge no longer has.
func (r *githubHistoryRead) followLeft(b *historyBudget) error {
	var numbers []int64
	for _, m := range r.more {
		if _, read := r.prs.byNumber[m.Number]; !read && !slices.Contains(numbers, m.Number) {
			numbers = append(numbers, m.Number)
		}
	}
	if len(numbers) > 0 {
		stored, err := r.stored(numbers)
		if err != nil {
			return err
		}
		r.more = slices.DeleteFunc(r.more, func(m githubMoreWalk) bool {
			_, read := r.prs.byNumber[m.Number]
			_, held := stored[m.Number]
			return !read && !held
		})
	}
	for len(r.more) > 0 {
		if !b.take() {
			return nil
		}
		m := r.more[0]
		page, found, err := r.morePage(m)
		if err != nil {
			return err
		}
		r.more = r.more[1:]
		if !found {
			continue
		}
		r.acts = append(r.acts, githubHistoryActs(m.Number, m.Kind, page)...)
		if next, ok := githubHistoryLeft(m.Number, m.Kind, page, m.Stop); ok {
			r.more = append([]githubMoreWalk{next}, r.more...)
		}
	}
	return nil
}

// morePage reads the follow-up page of stretch m; false when the forge no
// longer has its pull request.
func (r *githubHistoryRead) morePage(m githubMoreWalk) (githubHistoryActsRaw, bool, error) {
	var raw struct {
		Repository *struct {
			PullRequest *struct {
				Reviews   *githubHistoryActsRaw `json:"reviews"`
				Approvals *githubHistoryActsRaw `json:"approvals"`
				Comments  *githubHistoryActsRaw `json:"comments"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": r.owner, "name": r.name, "number": m.Number,
		"wantReviews": m.Kind == githubActsReviews, "reviewsBefore": nil,
		"wantApprovals": m.Kind == githubActsApprovals, "approvalsAfter": nil,
		"wantComments": m.Kind == githubActsComments, "commentsBefore": nil}
	vars[map[string]string{githubActsReviews: "reviewsBefore", githubActsApprovals: "approvalsAfter", githubActsComments: "commentsBefore"}[m.Kind]] = m.Cursor
	if _, err := r.client.GraphQL(r.ctx, githubPullHistoryMoreQuery, vars, &raw); err != nil {
		if graphQLNotFoundError(err) {
			return githubHistoryActsRaw{}, false, nil
		}
		return githubHistoryActsRaw{}, false, err
	}
	if raw.Repository == nil {
		return githubHistoryActsRaw{}, false, fmt.Errorf("GitHub answered no repository %s", r.repo.Project())
	}
	pull := raw.Repository.PullRequest
	if pull == nil {
		return githubHistoryActsRaw{}, false, nil
	}
	page := map[string]*githubHistoryActsRaw{githubActsReviews: pull.Reviews, githubActsApprovals: pull.Approvals, githubActsComments: pull.Comments}[m.Kind]
	if page == nil {
		return githubHistoryActsRaw{}, false, fmt.Errorf("GitHub answered no %s of %s#%d", m.Kind, r.repo.Project(), m.Number)
	}
	return *page, true, nil
}

// nullable is s, or nil for an empty cursor.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// githubHistoryActs are a page's acts of kind: a commenting review or a
// conversation comment is a comment; an approval is an approval, and a
// comment too when it carries line comments. An author that is not a
// User, or none, is no one.
func githubHistoryActs(number int64, kind string, page githubHistoryActsRaw) []PRHistoryAct {
	var out []PRHistoryAct
	for _, node := range page.Nodes {
		if node.Author == nil || node.Author.Typename != "User" || node.Author.Login == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, cmp.Or(node.CreatedAt, node.SubmittedAt))
		if err != nil {
			continue
		}
		person := PRListAuthor{Login: node.Author.Login, Name: node.Author.Name}
		if kind != githubActsApprovals {
			out = append(out, PRHistoryAct{Number: number, Person: person, At: t})
			continue
		}
		out = append(out, PRHistoryAct{Number: number, Approved: true, Person: person, At: t})
		if node.Comments != nil && node.Comments.TotalCount > 0 {
			out = append(out, PRHistoryAct{Number: number, Person: person, At: t})
		}
	}
	return out
}

// githubHistoryState is a pull request's state as history names it.
func githubHistoryState(state string, draft bool) string {
	switch strings.ToUpper(state) {
	case "MERGED":
		return "merged"
	case "CLOSED":
		return "closed"
	}
	if draft {
		return "draft"
	}
	return "open"
}
