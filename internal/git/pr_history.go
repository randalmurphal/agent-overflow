package git

import (
	"context"
	"errors"
	"slices"
	"time"
)

// The pull request history of a repository (docs/architecture/forge-transport.md#pull-request-history):
// each pull request updated inside a window, with who approved and
// commented on it, read a bounded part at a time newest first and then
// kept current. The reader owns its walk, which the caller stores
// whole and hands back to the next read.
//
// Each forge reads two kinds of stream. A list of pull requests by update
// time (listWalk) reads from the newest down to the newest it read last
// time, and the rest joins one gap it walks on newest first. An act
// stream (GitLab's events feeds, GitHub's review comments) reads its new
// acts the same way and walks its gap back in bands of whole days
// (actGap), never past where the list reached, so the pull requests its
// acts name are mostly read already. A stream's reach is the top of its
// gap, the time back to which it has read everything; the repository's
// is the stream that reached least.

// PRHistoryRequests bounds the requests one read makes, nested and
// follow-up pages included, except that a GitLab feed's band of one day
// is read whole (gitlabEventsMaxPages). A read that stops at the bound
// leaves the rest in its walk (PRHistory.Owed).
const PRHistoryRequests = 30

// PRHistoryInput is what a read starts from.
type PRHistoryInput struct {
	// Walk is the walk the last read returned; empty before the first.
	Walk []byte
	// WindowStart is the oldest update the history keeps.
	WindowStart time.Time
	// Known reports which of numbers the caller's store holds of this
	// repository, so an act that names a pull request neither it nor the
	// read holds fetches that pull request.
	Known func(numbers []int64) (map[int64]bool, error)
}

// PRHistory is what a read found.
type PRHistory struct {
	// OwnHead is the repository's head identity, as a pull request's
	// HeadRepo names a head in it: GitHub's repository node id, GitLab's
	// project id.
	OwnHead string
	// Parent is the repository the forge says it is a fork of; nil when
	// it is none.
	Parent *RepoReference
	PRs    []PRHistoryPR
	// Acts are an act stream's approvals and comments, to add to the
	// stored ones. Each names a pull request in PRs or one Known holds:
	// a stream moves past an act only once its pull request is.
	Acts []PRHistoryAct
	// Walk is the read's walk; nil when the read moved nothing.
	Walk []byte
	// Reach is the forge time back to which every update is read; zero
	// is back to the window's start. NoReach is a read whose stream read
	// oldest first (GitHub's review comments) has not reached the
	// present, so its reach holds for nothing.
	Reach   time.Time
	NoReach bool
	// Owed reports that the walk has work left: a gap, follow-up pages,
	// or a stream that has not reached the present.
	Owed bool
}

// PRHistoryPR is one pull request. State is open, draft, merged or
// closed. HeadRepo is its head repository's identity (PRHistory.OwnHead
// of that repository), empty when the forge no longer has it.
type PRHistoryPR struct {
	Number                               int64
	URL, Title, HeadRef, HeadRepo, State string
	Author                               PRListAuthor
	Created, Updated, Merged, Closed     time.Time
	// Acts are its approvals and comments, each person's earliest of each
	// kind; ActsWhole when they are all it has.
	Acts      []PRHistoryAct
	ActsWhole bool
}

// PRHistoryAct is one person's approval or comment on pull request
// Number. Person.Name is empty when the forge named only the login.
type PRHistoryAct struct {
	Number   int64
	Approved bool
	Person   PRListAuthor
	At       time.Time
}

// ReadPRHistory reads the next part of repo's pull request history, at
// most PRHistoryRequests requests. A read that fails answers what it read
// before the failure, with the walk moved to it, beside its error.
func (c *Core) ReadPRHistory(ctx context.Context, repo RepoReference, in PRHistoryInput) (PRHistory, error) {
	if err := repo.Validate(); err != nil {
		return PRHistory{}, err
	}
	return c.ForgeByID(repo.Forge).ReadPRHistory(ctx, repo, in)
}

func (nullForge) ReadPRHistory(context.Context, RepoReference, PRHistoryInput) (PRHistory, error) {
	return PRHistory{}, ErrUnsupportedForge
}

// historyBudget is the requests a read has left.
type historyBudget struct{ left int }

