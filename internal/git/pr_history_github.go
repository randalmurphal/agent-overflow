package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
)

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
	// githubHistoryMorePages bounds the follow-up pages of one pull
	// request's reviews and comments; past it its acts are added to the
	// stored ones instead of replacing them.
	githubHistoryMorePages = 10
)

const githubHistoryActor = `author { __typename login ... on User { name } }`

const githubHistoryReviews = `pageInfo { hasNextPage endCursor } nodes { state submittedAt ` + githubHistoryActor + ` }`

const githubHistoryComments = `pageInfo { hasNextPage endCursor } nodes { createdAt ` + githubHistoryActor + ` }`

// githubPullHistoryQuery is one page of a repository's pull requests,
// most recently updated first, each with its first reviews and
// conversation comments, beside the repository's node id and parent.
var githubPullHistoryQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PullHistory($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    id parent { owner { login } name }
    pullRequests(first: 50, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number url title state isDraft headRefName headRepository { id } createdAt updatedAt mergedAt closedAt `+githubHistoryActor+`
        reviews(first: 50) { `+githubHistoryReviews+` }
        comments(first: 50) { `+githubHistoryComments+` }
      }
    }
  }
}`).WithMaxBody(githubHistoryMaxBody)

// githubPullHistoryMoreQuery is a pull request's reviews and comments
// past those a history page held.
var githubPullHistoryMoreQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PullHistoryMore($owner: String!, $name: String!, $number: Int!, $wantReviews: Boolean!, $reviewsAfter: String, $wantComments: Boolean!, $commentsAfter: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
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

// githubHistoryWalk is where a GitHub repository's history read stands.
// Top is the newest update read (Unix ms): each read first reads down to
// it from the newest. Gap is a stretch not read yet, from GapHi, the
// oldest update its walk has read (0 before any), down to GapLo, below
// which everything is read or outside the window; the walk continues at
// GapCursor ("" at the newest). The first read is a gap down to the
// window's start. A read that cannot reach Top within its pages leaves
// the rest as the gap, joined with any gap still open. Moves in this
// order only go up, so a cursor never skips a pull request: one updated
// since is above it, which the next read's first pages read.
type githubHistoryWalk struct {
	Top       int64  `json:"top,omitempty"`
	Gap       bool   `json:"gap,omitempty"`
	GapCursor string `json:"gapCursor,omitempty"`
	GapHi     int64  `json:"gapHi,omitempty"`
	GapLo     int64  `json:"gapLo,omitempty"`
	Walked    bool   `json:"walked,omitempty"`
}

// ReadPRHistory reads at most PRHistoryPages pages: first from the newest
// down to the walk's Top, then the gap's walk with the pages left.
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
	floor := in.WindowStart.UnixMilli()
	if w.Top == 0 && !w.Gap {
		w = githubHistoryWalk{Gap: true, GapLo: floor}
	}
	r := githubHistoryRead{ctx: ctx, client: client, owner: owner, name: name, repo: repo, byNumber: map[int64]int{}}
	moved, behind := false, false
	err = func() error {
		if w.Top > 0 {
			// From the newest down to Top.
			var cursor string
			newest, oldest, reached := int64(0), int64(0), false
			for r.pages < PRHistoryPages && !reached {
				page, err := r.page(cursor)
				if err != nil {
					return err
				}
				moved = true
				for _, node := range page.nodes {
					if node.updated < max(w.Top, floor) {
						reached = true
						break
					}
					newest, oldest = max(newest, node.updated), minNonZero(oldest, node.updated)
				}
				if !page.more {
					reached = true
				}
				cursor = page.cursor
			}
			if !reached {
				behind = true
				lo := w.Top
				if w.Gap {
					lo = w.GapLo
				}
				w.Gap, w.GapCursor, w.GapHi, w.GapLo = true, cursor, oldest, lo
			}
			w.Top = max(w.Top, newest)
		}
		for w.Gap && r.pages < PRHistoryPages {
			page, err := r.page(w.GapCursor)
			if err != nil {
				return err
			}
			moved = true
			closed := !page.more
			for _, node := range page.nodes {
				if node.updated < max(w.GapLo, floor) {
					closed = true
					break
				}
				w.Top = max(w.Top, node.updated)
				w.GapHi = minNonZero(w.GapHi, node.updated)
			}
			w.GapCursor = page.cursor
			if closed {
				w.Gap, w.GapCursor, w.GapHi, w.GapLo, w.Walked = false, "", 0, 0, true
				w.Top = max(w.Top, floor)
			}
		}
		return nil
	}()
	out := PRHistory{OwnHead: r.ownHead, Parent: r.parent, PRs: r.prs, Walked: w.Walked, Behind: behind}
	if !moved {
		return out, err
	}
	walk, encodeErr := json.Marshal(w)
	if encodeErr != nil {
		return out, errors.Join(err, fmt.Errorf("encode the history walk: %w", encodeErr))
	}
	out.Walk = walk
	if w.Gap {
		out.Reach = fromUnixMilli(w.GapHi)
	}
	return out, err
}

func minNonZero(a, b int64) int64 {
	if a == 0 || b < a {
		return b
	}
	return a
}

// githubHistoryRead is one read's requests and what they found.
type githubHistoryRead struct {
	ctx         context.Context
	client      *forgeapi.Client
	owner, name string
	repo        RepoReference
	pages       int
	ownHead     string
	parent      *RepoReference
	prs         []PRHistoryPR
	byNumber    map[int64]int
}

// githubHistoryPage is a page as the walk reads it: each node's number
// and update time, the cursor past it and whether more follow.
type githubHistoryPage struct {
	nodes  []struct{ updated int64 }
	cursor string
	more   bool
}

// page reads the page after cursor and keeps its pull requests (a pull
// request read twice keeps its newest read).
func (r *githubHistoryRead) page(cursor string) (githubHistoryPage, error) {
	r.pages++
	var raw struct {
		Repository *struct {
			githubRepoParentRaw
			PullRequests struct {
				PageInfo githubPageInfo         `json:"pageInfo"`
				Nodes    []githubHistoryPullRaw `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": r.owner, "name": r.name, "after": nil}
	if cursor != "" {
		vars["after"] = cursor
	}
	if _, err := r.client.GraphQL(r.ctx, githubPullHistoryQuery, vars, &raw); err != nil {
		return githubHistoryPage{}, err
	}
	if raw.Repository == nil {
		return githubHistoryPage{}, fmt.Errorf("GitHub answered no repository %s", r.repo.Project())
	}
	r.ownHead = raw.Repository.ID
	r.parent = nil
	if p := raw.Repository.Parent; p != nil {
		r.parent = &RepoReference{Forge: r.repo.Forge, Host: r.repo.Host, Namespace: p.Owner.Login, Repo: p.Name}
	}
	var page githubHistoryPage
	for _, node := range raw.Repository.PullRequests.Nodes {
		pr, err := r.pull(node)
		if err != nil {
			return page, err
		}
		if at, ok := r.byNumber[pr.Number]; ok {
			if !pr.Updated.Before(r.prs[at].Updated) {
				r.prs[at] = pr
			}
		} else {
			r.byNumber[pr.Number] = len(r.prs)
			r.prs = append(r.prs, pr)
		}
		page.nodes = append(page.nodes, struct{ updated int64 }{pr.Updated.UnixMilli()})
	}
	info := raw.Repository.PullRequests.PageInfo
	page.cursor, page.more = info.EndCursor, info.HasNextPage && info.EndCursor != ""
	return page, nil
}

