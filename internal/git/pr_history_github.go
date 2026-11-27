package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
)

// A GitHub repository's history is two streams: the pull request list by
// update time, 50 a page, each with its first 50 reviews and conversation
// comments and the rest read by follow-up pages; and the repository's
// review comments (line comments and their replies) by update time, 100
// a page, which name people who commented only on the code, inside an
// approval or not.

// githubHistoryPageSize is the pull requests a history page holds, and
// githubHistoryNested the reviews and the conversation comments it reads
// of each. A page costs 1 + 2 * 50 connection requests, one point
// (GitHub's GraphQL rate limit divides by 100). Its answer is bounded by
// githubHistoryMaxBody, the transport's ceiling: 8 MiB holds 50 pull
// requests of 167 KB, 100 nested items of 1.6 KB each beside the pull
// request's own fields. Each item carries a state or nothing, a time, a
// login and a display name.
const (
	githubHistoryPageSize = 50
	githubHistoryNested   = 50
	githubHistoryMaxBody  = 8 << 20
)

const githubHistoryActor = `author { __typename login ... on User { name } }`

const githubHistoryReviews = `pageInfo { hasNextPage endCursor } nodes { state submittedAt ` + githubHistoryActor + ` }`

const githubHistoryComments = `pageInfo { hasNextPage endCursor } nodes { createdAt ` + githubHistoryActor + ` }`

const githubHistoryPullFields = `number url title state isDraft headRefName headRepository { id } createdAt updatedAt mergedAt closedAt ` + githubHistoryActor

// githubPullHistoryQuery is one page of a repository's pull requests,
// most recently updated first, each with its first reviews and
// conversation comments, beside the repository's node id and parent.
var githubPullHistoryQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PullHistory($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    id parent { owner { login } name }
    pullRequests(first: 50, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        `+githubHistoryPullFields+`
        reviews(first: 50) { `+githubHistoryReviews+` }
        comments(first: 50) { `+githubHistoryComments+` }
      }
    }
  }
}`).WithMaxBody(githubHistoryMaxBody)

// githubPullHistoryMoreQuery is one pull request's reviews and comments
// from their cursors, 100 a page, and with wantPull its own fields: a
// pull request a review comment names that the read has not read.
var githubPullHistoryMoreQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PullHistoryMore($owner: String!, $name: String!, $number: Int!, $wantPull: Boolean!, $wantReviews: Boolean!, $reviewsAfter: String, $wantComments: Boolean!, $commentsAfter: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      ... on PullRequest @include(if: $wantPull) { `+githubHistoryPullFields+` }
      reviews(first: 100, after: $reviewsAfter) @include(if: $wantReviews) { `+githubHistoryReviews+` }
      comments(first: 100, after: $commentsAfter) @include(if: $wantComments) { `+githubHistoryComments+` }
    }
  }
}`).WithMaxBody(githubHistoryMaxBody)

type githubHistoryActorRaw struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
	Name     string `json:"name"`
}

