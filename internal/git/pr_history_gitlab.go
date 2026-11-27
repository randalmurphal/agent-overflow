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

// A GitLab project's history is three streams: the merge request list by
// update time, 100 a page, and the project's approved and commented
// events. An approval does not reliably move a merge request's update
// time, so approvals and comments come from the events, each by its own
// time, and are added to the stored ones: the feed has no event for a
// withdrawn approval or a deleted comment, so those stay. The commented
// feed carries every kind of note on a merge request (Note, DiffNote,
// DiscussionNote): GitLab exposes an event's note whenever its target is
// a Note.
//
// The events feed is sorted by event id and paged by number, its count
// capped at 10,000 (GitLab's MAX_COUNT_LIMIT), and filtered by whole
// days (after: created after the end of the day; before: created before
// its start, in the server's time zone). So a feed is never walked by a
// stored page number: reading on reads newest first, page numbers within
// the read only, down past both the highest event id the last read read
// and an hour before its start; walking back reads a stretch of days
// whole. A deletion or a late event moves nothing past a stored
// position.

// gitlabHistoryWalk is where a GitLab project's history read stands.
type gitlabHistoryWalk struct {
	List      listWalk       `json:"list"`
	Approved  gitlabFeedWalk `json:"approved"`
	Commented gitlabFeedWalk `json:"commented"`
}

// gitlabFeedWalk is an events feed's part. Top is this computer's time
// at the start of the last read that read the feed to its newest event,
// and TopID the highest event id read then: each read reads the feed
// newest first until it has passed both, the id in the order the feed
// gives (an imported event's id is newer than its time) and
// gitlabEventsLate before Top for an event whose row committed after a
// later one's and a forge clock behind this computer's. Gap is what is
// not read yet, walked back in bands of days, each read whole.
type gitlabFeedWalk struct {
	Top   int64  `json:"top,omitempty"`
	TopID int64  `json:"topId,omitempty"`
	Gap   actGap `json:"gap"`
}

// reach is the time back to which the feed has read every event: the top
// of its gap, 0 when it has none; false before its first read.
func (w gitlabFeedWalk) reach() streamReach {
	switch {
	case w.Top == 0:
		return streamReach{}
	case w.Gap.Open:
		return streamReach{w.Gap.Hi, true}
	}
	return streamReach{0, true}
}

// gitlabEventsLate is how far a feed's read on from its newest events
// reads below the last read's start.
const gitlabEventsLate = time.Hour

// gitlabEventsMaxPages is the most pages of 100 GitLab counts: past
// 10,000 events a feed answers no next page though more follow, so a
// band that reaches it is too wide to read.
const gitlabEventsMaxPages = 100

// gitlabHistoryMaxRequests is the most requests one GitLab read makes:
// it starts work within PRHistoryRequests, and a band of one day, the
// least a feed can filter, started with the last of them, finishes past
// it: up to gitlabEventsMaxPages pages and a fetch of the merge requests
// their events name, 100 a request.
const gitlabHistoryMaxRequests = PRHistoryRequests + gitlabEventsMaxPages - 1 + gitlabEventsMaxPages

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