// pull is node as history keeps it, its acts read whole: a review or
// comment list past its first page is read on, at most
// githubHistoryMorePages pages each, and one still longer leaves the
// acts read to add to the stored ones.
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
		ActsWhole: true,
	}
	if node.HeadRepository != nil {
		pr.HeadRepo = node.HeadRepository.ID
	}
	if node.Author != nil {
		pr.Author = PRListAuthor{Login: node.Author.Login, Name: node.Author.Name}
	}
	if pr.Updated.IsZero() {
		return PRHistoryPR{}, fmt.Errorf("GitHub pull request %s#%d has no update time", r.repo.Project(), node.Number)
	}
	reviews, comments := node.Reviews, node.Comments
	var acts []PRHistoryAct
	for more := 0; ; more++ {
		acts = append(acts, githubHistoryActs(pr.Number, reviews, true)...)
		acts = append(acts, githubHistoryActs(pr.Number, comments, false)...)
		wantReviews, wantComments := reviews.PageInfo.HasNextPage, comments.PageInfo.HasNextPage
		if !wantReviews && !wantComments {
			break
		}
		if more == githubHistoryMorePages {
			pr.ActsWhole = false
			break
		}
		var raw struct {
			Repository *struct {
				PullRequest *struct {
					Reviews  *githubHistoryItemsRaw `json:"reviews"`
					Comments *githubHistoryItemsRaw `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": r.owner, "name": r.name, "number": pr.Number,
			"wantReviews": wantReviews, "reviewsAfter": nullable(reviews.PageInfo.EndCursor),
			"wantComments": wantComments, "commentsAfter": nullable(comments.PageInfo.EndCursor)}
		if _, err := r.client.GraphQL(r.ctx, githubPullHistoryMoreQuery, vars, &raw); err != nil {
			return PRHistoryPR{}, err
		}
		if raw.Repository == nil || raw.Repository.PullRequest == nil {
			return PRHistoryPR{}, fmt.Errorf("GitHub answered no pull request %s#%d", r.repo.Project(), pr.Number)
		}
		reviews, comments = githubHistoryItemsRaw{}, githubHistoryItemsRaw{}
		if got := raw.Repository.PullRequest.Reviews; wantReviews && got != nil {
			reviews = *got
		}
		if got := raw.Repository.PullRequest.Comments; wantComments && got != nil {
			comments = *got
		}
	}
	pr.Acts = earliestActs(acts)
	return pr, nil
}

// nullable is s, or nil for an empty cursor.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// githubHistoryActs are a list's acts: a submitted review is an approval
// when it approves and a comment otherwise (a review's line comments come
// with it); a pending review, a dismissed one, a bot's and a deleted
// account's are none.
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