type githubHistoryItemsRaw struct {
	PageInfo githubPageInfo `json:"pageInfo"`
	Nodes    []struct {
		State       string                 `json:"state"`
		SubmittedAt string                 `json:"submittedAt"`
		CreatedAt   string                 `json:"createdAt"`
		Author      *githubHistoryActorRaw `json:"author"`
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
	Reviews        githubHistoryItemsRaw  `json:"reviews"`
	Comments       githubHistoryItemsRaw  `json:"comments"`
}

// githubReviewCommentRaw is a review comment of the repository's list
// (GET repos/O/R/pulls/comments).
type githubReviewCommentRaw struct {
	ID   int64 `json:"id"`
	User *struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	PullRequestURL string `json:"pull_request_url"`
}

// githubReviewCommentsMaxBody bounds a page of 100 review comments, each
// with its body and diff hunk; not measured. 8 MiB holds 100 of 84 KB.
const githubReviewCommentsMaxBody = 8 << 20

// githubCommentsLag is how far before the first read's time its review
// comment stream starts reading new comments, so a forge clock behind
// this computer's loses none.
const githubCommentsLag = 5 * time.Minute

// githubHistoryWalk is where a GitHub repository's history read stands:
// its pull request list, the follow-up pages of pull requests whose
// reviews or comments a read could not finish, and its review comments.
type githubHistoryWalk struct {
	List     listWalk           `json:"list"`
	More     []githubMoreWalk   `json:"more,omitempty"`
	Comments githubCommentsWalk `json:"comments"`
}

// githubMoreWalk is a pull request's reviews and comments still to read,
// from their cursors.
type githubMoreWalk struct {
	Number        int64  `json:"n"`
	Reviews       bool   `json:"reviews,omitempty"`
	ReviewsAfter  string `json:"reviewsAfter,omitempty"`
	Comments      bool   `json:"comments,omitempty"`
	CommentsAfter string `json:"commentsAfter,omitempty"`
}

// githubCommentsWalk is where the review comment stream stands. The list
// can only be read oldest update first, from a time (since), and its
// pages never by number, so an edit or a deletion moves nothing past a
// position. Top is the newest update read: each read reads on from it to
// the present, and Behind is set while it has not reached the present,
// when the stream has read nothing back to any time. The gap's walk reads
// each band oldest first from its lower end: while a band is in progress
// (Pages > 0), Lo is that end, At the newest update it read and Pages the
// pages it took.
type githubCommentsWalk struct {
	Top    int64  `json:"top,omitempty"`
	Behind bool   `json:"behind,omitempty"`
	Gap    actGap `json:"gap"`
	Lo     int64  `json:"lo,omitempty"`
	At     int64  `json:"at,omitempty"`
	Pages  int    `json:"pages,omitempty"`
}

// ReadPRHistory reads at most PRHistoryRequests requests: the list and
// the review comments from the newest down to where the last read
// reached, the follow-up pages the last read left, then the list's gap
// and the review comments' gap with what is left.
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
	r := githubHistoryRead{ctx: ctx, client: client, owner: owner, name: name, repo: repo, known: in.Known,
		budget: &historyBudget{left: PRHistoryRequests}, now: f.core.nowFn()}
	floor := in.WindowStart.UnixMilli()
	w.Comments.Gap.clip(floor)
	err = func() error {
		if err := w.List.head(r.budget, floor, r.page); err != nil {
			return err
		}
		if err := r.commentsHead(&w.Comments, floor); err != nil {
			return err
		}
		if err := r.followLeft(&w); err != nil {
			return err
		}
		half := &historyBudget{left: (r.budget.left + 1) / 2}
		spent := half.left
		err := w.List.walk(half, floor, r.pager(half))
		r.budget.left -= spent - half.left
		if err != nil {
			return err
		}
		if err := r.commentsGap(&w.Comments, max(w.List.reach(), floor)); err != nil {
			return err
		}
		return w.List.walk(r.budget, floor, r.page)
	}()
	if errors.Is(err, errHistoryBudget) {
		err = nil
	}
	w.More = r.more
	nameActs(r.acts, r.prs.prs)
	out := PRHistory{OwnHead: r.ownHead, Parent: r.parent, PRs: r.prs.prs, Acts: earliestActs(r.acts)}
	walk, encodeErr := json.Marshal(w)
	if encodeErr != nil {
		return out, errors.Join(err, fmt.Errorf("encode the history walk: %w", encodeErr))
	}
	out.Walk = walk
	out.Reach = fromUnixMilli(max(w.List.reach(), w.Comments.Gap.reach()))
	out.NoReach = w.Comments.Behind
	out.Owed = w.List.Gap || len(w.More) > 0 || w.Comments.Behind || w.Comments.Gap.Open
	return out, err
}

// githubHistoryRead is one read's requests and what they found.
type githubHistoryRead struct {
	ctx         context.Context
	client      *forgeapi.Client
	owner, name string
	repo        RepoReference
	known       func([]int64) (map[int64]bool, error)
	budget      *historyBudget
	now         time.Time
	ownHead     string
	parent      *RepoReference
	prs         historyPRs
	acts        []PRHistoryAct
	// more are the follow-up pages this read leaves to the next.
	more []githubMoreWalk
}

// page is the list's pager on the read's own budget.
func (r *githubHistoryRead) page(cursor string, cutoff int64) (listPage, error) {
	return r.pager(r.budget)(cursor, cutoff)
}