// ReadPRHistory starts work within PRHistoryRequests requests, the
// project's own read among them: the list and each feed from the newest
// down to where the last read reached, then the list's gap and each
// feed's gap with what is left. A feed's gap never walks below the
// list's reach.
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
	b := &historyBudget{left: PRHistoryRequests}
	b.take()
	settings, err := f.projectSettings(ctx, client, repo)
	if err != nil {
		return PRHistory{}, err
	}
	r := gitlabHistoryRead{ctx: ctx, client: client, repo: repo, stored: in.Stored, budget: b, clock: f.core.nowFn, now: f.core.nowFn()}
	out := PRHistory{OwnHead: strconv.FormatInt(settings.ID, 10)}
	if p := settings.ForkedFrom; p != nil && p.PathWithNamespace != "" {
		namespace, name, err := SplitProjectForForge(repo.Forge, p.PathWithNamespace)
		if err != nil {
			return PRHistory{}, fmt.Errorf("GitLab history of %s: its parent: %w", repo.Key(), err)
		}
		out.Parent = &RepoReference{Forge: repo.Forge, Host: repo.Host, Namespace: namespace, Repo: name}
	}
	floor := in.WindowStart.UnixMilli()
	feeds := []struct {
		action string
		walk   *gitlabFeedWalk
	}{{"approved", &w.Approved}, {"commented", &w.Commented}}
	for _, feed := range feeds {
		feed.walk.Gap.clip(floor)
	}
	err = func() error {
		if err := w.List.head(b, floor, r.listPage); err != nil {
			return err
		}
		for _, feed := range feeds {
			if err := r.feedHead(feed.action, feed.walk, floor); err != nil {
				return err
			}
		}
		if err := b.part(func(half *historyBudget) error { return w.List.walk(half, floor, r.listPage) }); err != nil {
			return err
		}
		for _, feed := range feeds {
			reach, _ := w.List.reach()
			if err := r.feedGap(feed.action, feed.walk, max(reach, floor)); err != nil {
				return err
			}
		}
		return w.List.walk(b, floor, r.listPage)
	}()
	if errors.Is(err, errHistoryBudget) {
		err = nil
	}
	out.PRs, out.Acts = r.prs.prs, earliestActs(r.acts)
	walk, encodeErr := json.Marshal(w)
	if encodeErr != nil {
		return out, errors.Join(err, fmt.Errorf("encode the history walk: %w", encodeErr))
	}
	out.Walk = walk
	listAt, listOK := w.List.reach()
	setReach(&out, streamReach{listAt, listOK}, w.Approved.reach(), w.Commented.reach())
	out.Owed = w.List.Gap || w.Approved.Gap.Open || w.Commented.Gap.Open
	return out, err
}

// gitlabHistoryRead is one read's requests and what they found.
type gitlabHistoryRead struct {
	ctx    context.Context
	client *forgeapi.Client
	repo   RepoReference
	stored func([]int64) (map[int64]StoredPR, error)
	budget *historyBudget
	// clock is this computer's time, read at each request's start; now
	// is the read's start.
	clock func() time.Time
	now   time.Time
	prs   historyPRs
	acts  []PRHistoryAct
}

// listPage is the list's pager: the merge requests updated at or after
// cutoff, newest first, at or before the cursor's time (keyset: the
// cursor is the oldest update of the page before). More merge requests
// sharing one update time than a page holds is a failure, since no
// request can move past them.
func (r *gitlabHistoryRead) listPage(cursor string, cutoff int64) (listPage, error) {
	query := url.Values{"order_by": {"updated_at"}, "sort": {"desc"}, "updated_after": {fromUnixMilli(cutoff).Format(time.RFC3339Nano)}}
	if cursor != "" {
		query.Set("updated_before", cursor)
	}
	page, err := r.mergeRequests(query)
	if err != nil {
		return listPage{}, err
	}
	var out listPage
	for _, mr := range page {
		out.updated = append(out.updated, mr.Updated.UnixMilli())
	}
	out.more = len(page) == gitlabPageSize
	if out.more {
		oldest := page[len(page)-1].Updated
		if cursor != "" && oldest.Format(time.RFC3339Nano) == cursor {
			return out, fmt.Errorf("GitLab history of %s: more than %d merge requests share the update time %s, so the read cannot move past it",
				r.repo.Key(), gitlabPageSize, cursor)
		}
		out.cursor = oldest.Format(time.RFC3339Nano)
	}
	return out, nil
}

