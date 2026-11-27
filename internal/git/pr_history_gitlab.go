package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
)

// A GitLab project's history is three streams, each read up to
// PRHistoryPages pages of 100 a read: the merge request list by update
// time, and the project's approved and commented events. An approval does
// not reliably move a merge request's update time, so approvals and
// comments come from the events, each by its own time, and are added to
// the stored ones: the feed has no event for a withdrawn approval or a
// deleted comment, so those stay.

// gitlabHistoryWalk is where a GitLab project's history read stands.
type gitlabHistoryWalk struct {
	List      gitlabListWalk   `json:"list"`
	Approved  gitlabEventsWalk `json:"approved"`
	Commented gitlabEventsWalk `json:"commented"`
}

// gitlabListWalk is the merge request list's part. Top is the newest
// update read (Unix ms): each read reads on from it, oldest update first,
// each page asking for those updated at or after the last one read, so a
// merge request updated meanwhile moves past the read and is read again
// and none is skipped. The first read walks back from the newest: Hi is
// the oldest update it has read, each page asking for those updated at or
// before it, until the window's start.
type gitlabListWalk struct {
	Top     int64 `json:"top,omitempty"`
	Walking bool  `json:"walking,omitempty"`
	Hi      int64 `json:"hi,omitempty"`
	Walked  bool  `json:"walked,omitempty"`
}

// gitlabEventsWalk is an events feed's part. Events never change, and a
// page of those created after a past date, oldest first, stays the same
// but for new events at its end, as does a page of those created before
// a past date, newest first. So the read on from the newest event read
// keeps its date (After) and page, and skips the events it read at TopAt
// (TopIDs); and the first read's walk back keeps its date (Before) and
// page, down to the window's start, Hi the oldest event it has read.
type gitlabEventsWalk struct {
	TopAt    int64   `json:"topAt,omitempty"`
	TopIDs   []int64 `json:"topIds,omitempty"`
	After    string  `json:"after,omitempty"`
	Page     int     `json:"page,omitempty"`
	Walking  bool    `json:"walking,omitempty"`
	Before   string  `json:"before,omitempty"`
	WalkPage int     `json:"walkPage,omitempty"`
	Hi       int64   `json:"hi,omitempty"`
	Walked   bool    `json:"walked,omitempty"`
}

// gitlabEventsRebasePages is how far the read on from the newest event
// lets its page grow before it starts again from the day before TopAt.
const gitlabEventsRebasePages = 20