// pager reads the page after cursor and keeps its pull requests updated at
// or after cutoff, each with its acts read on with budget's requests: a
// pull request the budget cannot finish leaves its follow-up pages to the
// next read.
func (r *githubHistoryRead) pager(budget *historyBudget) listPager {
	return func(cursor string, cutoff int64) (listPage, error) {
		var raw struct {
			Repository *struct {
				githubRepoParentRaw
				PullRequests struct {
					PageInfo githubPageInfo         `json:"pageInfo"`
					Nodes    []githubHistoryPullRaw `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": r.owner, "name": r.name, "after": nullable(cursor)}
		if _, err := r.client.GraphQL(r.ctx, githubPullHistoryQuery, vars, &raw); err != nil {
			return listPage{}, err
		}
		if raw.Repository == nil {
			return listPage{}, fmt.Errorf("GitHub answered no repository %s", r.repo.Project())
		}
		r.ownHead = raw.Repository.ID
		r.parent = nil
		if p := raw.Repository.Parent; p != nil {
			r.parent = &RepoReference{Forge: r.repo.Forge, Host: r.repo.Host, Namespace: p.Owner.Login, Repo: p.Name}
		}
		var page listPage
		for _, node := range raw.Repository.PullRequests.Nodes {
			pr, err := r.pull(node)
			if err != nil {
				return page, err
			}
			if pr.Updated.UnixMilli() < cutoff {
				page.below = true
				break
			}
			if err := r.readOn(&pr, budget, node.Reviews, node.Comments); err != nil {
				return page, err
			}
			r.prs.keep(pr)
			page.updated = append(page.updated, pr.Updated.UnixMilli())
		}
		info := raw.Repository.PullRequests.PageInfo
		page.cursor, page.more = info.EndCursor, info.HasNextPage && info.EndCursor != ""
		return page, nil
	}
}

// pull is node as history keeps it, its acts not read yet.
func (r *githubHistoryRead) pull(node githubHistoryPullRaw) (PRHistoryPR, error) {
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
		State: githubHistoryState(node.State, node.IsDraft), Created: times[0], Updated: times[1], Merged: times[2], Closed: times[3],
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

// readOn gives pr the acts of its first reviews and comments and reads
// the rest with budget's requests. Its acts are whole when it read them
// all; otherwise the rest are left to the next read.
func (r *githubHistoryRead) readOn(pr *PRHistoryPR, budget *historyBudget, reviews, comments githubHistoryItemsRaw) error {
	r.dropMore(pr.Number)
	acts := append(githubHistoryActs(pr.Number, reviews, true), githubHistoryActs(pr.Number, comments, false)...)
	rest := githubMoreWalk{Number: pr.Number, Reviews: reviews.PageInfo.HasNextPage, ReviewsAfter: reviews.PageInfo.EndCursor,
		Comments: comments.PageInfo.HasNextPage, CommentsAfter: comments.PageInfo.EndCursor}
	more, err := r.follow(&rest, budget)
	pr.Acts, pr.ActsWhole = earliestActs(append(acts, more...)), err == nil && !rest.Reviews && !rest.Comments
	if errors.Is(err, errHistoryBudget) {
		r.more = append(r.more, rest)
		return nil
	}
	return err
}

// dropMore forgets the follow-up pages left for pull request n, which a
// read of it from the start replaces.
func (r *githubHistoryRead) dropMore(n int64) {
	for i, m := range r.more {
		if m.Number == n {
			r.more = append(r.more[:i], r.more[i+1:]...)
			return
		}
	}
}

// follow reads m's reviews and comments on from its cursors with
// budget's requests, moving m to where it stopped, and answers their
// acts; errHistoryBudget when the budget ran out first.
func (r *githubHistoryRead) follow(m *githubMoreWalk, budget *historyBudget) ([]PRHistoryAct, error) {
	var acts []PRHistoryAct
	for m.Reviews || m.Comments {
		if !budget.take() {
			return acts, errHistoryBudget
		}
		got, err := r.moreQuery(m.Number, false, m.Reviews, m.ReviewsAfter, m.Comments, m.CommentsAfter)
		if err != nil {
			return acts, err
		}
		if got == nil {
			// The forge no longer has the pull request.
			m.Reviews, m.Comments = false, false
			return acts, nil
		}
		if m.Reviews && got.Reviews != nil {
			acts = append(acts, githubHistoryActs(m.Number, *got.Reviews, true)...)
			m.Reviews, m.ReviewsAfter = got.Reviews.PageInfo.HasNextPage, got.Reviews.PageInfo.EndCursor
		} else {
			m.Reviews = false
		}
		if m.Comments && got.Comments != nil {
			acts = append(acts, githubHistoryActs(m.Number, *got.Comments, false)...)
			m.Comments, m.CommentsAfter = got.Comments.PageInfo.HasNextPage, got.Comments.PageInfo.EndCursor
		} else {
			m.Comments = false
		}
	}
	return acts, nil
}

// githubHistoryMoreRaw is PullHistoryMore's pull request.
type githubHistoryMoreRaw struct {
	githubHistoryPullRaw
	Reviews  *githubHistoryItemsRaw `json:"reviews"`
	Comments *githubHistoryItemsRaw `json:"comments"`
}

// moreQuery asks PullHistoryMore for pull request number; nil when the
// forge has no such pull request.
func (r *githubHistoryRead) moreQuery(number int64, wantPull, wantReviews bool, reviewsAfter string, wantComments bool, commentsAfter string) (*githubHistoryMoreRaw, error) {
	var raw struct {
		Repository *struct {
			PullRequest *githubHistoryMoreRaw `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": r.owner, "name": r.name, "number": number, "wantPull": wantPull,
		"wantReviews": wantReviews, "reviewsAfter": nullable(reviewsAfter), "wantComments": wantComments, "commentsAfter": nullable(commentsAfter)}
	if _, err := r.client.GraphQL(r.ctx, githubPullHistoryMoreQuery, vars, &raw); err != nil {
		if graphQLNotFoundError(err) {
			return nil, nil
		}
		return nil, err
	}
	if raw.Repository == nil {
		return nil, fmt.Errorf("GitHub answered no repository %s", r.repo.Project())
	}
	return raw.Repository.PullRequest, nil
}

// followLeft reads the follow-up pages the last read left, as far as the
// budget goes; their acts add to the stored ones. A pull request this
// read reads from the start drops its own (dropMore). When the acts' pull
// requests cannot be resolved, nothing the pages read counts and every
// one is left as it was.
func (r *githubHistoryRead) followLeft(w *githubHistoryWalk) error {
	left := slices.DeleteFunc(slices.Clone(w.More), func(m githubMoreWalk) bool { _, read := r.prs.byNumber[m.Number]; return read })
	work := slices.Clone(left)
	var acts []PRHistoryAct
	var stopped error
	for i := range work {
		got, err := r.follow(&work[i], r.budget)
		acts = append(acts, got...)
		if err != nil {
			stopped = err
			break
		}
	}
	kept, err := resolveActs(acts, &r.prs, r.known, r.fetch)
	if err != nil {
		r.more = append(r.more, left...)
		return err
	}
	r.acts = append(r.acts, kept...)
	for _, m := range work {
		if m.Reviews || m.Comments {
			r.more = append(r.more, m)
		}
	}
	return stopped
}

// fetch reads the pull requests numbers names with their first reviews
// and comments, one request each; one the forge does not have is not
// kept.
func (r *githubHistoryRead) fetch(numbers []int64) error {
	for _, n := range numbers {
		if !r.budget.take() {
			return errHistoryBudget
		}
		got, err := r.moreQuery(n, true, true, "", true, "")
		if err != nil {
			return err
		}
		if got == nil {
			continue
		}
		pr, err := r.pull(got.githubHistoryPullRaw)
		if err != nil {
			return err
		}
		var reviews, comments githubHistoryItemsRaw
		if got.Reviews != nil {
			reviews = *got.Reviews
		}
		if got.Comments != nil {
			comments = *got.Comments
		}
		if err := r.readOn(&pr, r.budget, reviews, comments); err != nil {
			return err
		}
		r.prs.keep(pr)
	}
	return nil
}

// commentsHead reads the review comments updated since the walk's Top
// to the present, a page at a time from the newest update read, and the
// first read opens the gap down to the window's start.
func (r *githubHistoryRead) commentsHead(w *githubCommentsWalk, floor int64) error {
	if w.Top == 0 && !w.Gap.Open {
		top := r.now.Add(-githubCommentsLag).UnixMilli()
		w.Gap.open(top, floor)
		w.Top = top
		return nil
	}
	for {
		acts, newest, full, err := r.commentsPage(w.Top, 0)
		if err != nil {
			if errors.Is(err, errHistoryBudget) {
				w.Behind = true
			}
			return err
		}
		r.acts = append(r.acts, acts...)
		w.Top = max(w.Top, newest)
		if !full {
			w.Behind = false
			return nil
		}
	}
}

// commentsGap walks the review comments' gap back a band at a time, each
// oldest first from its lower end, never past floor, the list's reach. A
// band in progress keeps the lower end it began with: the next band's
// would leave the comments between them unread.
func (r *githubHistoryRead) commentsGap(w *githubCommentsWalk, floor int64) error {
	for {
		lo, hi, ok := w.Gap.band(floor)
		if !ok {
			return nil
		}
		if w.Pages > 0 {
			lo = w.Lo
		}
		w.Lo = lo
		at := max(w.At, lo)
		for {
			acts, newest, full, err := r.commentsPage(at, hi)
			if err != nil {
				return err
			}
			w.Pages++
			r.acts = append(r.acts, acts...)
			if newest > hi || !full {
				break
			}
			at = max(at, newest)
			w.At = at
		}
		w.Gap.walked(lo, w.Pages)
		w.Lo, w.At, w.Pages = 0, 0, 0
	}
}

// commentsPage reads the page of review comments updated from a second
// before since, oldest first: its acts, up to hi when it is not 0, each
// with its pull request read or known (resolveActs), the newest update it
// read and whether the page was full. More than a page's comments sharing
// one second is a failure, since no request can move past them.
func (r *githubHistoryRead) commentsPage(since, hi int64) ([]PRHistoryAct, int64, bool, error) {
	if !r.budget.take() {
		return nil, 0, false, errHistoryBudget
	}
	query := url.Values{"sort": {"updated"}, "direction": {"asc"}, "per_page": {strconv.Itoa(gitlabPageSize)},
		"since": {fromUnixMilli(since - time.Second.Milliseconds()).Format(time.RFC3339)}}
	var raw []githubReviewCommentRaw
	if _, err := r.client.JSON(r.ctx, forgeapi.Request{Path: "repos/" + r.owner + "/" + r.name + "/pulls/comments", Query: query, MaxBodyBytes: githubReviewCommentsMaxBody}, &raw); err != nil {
		return nil, 0, false, err
	}
	var acts []PRHistoryAct
	newest := int64(0)
	for _, c := range raw {
		updated, err := time.Parse(time.RFC3339, c.UpdatedAt)
		if err != nil {
			return nil, 0, false, fmt.Errorf("GitHub review comment %d of %s: time %q: %w", c.ID, r.repo.Project(), c.UpdatedAt, err)
		}
		newest = max(newest, updated.UnixMilli())
		if hi != 0 && updated.UnixMilli() > hi {
			break
		}
		created, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil {
			return nil, 0, false, fmt.Errorf("GitHub review comment %d of %s: time %q: %w", c.ID, r.repo.Project(), c.CreatedAt, err)
		}
		number, err := strconv.ParseInt(path.Base(c.PullRequestURL), 10, 64)
		if err != nil || !strings.Contains(c.PullRequestURL, "/pulls/") {
			return nil, 0, false, fmt.Errorf("GitHub review comment %d of %s names pull request %q", c.ID, r.repo.Project(), c.PullRequestURL)
		}
		if c.User == nil || c.User.Type != "User" || c.User.Login == "" {
			continue
		}
		acts = append(acts, PRHistoryAct{Number: number, Person: PRListAuthor{Login: c.User.Login}, At: created})
	}
	full := len(raw) == gitlabPageSize
	if full && newest <= since {
		return nil, 0, false, fmt.Errorf("GitHub history of %s: more than %d review comments share the update time %s, so the read cannot move past it",
			r.repo.Key(), gitlabPageSize, fromUnixMilli(since).Format(time.RFC3339))
	}
	kept, err := resolveActs(acts, &r.prs, r.known, r.fetch)
	if err != nil {
		return nil, 0, false, err
	}
	return kept, newest, full, nil
}

// nullable is s, or nil for an empty cursor.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// githubHistoryActs are a list's acts: a submitted review is an approval
// when it approves and a comment when it comments or requests changes; a
// pending review, a dismissed one, a bot's and a deleted account's are
// none. A review's line comments are the review comment stream's.
func githubHistoryActs(number int64, items githubHistoryItemsRaw, reviews bool) []PRHistoryAct {
	var out []PRHistoryAct
	for _, node := range items.Nodes {
		at := node.CreatedAt
		if reviews {
			at = node.SubmittedAt
		}
		if node.Author == nil || node.Author.Typename != "User" || node.Author.Login == "" || at == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			continue
		}
		act := PRHistoryAct{Number: number, Person: PRListAuthor{Login: node.Author.Login, Name: node.Author.Name}, At: t}
		if reviews {
			switch node.State {
			case "APPROVED":
				act.Approved = true
			case "COMMENTED", "CHANGES_REQUESTED":
			default:
				continue
			}
		}
		out = append(out, act)
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