// mergeRequests reads one page of the merge request list in every state
// and keeps its merge requests.
func (r *gitlabHistoryRead) mergeRequests(query url.Values) ([]PRHistoryPR, error) {
	query.Set("state", "all")
	query.Set("per_page", strconv.Itoa(gitlabPageSize))
	seen := r.clock()
	var raw []gitlabHistoryMRRaw
	if _, err := r.client.JSON(r.ctx, forgeapi.Request{Path: gitlabProjectPath(r.repo.Project()) + "/merge_requests", Query: query, MaxBodyBytes: gitlabMRListMaxBody}, &raw); err != nil {
		return nil, err
	}
	out := make([]PRHistoryPR, 0, len(raw))
	for _, mr := range raw {
		pr, err := r.mergeRequest(mr, seen)
		if err != nil {
			return out, err
		}
		r.prs.keep(pr)
		out = append(out, pr)
	}
	return out, nil
}

// mergeRequest is mr as history keeps it, seen at seen.
func (r *gitlabHistoryRead) mergeRequest(mr gitlabHistoryMRRaw, seen time.Time) (PRHistoryPR, error) {
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
		Created: times[0], Updated: times[1], Merged: times[2], Closed: times[3], Seen: seen,
	}, nil
}

// feedHead reads a feed newest first, a page at a time, each page's acts
// counting once their merge requests resolve, until a page has passed
// both the highest event id the last read read and gitlabEventsLate
// before its start, or the feed ends; what the budget leaves unread joins
// the gap. The first read reads the feed's newest page, for its highest
// id, and opens the gap below it down to the window's start.
func (r *gitlabHistoryRead) feedHead(action string, w *gitlabFeedWalk, floor int64) error {
	start := r.now.UnixMilli()
	if w.Top == 0 {
		if !r.budget.take() {
			return errHistoryBudget
		}
		events, _, err := r.eventsPage(action, url.Values{"sort": {"desc"}, "page": {"1"}})
		if err != nil {
			return err
		}
		kept, err := resolveActs(eventActs(events), &r.prs, r.stored, r.fetch)
		if err != nil {
			return err
		}
		r.acts = append(r.acts, kept...)
		w.Gap.open(start, floor)
		w.Top, w.TopID = start, max(w.TopID, highestEvent(events))
		return nil
	}
	stop := w.Top - gitlabEventsLate.Milliseconds()
	// oldest is the oldest event, and highest the highest id, of the pages
	// that counted.
	oldest, highest := int64(0), w.TopID
	leave := func(err error) error {
		if errors.Is(err, errHistoryBudget) && oldest > 0 {
			w.Gap.open(oldest+gitlabEventsLate.Milliseconds(), stop)
			w.Top, w.TopID = start, highest
		}
		return err
	}
	query := url.Values{"sort": {"desc"}, "after": {gitlabEventsAfter(stop)}}
	for page := 1; ; page++ {
		if !r.budget.take() {
			return leave(errHistoryBudget)
		}
		query.Set("page", strconv.Itoa(page))
		events, next, err := r.eventsPage(action, query)
		if err != nil {
			return err
		}
		older, passed := true, false
		for _, ev := range events {
			older = older && ev.at.UnixMilli() < stop
			passed = passed || ev.id <= w.TopID
			oldest = minNonZero(oldest, ev.at.UnixMilli())
		}
		kept, err := resolveActs(eventActs(events), &r.prs, r.stored, r.fetch)
		if err != nil {
			return leave(err)
		}
		r.acts = append(r.acts, kept...)
		highest = max(highest, highestEvent(events))
		switch {
		case next == 0 || len(events) > 0 && older && passed:
			w.Top, w.TopID = start, highest
			return nil
		case page == gitlabEventsMaxPages:
			return fmt.Errorf("GitLab history of %s: more than %d %s events since %s, past what GitLab counts",
				r.repo.Key(), gitlabEventsMaxPages*gitlabPageSize, action, fromUnixMilli(stop).Format(time.RFC3339))
		}
	}
}