type gitlabHistoryMRRaw struct {
	IID             int64           `json:"iid"`
	WebURL          string          `json:"web_url"`
	Title           string          `json:"title"`
	SourceBranch    string          `json:"source_branch"`
	SourceProjectID int64           `json:"source_project_id"`
	State           string          `json:"state"`
	Draft           bool            `json:"draft"`
	Author          gitlabAuthorRaw `json:"author"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
	MergedAt        string          `json:"merged_at"`
	ClosedAt        string          `json:"closed_at"`
}

type gitlabHistoryEventRaw struct {
	ID         int64           `json:"id"`
	ActionName string          `json:"action_name"`
	TargetType string          `json:"target_type"`
	TargetIID  *int64          `json:"target_iid"`
	CreatedAt  string          `json:"created_at"`
	Author     gitlabAuthorRaw `json:"author"`
	Note       *struct {
		NoteableType string `json:"noteable_type"`
		NoteableIID  *int64 `json:"noteable_iid"`
		System       bool   `json:"system"`
	} `json:"note"`
}

// ReadPRHistory reads at most PRHistoryPages pages of each stream, then
// the merge requests the events name that neither the read nor the
// caller's store holds.
func (f *gitlabForge) ReadPRHistory(ctx context.Context, repo RepoReference, in PRHistoryInput) (PRHistory, error) {
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return PRHistory{}, err
	}
	var w gitlabHistoryWalk
	if len(in.Walk) > 0 {
		if err := json.Unmarshal(in.Walk, &w); err != nil {
			return PRHistory{}, fmt.Errorf("GitLab history of %s: read the walk: %w", repo.Key(), err)
		}
	}
	settings, err := f.projectSettings(ctx, client, repo)
	if err != nil {
		return PRHistory{}, err
	}
	r := gitlabHistoryRead{ctx: ctx, client: client, repo: repo, floor: in.WindowStart.UnixMilli(), byNumber: map[int64]int{}}
	out := PRHistory{OwnHead: strconv.FormatInt(settings.ID, 10)}
	if p := settings.ForkedFrom; p != nil && p.PathWithNamespace != "" {
		namespace, name, err := SplitProjectForForge(repo.Forge, p.PathWithNamespace)
		if err != nil {
			return PRHistory{}, fmt.Errorf("GitLab history of %s: its parent: %w", repo.Key(), err)
		}
		out.Parent = &RepoReference{Forge: repo.Forge, Host: repo.Host, Namespace: namespace, Repo: name}
	}
	now := f.core.nowFn()
	moved := false
	err = func() error {
		listMoved, err := r.list(&w.List)
		moved = moved || listMoved
		if err != nil {
			return err
		}
		for _, feed := range []struct {
			action string
			walk   *gitlabEventsWalk
		}{{"approved", &w.Approved}, {"commented", &w.Commented}} {
			feedMoved, err := r.events(feed.action, feed.walk, now)
			moved = moved || feedMoved
			if err != nil {
				return err
			}
		}
		return r.missing(in.Known)
	}()
	out.PRs, out.Acts = r.prs, r.acts
	out.Walked = w.List.Walked && w.Approved.Walked && w.Commented.Walked
	if !moved {
		return out, err
	}
	walk, encodeErr := json.Marshal(w)
	if encodeErr != nil {
		return out, errors.Join(err, fmt.Errorf("encode the history walk: %w", encodeErr))
	}
	out.Walk = walk
	// Each stream has read every update back to its Hi while it walks:
	// the project only as far as the stream that reached the least.
	var reach int64
	if w.List.Walking {
		reach = w.List.Hi
	}
	for _, feed := range []gitlabEventsWalk{w.Approved, w.Commented} {
		if feed.Walking {
			reach = max(reach, feed.Hi)
		}
	}
	out.Reach = fromUnixMilli(reach)
	return out, err
}

// gitlabHistoryRead is one read's requests and what they found.
type gitlabHistoryRead struct {
	ctx      context.Context
	client   *forgeapi.Client
	repo     RepoReference
	floor    int64
	prs      []PRHistoryPR
	byNumber map[int64]int
	acts     []PRHistoryAct
}

// list reads the merge request list's part, reporting whether it read a
// page.
func (r *gitlabHistoryRead) list(w *gitlabListWalk) (bool, error) {
	moved := false
	if w.Top == 0 && !w.Walking && !w.Walked {
		w.Walking = true
	}
	pages := 0
	for w.Walking && pages < PRHistoryPages {
		query := url.Values{"order_by": {"updated_at"}, "sort": {"desc"}}
		if w.Hi > 0 {
			query.Set("updated_before", fromUnixMilli(w.Hi).Format(time.RFC3339Nano))
		}
		page, err := r.listPage(query)
		pages++
		if err != nil {
			return moved, err
		}
		moved = true
		fresh, closed := 0, len(page) < gitlabPageSize
		for _, mr := range page {
			updated := mr.Updated.UnixMilli()
			if updated < r.floor {
				closed = true
				break
			}
			w.Top = max(w.Top, updated)
			if w.Hi == 0 || updated < w.Hi {
				w.Hi, fresh = updated, fresh+1
			}
		}
		if !closed && fresh == 0 {
			return moved, fmt.Errorf("GitLab history of %s: more than %d merge requests share the update time %s, so the read cannot move past it",
				r.repo.Key(), gitlabPageSize, fromUnixMilli(w.Hi).Format(time.RFC3339Nano))
		}
		if closed {
			w.Walking, w.Hi, w.Walked = false, 0, true
			w.Top = max(w.Top, r.floor)
		}
	}
	if w.Walking {
		return moved, nil
	}
	for pages < PRHistoryPages {
		query := url.Values{"order_by": {"updated_at"}, "sort": {"asc"}, "updated_after": {fromUnixMilli(w.Top).Format(time.RFC3339Nano)}}
		page, err := r.listPage(query)
		pages++
		if err != nil {
			return moved, err
		}
		moved = true
		fresh := 0
		for _, mr := range page {
			if updated := mr.Updated.UnixMilli(); updated > w.Top {
				w.Top, fresh = updated, fresh+1
			}
		}
		if len(page) < gitlabPageSize {
			break
		}
		if fresh == 0 {
			return moved, fmt.Errorf("GitLab history of %s: more than %d merge requests share the update time %s, so the read cannot move past it",
				r.repo.Key(), gitlabPageSize, fromUnixMilli(w.Top).Format(time.RFC3339Nano))
		}
	}
	return moved, nil
}

// listPage reads one page of the merge request list in every state and
// keeps its merge requests.
func (r *gitlabHistoryRead) listPage(query url.Values) ([]PRHistoryPR, error) {
	query.Set("state", "all")
	query.Set("per_page", strconv.Itoa(gitlabPageSize))
	var raw []gitlabHistoryMRRaw
	if _, err := r.client.JSON(r.ctx, forgeapi.Request{Path: gitlabProjectPath(r.repo.Project()) + "/merge_requests", Query: query, MaxBodyBytes: gitlabMRListMaxBody}, &raw); err != nil {
		return nil, err
	}
	out := make([]PRHistoryPR, 0, len(raw))
	for _, mr := range raw {
		pr, err := r.mergeRequest(mr)
		if err != nil {
			return out, err
		}
		r.keep(pr)
		out = append(out, pr)
	}
	return out, nil
}

// keep adds pr to the read, a merge request read twice keeping its
// newest read.
func (r *gitlabHistoryRead) keep(pr PRHistoryPR) {
	if at, ok := r.byNumber[pr.Number]; ok {
		if !pr.Updated.Before(r.prs[at].Updated) {
			r.prs[at] = pr
		}
		return
	}
	r.byNumber[pr.Number] = len(r.prs)
	r.prs = append(r.prs, pr)
}

// mergeRequest is mr as history keeps it.
func (r *gitlabHistoryRead) mergeRequest(mr gitlabHistoryMRRaw) (PRHistoryPR, error) {
	times := make([]time.Time, 4)
	for i, raw := range []string{mr.CreatedAt, mr.UpdatedAt, mr.MergedAt, mr.ClosedAt} {
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return PRHistoryPR{}, fmt.Errorf("GitLab merge request %s!%d: time %q: %w", r.repo.Project(), mr.IID, raw, err)
		}
		times[i] = t
	}
	if times[1].IsZero() || mr.IID <= 0 {
		return PRHistoryPR{}, fmt.Errorf("GitLab merge request %s!%d has no update time", r.repo.Project(), mr.IID)
	}
	state := "open"
	switch mr.State {
	case "merged":
		state = "merged"
	case "closed":
		state = "closed"
	default:
		if mr.Draft {
			state = "draft"
		}
	}
	return PRHistoryPR{
		Number: mr.IID, URL: mr.WebURL, Title: mr.Title, HeadRef: mr.SourceBranch, HeadRepo: strconv.FormatInt(mr.SourceProjectID, 10),
		State: state, Author: PRListAuthor{Login: mr.Author.Username, Name: mr.Author.Name},
		Created: times[0], Updated: times[1], Merged: times[2], Closed: times[3],
	}, nil
}

// events reads one action's feed, reporting whether it read a page.
func (r *gitlabHistoryRead) events(action string, w *gitlabEventsWalk, now time.Time) (bool, error) {
	moved := false
	if w.TopAt == 0 && w.After == "" && !w.Walking && !w.Walked {
		split := now.UTC().AddDate(0, 0, -1)
		w.After, w.Page = split.AddDate(0, 0, -1).Format(time.DateOnly), 1
		w.Walking, w.Before, w.WalkPage = true, split.AddDate(0, 0, 1).Format(time.DateOnly), 1
	}
	pages := 0
	for pages < PRHistoryPages {
		events, next, err := r.eventsPage(action, url.Values{"after": {w.After}, "sort": {"asc"}, "page": {strconv.Itoa(w.Page)}})
		pages++
		if err != nil {
			return moved, err
		}
		moved = true
		for _, ev := range events {
			at := ev.at.UnixMilli()
			if at < w.TopAt || at == w.TopAt && slices.Contains(w.TopIDs, ev.id) {
				continue
			}
			if at > w.TopAt {
				w.TopAt, w.TopIDs = at, nil
			}
			w.TopIDs = append(w.TopIDs, ev.id)
			r.acts = append(r.acts, ev.act)
		}
		if next == 0 {
			if w.Page > gitlabEventsRebasePages && w.TopAt > 0 {
				w.After, w.Page = fromUnixMilli(w.TopAt).AddDate(0, 0, -1).Format(time.DateOnly), 1
			}
			break
		}
		w.Page = next
	}
	for w.Walking && pages < PRHistoryPages {
		events, next, err := r.eventsPage(action, url.Values{"before": {w.Before}, "sort": {"desc"}, "page": {strconv.Itoa(w.WalkPage)}})
		pages++
		if err != nil {
			return moved, err
		}
		moved = true
		closed := next == 0
		for _, ev := range events {
			at := ev.at.UnixMilli()
			if at < r.floor {
				closed = true
				break
			}
			w.Hi = minNonZero(w.Hi, at)
			r.acts = append(r.acts, ev.act)
		}
		w.WalkPage = next
		if closed {
			w.Walking, w.Before, w.WalkPage, w.Hi, w.Walked = false, "", 0, 0, true
		}
	}
	return moved, nil
}

// gitlabHistoryEvent is an event that is an approval of or a comment on a
// merge request.
type gitlabHistoryEvent struct {
	id  int64
	at  time.Time
	act PRHistoryAct
}

// eventsPage reads one page of the project's events of action, answering
// those on a merge request and the next page (0 for none). GitLab drops
// the events the reader may not see after it selects a page, so a page
// can come back short and still name a next.
func (r *gitlabHistoryRead) eventsPage(action string, query url.Values) ([]gitlabHistoryEvent, int, error) {
	query.Set("action", action)
	query.Set("per_page", strconv.Itoa(gitlabPageSize))
	resp, err := r.client.Do(r.ctx, forgeapi.Request{Path: gitlabProjectPath(r.repo.Project()) + "/events", Query: query, MaxBodyBytes: gitlabEventsMaxBody})
	if err != nil {
		return nil, 0, err
	}
	var raw []gitlabHistoryEventRaw
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, 0, fmt.Errorf("GitLab %s events of %s: decode response: %w", action, r.repo.Project(), err)
	}
	page, _ := strconv.Atoi(query.Get("page"))
	next := page
	if !advanceGitLabPage(resp, &next) {
		next = 0
	}
	var out []gitlabHistoryEvent
	for _, ev := range raw {
		at, err := time.Parse(time.RFC3339, ev.CreatedAt)
		if err != nil {
			return nil, 0, fmt.Errorf("GitLab %s event %d of %s: time %q: %w", action, ev.ID, r.repo.Project(), ev.CreatedAt, err)
		}
		var number int64
		switch {
		case ev.ActionName == "approved" && ev.TargetType == "MergeRequest" && ev.TargetIID != nil:
			number = *ev.TargetIID
		case strings.HasPrefix(ev.ActionName, "commented") && ev.Note != nil && ev.Note.NoteableType == "MergeRequest" && ev.Note.NoteableIID != nil && !ev.Note.System:
			number = *ev.Note.NoteableIID
		default:
			continue
		}
		if ev.Author.Username == "" {
			continue
		}
		out = append(out, gitlabHistoryEvent{id: ev.ID, at: at, act: PRHistoryAct{
			Number: number, Approved: action == "approved", Person: PRListAuthor{Login: ev.Author.Username, Name: ev.Author.Name}, At: at,
		}})
	}
	return out, next, nil
}

// missing fetches the merge requests the read's events name that neither
// the read nor known holds, 100 a request.
func (r *gitlabHistoryRead) missing(known func([]int64) (map[int64]bool, error)) error {
	var named []int64
	for _, a := range r.acts {
		if _, ok := r.byNumber[a.Number]; !ok && !slices.Contains(named, a.Number) {
			named = append(named, a.Number)
		}
	}
	if len(named) == 0 {
		return nil
	}
	held, err := known(named)
	if err != nil {
		return err
	}
	named = slices.DeleteFunc(named, func(n int64) bool { return held[n] })
	for chunk := range slices.Chunk(named, gitlabPageSize) {
		query := url.Values{}
		for _, n := range chunk {
			query.Add("iids[]", strconv.FormatInt(n, 10))
		}
		if _, err := r.listPage(query); err != nil {
			return err
		}
	}
	r.acts = earliestActs(r.acts)
	return nil
}
