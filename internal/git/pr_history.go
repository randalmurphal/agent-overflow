package git

import (
	"cmp"
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
// A list of pull requests by update time (listWalk) reads from the newest
// down to the newest it read last time, and the rest joins one gap it
// walks on newest first. GitHub reads each pull request's acts with it;
// GitLab reads its acts from two events feeds, each an act stream that
// reads its new acts from the newest and walks its gap back in bands of
// whole days (actGap), never past where the list reached, so the pull
// requests its acts name are mostly read already. A stream's reach is
// the time back to which it has proved it read everything; the
// repository has one only when every stream has, the least of them.
//
// Acts are add-only: a read adds approvals and comments, each person's
// earliest of each kind, and never removes one, so a withdrawn approval
// or a deleted comment stays. The walk moves only with what a read
// returns, which the caller stores in one write.

// PRHistoryRequests is the requests one read may start work with, nested
// and follow-up pages included: a unit of work starts only while one is
// left, and only a started minimum unit, a GitLab feed's band of one day,
// may finish past it (gitlabHistoryMaxRequests). A read that stops at the
// bound leaves the rest in its walk (PRHistory.Owed).
const PRHistoryRequests = 30

// PRHistoryInput is what a read starts from.
type PRHistoryInput struct {
	// Walk is the walk the last read returned; empty before the first.
	Walk []byte
	// WindowStart is the oldest update the history keeps.
	WindowStart time.Time
	// Stored answers which of numbers the caller's store holds of this
	// repository, with what it holds of each: a GitHub pull request whose
	// update time is unchanged costs no follow-up page, and a GitLab act
	// that names one neither it nor the read holds fetches it.
	Stored func(numbers []int64) (map[int64]StoredPR, error)
}

// StoredPR is what the caller's store holds of a pull request: its update
// time and when a read last saw it (PRHistoryPR.Seen).
type StoredPR struct {
	Updated, Seen time.Time
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
	// Viewer is the login of the account whose token read the
	// repository; empty when the read did not learn it.
	Viewer string
	PRs    []PRHistoryPR
	// Acts are the approvals and comments the read found, each person's
	// earliest of each kind, to add to the stored ones. Each names a pull
	// request in PRs or one the store holds.
	Acts []PRHistoryAct
	// Walk is the read's walk; nil when the read moved nothing.
	Walk []byte
	// Reach is the forge time back to which every stream has read every
	// update, when HasReach; the zero time is back to the window's start.
	// A repository has no reach until every stream has proved one.
	Reach    time.Time
	HasReach bool
	// Owed reports that the walk has work left: a gap or follow-up pages.
	Owed bool
	// ActsOwed reports that the read left acts of a pull request it read
	// unread (follow-up pages): the acts inside Reach are not all read
	// until a read reports none. A forge whose acts are streams of their
	// own, which Reach counts, never sets it.
	ActsOwed bool
}

// PRHistoryPR is one pull request. State is open, draft, merged or
// closed. HeadRepo is its head repository's identity (PRHistory.OwnHead
// of that repository), empty when the forge no longer has it. Seen is
// this computer's time when the request that read it started.
type PRHistoryPR struct {
	Number                               int64
	URL, Title, HeadRef, HeadRepo, State string
	Author                               PRListAuthor
	Created, Updated, Merged, Closed     time.Time
	Seen                                 time.Time
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

// take spends one request to start or go on with a unit of work; false
// when none is left.
func (b *historyBudget) take() bool {
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

// spend spends one request of a minimum unit already started, which
// finishes past the budget.
func (b *historyBudget) spend() bool {
	b.left--
	return true
}

// part runs fn with half the requests b has left, rounded up, and
// charges b what fn spent.
func (b *historyBudget) part(fn func(*historyBudget) error) error {
	half := &historyBudget{left: (b.left + 1) / 2}
	given := half.left
	err := fn(half)
	b.left -= given - half.left
	return err
}

// errHistoryBudget stops a read that ran out of requests; the read stores
// what it read before, and the walk holds the rest.
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
// top of its gap, 0 when it has none; false until a page of the walk has
// landed.
func (w listWalk) reach() (int64, bool) {
	if w.Gap {
		return w.GapHi, w.GapHi > 0
	}
	return 0, w.Top > 0
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
	*g = actGap{Open: true, Hi: hi, Lo: lo, Width: cmp.Or(g.Width, actBandFirstDays)}
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

// streamReach is one stream's reach: the time (Unix ms) back to which it
// has read everything, 0 for the window's start, when ok.
type streamReach struct {
	at int64
	ok bool
}

// setReach gives out the repository's reach of its streams': the one
// that reached least, and none unless every stream has one.
func setReach(out *PRHistory, streams ...streamReach) {
	at := int64(0)
	for _, s := range streams {
		if !s.ok {
			return
		}
		at = max(at, s.at)
	}
	out.Reach, out.HasReach = fromUnixMilli(at), true
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
func resolveActs(acts []PRHistoryAct, h *historyPRs, stored func([]int64) (map[int64]StoredPR, error), fetch func(numbers []int64) error) ([]PRHistoryAct, error) {
	var named []int64
	for _, a := range acts {
		if _, ok := h.byNumber[a.Number]; !ok && !slices.Contains(named, a.Number) {
			named = append(named, a.Number)
		}
	}
	if len(named) == 0 {
		return acts, nil
	}
	have, err := stored(named)
	if err != nil {
		return nil, err
	}
	held := func(n int64) bool { _, ok := have[n]; return ok }
	named = slices.DeleteFunc(named, held)
	if len(named) > 0 {
		if err := fetch(named); err != nil {
			return nil, err
		}
	}
	return slices.DeleteFunc(slices.Clone(acts), func(a PRHistoryAct) bool {
		_, read := h.byNumber[a.Number]
		return !read && !held(a.Number)
	}), nil
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