// eventActs are events' acts.
func eventActs(events []gitlabHistoryEvent) []PRHistoryAct {
	acts := make([]PRHistoryAct, 0, len(events))
	for _, ev := range events {
		acts = append(acts, ev.act)
	}
	return acts
}

// highestEvent is the highest id of events, 0 for none.
func highestEvent(events []gitlabHistoryEvent) int64 {
	highest := int64(0)
	for _, ev := range events {
		highest = max(highest, ev.id)
	}
	return highest
}

// feedGap walks a feed's gap back a band of days at a time, each read
// whole: its acts count, and the gap moves past it, only once every page
// is read and every merge request its acts name is read or stored. A band
// starts only while the budget has a request left. One the budget cannot
// finish is read again, half as wide, by the next read; a band of one
// day, the least a feed can filter, finishes past the budget, up to what
// GitLab counts (gitlabHistoryMaxRequests). The gap never walks below
// floor, the list's reach.
func (r *gitlabHistoryRead) feedGap(action string, w *gitlabFeedWalk, floor int64) error {
	for {
		lo, hi, ok := w.Gap.band(floor)
		if !ok {
			return nil
		}
		if !r.budget.take() {
			return errHistoryBudget
		}
		next := r.budget.take
		if w.Gap.Width <= 1 {
			next = r.budget.spend
		}
		query := url.Values{"sort": {"desc"}, "after": {gitlabEventsAfter(lo)}, "before": {gitlabEventsBefore(hi)}}
		var events []gitlabHistoryEvent
		pages := 0
		for page := 1; page != 0; pages++ {
			if pages == gitlabEventsMaxPages {
				return fmt.Errorf("GitLab history of %s: more than %d %s events in the days around %s, past what GitLab counts",
					r.repo.Key(), gitlabEventsMaxPages*gitlabPageSize, action, fromUnixMilli(hi).Format(time.DateOnly))
			}
			if pages > 0 && !next() {
				w.Gap.narrow()
				return errHistoryBudget
			}
			query.Set("page", strconv.Itoa(page))
			got, n, err := r.eventsPage(action, query)
			if err != nil {
				return err
			}
			events = append(events, got...)
			page = n
		}
		kept, err := resolveActs(eventActs(events), &r.prs, r.stored, func(numbers []int64) error { return r.fetchWith(numbers, next) })
		if err != nil {
			if errors.Is(err, errHistoryBudget) {
				w.Gap.narrow()
			}
			return err
		}
		r.acts = append(r.acts, kept...)
		w.Gap.walked(lo, pages)
	}
}

// gitlabEventsAfter is the feed's after date that keeps every event
// created at or after t (Unix ms) whatever the server's time zone: the
// filter keeps those created after the end of the day it names there,
// and no zone is more than 14 hours from UTC.
func gitlabEventsAfter(t int64) string {
	return fromUnixMilli(t).AddDate(0, 0, -2).Format(time.DateOnly)
}

// gitlabEventsBefore is the feed's before date that keeps every event
// created at or before t: the filter keeps those created before the
// start of the day it names in the server's time zone.
func gitlabEventsBefore(t int64) string {
	return fromUnixMilli(t).AddDate(0, 0, 2).Format(time.DateOnly)
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

// fetch reads the merge requests numbers names, 100 a request, on the
// read's budget.
func (r *gitlabHistoryRead) fetch(numbers []int64) error {
	return r.fetchWith(numbers, r.budget.take)
}

// fetchWith is fetch with take deciding whether a request may be made.
func (r *gitlabHistoryRead) fetchWith(numbers []int64, take func() bool) error {
	for chunk := range slices.Chunk(numbers, gitlabPageSize) {
		if !take() {
			return errHistoryBudget
		}
		query := url.Values{}
		for _, n := range chunk {
			query.Add("iids[]", strconv.FormatInt(n, 10))
		}
		if _, err := r.mergeRequests(query); err != nil {
			return err
		}
	}
	return nil
}