// take spends one request; false when none is left.
func (b *historyBudget) take() bool {
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

// errHistoryBudget stops a part of a read that ran out of requests; the
// read stores what it read before, and the walk holds the rest.
var errHistoryBudget = errors.New("the read used its requests")

// listWalk is where a list of pull requests by update time stands. Top
// is the newest update read (Unix ms): each read first reads from the
// newest down to it. Gap is a stretch not read yet, from GapHi, the
// oldest update its walk has read (0 before any), down to GapLo, below
// which everything is read or outside the window; the walk continues at
// GapCursor ("" at the newest). The first read is a gap down to the
// window's start. A read that cannot reach Top within its requests
// leaves the rest as the gap, joined with any gap still open. Moves in
// this order only go up, so a cursor never skips a pull request: one
// updated since is above it, which the next read's first pages read.
type listWalk struct {
	Top       int64  `json:"top,omitempty"`
	Gap       bool   `json:"gap,omitempty"`
	GapCursor string `json:"gapCursor,omitempty"`
	GapHi     int64  `json:"gapHi,omitempty"`
	GapLo     int64  `json:"gapLo,omitempty"`
}

// listPage is a page of a list as a walk reads it: the update time of
// each pull request it kept, the cursor past it, whether more follow, and
// whether it held one updated before the cutoff, below which the walk
// has read everything.
type listPage struct {
	updated []int64
	cursor  string
	more    bool
	below   bool
}

// listPager reads the page after cursor, keeping only the pull requests
// updated at or after cutoff, the only ones a walk reads.
type listPager func(cursor string, cutoff int64) (listPage, error)

// head reads from the newest down to Top; what the budget leaves unread
// joins the gap. The first read opens the gap down to the window's start.
func (w *listWalk) head(b *historyBudget, floor int64, page listPager) error {
	if w.Top == 0 && !w.Gap {
		*w = listWalk{Gap: true, GapLo: floor}
		return nil
	}
	if w.Top == 0 {
		return nil
	}
	var cursor string
	newest, oldest, reached := int64(0), int64(0), false
	cutoff := max(w.Top, floor)
	for !reached && b.take() {
		p, err := page(cursor, cutoff)
		if err != nil {
			return err
		}
		for _, at := range p.updated {
			newest, oldest = max(newest, at), minNonZero(oldest, at)
		}
		reached = !p.more || p.below
		cursor = p.cursor
	}
	if !reached && oldest > 0 {
		lo := w.Top
		if w.Gap {
			lo = w.GapLo
		}
		w.Gap, w.GapCursor, w.GapHi, w.GapLo = true, cursor, oldest, lo
	}
	w.Top = max(w.Top, newest)
	return nil
}

// walk walks the gap on with the budget's requests.
func (w *listWalk) walk(b *historyBudget, floor int64, page listPager) error {
	for w.Gap && b.take() {
		p, err := page(w.GapCursor, max(w.GapLo, floor))
		if err != nil {
			return err
		}
		for _, at := range p.updated {
			w.Top = max(w.Top, at)
			w.GapHi = minNonZero(w.GapHi, at)
		}
		w.GapCursor = p.cursor
		if !p.more || p.below {
			w.Gap, w.GapCursor, w.GapHi, w.GapLo = false, "", 0, 0
			w.Top = max(w.Top, floor)
		}
	}
	return nil
}

// reach is the time back to which the list has read every update: the
// top of its gap, 0 when it has none.
func (w listWalk) reach() int64 {
	if w.Gap {
		return w.GapHi
	}
	return 0
}

// actGap is the part of an act stream not read yet: everything from Hi
// down to Lo (Unix ms), walked back in bands of Width days, newest first.
// Open is false once the walk reached the window's start.
type actGap struct {
	Open  bool  `json:"open,omitempty"`
	Hi    int64 `json:"hi,omitempty"`
	Lo    int64 `json:"lo,omitempty"`
	Width int   `json:"width,omitempty"`
}

// Band widths of an act stream's walk back, in days: a band starts at
// actBandFirstDays, doubles after one that held a page or less, up to
// actBandMaxDays, and halves after one past actBandManyPages pages, down
// to a day.
const (
	actBandFirstDays = 7
	actBandMaxDays   = 56
	actBandManyPages = 20
)

const day = 24 * time.Hour

// open makes the gap everything from hi down to lo, joined with what is
// still open below.
func (g *actGap) open(hi, lo int64) {
	if g.Open {
		lo = g.Lo
	}
	*g = actGap{Open: true, Hi: hi, Lo: lo, Width: cmpOr(g.Width, actBandFirstDays)}
}

// clip drops what of the gap fell out of the window, whose start moves
// on with each read, and closes the gap when nothing of it is left.
func (g *actGap) clip(windowStart int64) {
	if !g.Open || g.Lo >= windowStart {
		return
	}
	g.Lo = windowStart
	if g.Hi <= g.Lo {
		*g = actGap{}
	}
}

// band is the next band to walk: from its lower end lo up to the gap's
// top, never below floor, where the list's walk has reached; false when
// the gap waits for the list.
func (g actGap) band(floor int64) (lo, hi int64, ok bool) {
	if !g.Open || g.Hi <= floor {
		return 0, 0, false
	}
	return max(g.Hi-int64(g.Width)*day.Milliseconds(), g.Lo, floor), g.Hi, true
}

// walked records the band down to lo, read in pages requests, and closes
// the gap when it reached the gap's end.
func (g *actGap) walked(lo int64, pages int) {
	g.Hi = lo
	switch {
	case pages <= 1:
		g.Width = min(g.Width*2, actBandMaxDays)
	case pages > actBandManyPages:
		g.Width = max(g.Width/2, 1)
	}
	if lo <= g.Lo {
		*g = actGap{}
	}
}

// narrow halves the band width after a band too large to read; false
// when it is a day already.
func (g *actGap) narrow() bool {
	if g.Width <= 1 {
		return false
	}
	g.Width /= 2
	return true
}

// reach is the time back to which the stream has read every act: the
// top of its gap, 0 when it has none.
func (g actGap) reach() int64 {
	if g.Open {
		return g.Hi
	}
	return 0
}

func cmpOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// historyPRs are the pull requests one read found, each once at its
// newest read.
type historyPRs struct {
	prs      []PRHistoryPR
	byNumber map[int64]int
}

// keep adds pr, a pull request read twice keeping its newest read.
func (h *historyPRs) keep(pr PRHistoryPR) {
	if h.byNumber == nil {
		h.byNumber = map[int64]int{}
	}
	if at, ok := h.byNumber[pr.Number]; ok {
		if !pr.Updated.Before(h.prs[at].Updated) {
			h.prs[at] = pr
		}
		return
	}
	h.byNumber[pr.Number] = len(h.prs)
	h.prs = append(h.prs, pr)
}

// resolveActs is the one rule for an act stream's acts: an act counts
// only once its pull request is in the read or the caller's store. It
// answers acts with the ones whose pull request is in neither fetched
// (fetch keeps what it finds in h), and drops the acts of a pull request
// the forge does not return, which it no longer has. A fetch that fails
// answers its error, and the caller moves past none of acts.
func resolveActs(acts []PRHistoryAct, h *historyPRs, known func([]int64) (map[int64]bool, error), fetch func(numbers []int64) error) ([]PRHistoryAct, error) {
	var named []int64
	for _, a := range acts {
		if _, ok := h.byNumber[a.Number]; !ok && !slices.Contains(named, a.Number) {
			named = append(named, a.Number)
		}
	}
	if len(named) == 0 {
		return acts, nil
	}
	held, err := known(named)
	if err != nil {
		return nil, err
	}
	named = slices.DeleteFunc(named, func(n int64) bool { return held[n] })
	if len(named) > 0 {
		if err := fetch(named); err != nil {
			return nil, err
		}
	}
	return slices.DeleteFunc(slices.Clone(acts), func(a PRHistoryAct) bool {
		_, read := h.byNumber[a.Number]
		return !read && !held[a.Number]
	}), nil
}

// nameActs fills the name of each act whose forge named only the login
// from another act or pull request of the read by the same login.
func nameActs(acts []PRHistoryAct, prs []PRHistoryPR) {
	names := map[string]string{}
	note := func(p PRListAuthor) {
		if p.Name != "" && p.Login != "" {
			names[p.Login] = p.Name
		}
	}
	for _, pr := range prs {
		note(pr.Author)
		for _, a := range pr.Acts {
			note(a.Person)
		}
	}
	for _, a := range acts {
		note(a.Person)
	}
	for i := range acts {
		if acts[i].Person.Name == "" {
			acts[i].Person.Name = names[acts[i].Person.Login]
		}
	}
}

// earliestActs keeps each person's earliest act of each kind.
func earliestActs(acts []PRHistoryAct) []PRHistoryAct {
	type key struct {
		number   int64
		approved bool
		login    string
	}
	at := map[key]int{}
	var out []PRHistoryAct
	for _, a := range acts {
		k := key{a.Number, a.Approved, a.Person.Login}
		if i, ok := at[k]; ok {
			if a.At.Before(out[i].At) {
				out[i].At = a.At
			}
			if out[i].Person.Name == "" {
				out[i].Person.Name = a.Person.Name
			}
			continue
		}
		at[k] = len(out)
		out = append(out, a)
	}
	return out
}

func minNonZero(a, b int64) int64 {
	if a == 0 || b < a {
		return b
	}
	return a
}

// unixMilli is t in Unix ms, 0 for the zero time.
func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromUnixMilli is ms as a time, the zero time for 0.
func fromUnixMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
