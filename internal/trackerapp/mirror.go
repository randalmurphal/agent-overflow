package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The mirror's sync and reads. A scope's tick (syncBoard, syncDelta,
// SyncLinked) runs the units it is asked for in a fixed order, one at a
// time, each a bounded amount of work, and returns: it starts no
// goroutine and keeps nothing after it returns but the connection it held
// (Service.holds) and its board pump's reconcile pass (boardPump.pass). A
// tick captures the connection's mirror
// root when it starts and names it in every write, so a tick that
// outlives its mirror (the connection turned off, removed, or off and on
// again) stores nothing and ends with Outcome.Ended. Ticks are safe to
// call concurrently with each other, with themselves and with any change
// to the connections: two ticks of one scope run one after the other, and
// the history units of one connection's scopes do too.

// HistoryWindow is how far back the mirror keeps closed sprints and done
// issues, and where a board's first delta walk starts.
const HistoryWindow = 180 * 24 * time.Hour

// window is the start of the history window now: the closed sprints a
// board keeps (Provider.Structure) and a Jira card names
// (jiraprovider.Options.SprintsFrom) closed at or after it.
func (s *Service) window() time.Time { return s.now().Add(-HistoryWindow) }

// MirrorStore is the mirror accessors the service uses.
type MirrorStore interface {
	GetOrCreateMirrorRoot(connectionID string) (int64, bool, error)
	GetMirrorRoot(connectionID string) (int64, bool, error)
	DetachMirror(connectionID string) (bool, error)
	FinishMirrorCleanup(ctx context.Context, pause store.ChunkPause) error
	KeepMirrorTicket(rootID int64, issueID string, session, pump int64, opened store.MirrorRead) (store.MirrorBind, bool, error)
	ActivateMirrorBind(b store.MirrorBind) bool
	EndMirrorBind(b store.MirrorBind)
	DropMirrorTicket(session, pump int64) (bool, error)
	DropMirrorTicketsOfOtherSessions(session int64) (int64, error)

	BeginMirrorRead(rootID, now int64) (store.MirrorRead, error)
	GetMirrorSync(rootID int64, boardID string, now int64) (store.MirrorSync, bool, error)
	RecordMirrorSyncAttempt(rootID int64, boardID string, a store.MirrorSyncAttempt) (store.MirrorSyncOutcome, bool, error)

	WriteMirrorRowPage(rootID int64, page store.MirrorRowPage, cursor []byte) (store.MirrorChanges, error)
	WriteMirrorRows(rootID int64, page store.MirrorRowPage) (store.MirrorChanges, error)
	UpsertMirrorCards(rootID, sweep int64, cards []store.MirrorCard, catalog store.MirrorCatalog, read store.MirrorRead) (store.MirrorChanges, error)
	ReconcileMirrorBoard(rootID int64, boardID string, lifetime int64, listed []string, complete bool, read store.MirrorRead) (store.MirrorReconcile, error)
	ReplaceMirrorStructure(rootID int64, st store.MirrorStructure) (store.MirrorStructureWrite, error)
	ReplaceMirrorHistories(rootID, sweep int64, histories []store.MirrorHistory, catalog store.MirrorCatalog, read store.MirrorRead) ([]store.MirrorHistoryResult, bool, error)
	ListMirrorHistoryDue(rootID int64, now int64, first ...string) ([]store.MirrorHistoryDue, bool, error)
	StoreMirrorTickets(rootID, sweep int64, tickets []store.MirrorTicket, catalog store.MirrorCatalog, opened, read store.MirrorRead) (store.MirrorTicketsWrite, error)

	ListMirrorBodies(rootID int64, issueIDs []string) ([]store.MirrorBody, error)
	GetMirrorBoard(rootID int64, boardID string) (store.MirrorStructure, bool, error)
	GetMirrorCatalog(rootID int64, users, statuses []string) (store.MirrorCatalog, error)
	ListMirrorCards(rootID int64, issueIDs []string) ([]store.MirrorCard, error)
	ListMirrorIssueRows(rootID int64, issueID, afterBoard string) ([]store.MirrorIssueRow, bool, error)
	ListMirrorOffBoardLinkedIssues(rootID int64, after string, polled []string) ([]string, bool, error)
	ListMirrorSection(rootID int64, boardID string, regroup store.MirrorRegroup, section store.MirrorSection, after store.MirrorRowKey) (store.MirrorSectionPage, error)
	ReadMirrorBoardView(rootID int64, boardID string, regroup store.MirrorRegroup, sections []store.MirrorSection) (store.MirrorBoardView, bool, error)
	ReadMirrorBoardPatch(rootID int64, boardID string, regroup store.MirrorRegroup, sections []store.MirrorSection, issueIDs []string,
		held store.MirrorTotalsAt) (store.MirrorBoardPatch, bool, error)
	ListMirrorIssueBoards(rootID int64, issueIDs, parentIDs []string) ([]store.MirrorIssueBoard, error)
	ReadMirrorRefreshTargets(rootID int64, cardIDs, detailIDs, boardIDs []string) (store.MirrorRefreshTargets, error)

	ReplaceMirrorWorklogs(rootID int64, authorID, fromDay, toDay string, read store.MirrorRead, logs []store.MirrorWorklog) (store.MirrorWorklogChanges, error)
	ListMirrorWorklogs(rootID int64, authorID, fromDay, toDay string) ([]store.MirrorWorklog, bool, error)
}

var (
	// ErrInvalidRequest is a mirror call refused for what it names before
	// anything is read: a scope without its board, a section that names
	// nothing.
	ErrInvalidRequest = errors.New("trackerapp: invalid mirror request")
	// ErrNotMirrored is a read of something the mirror does not hold: a
	// board whose structure was never stored, an issue it does not keep,
	// or a connection with no mirror.
	ErrNotMirrored = errors.New("trackerapp: not in the mirror")
)

// Scope is what a unit of sync works on and records its outcome under: a
// board of a connection, or with Board empty the connection's linked
// issues (the issues threads this computer owns are linked to).
type Scope struct {
	Connection string          `json:"connection"`
	Board      tracker.BoardID `json:"board,omitempty"`
}

// boardOnly refuses a scope that names no board, for the board units.
func (sc Scope) boardOnly() error {
	if sc.Connection == "" || sc.Board == "" {
		return fmt.Errorf("%w: a board unit needs a connection and a board, not %+v", ErrInvalidRequest, sc)
	}
	return nil
}

// Urgency says whether a person waits on a call. A background call fails
// at once while the tracker is limiting requests; an interactive one
// waits a few seconds for the limit to lift (trackerapi.WithInteractive)
// and so spends the budget a person's actions need.
type Urgency uint8

const (
	// Background is a call nobody waits on: a pump's tick.
	Background Urgency = iota
	// Interactive is a call a person waits on: a refresh they asked for,
	// a ticket they opened.
	Interactive
)

// Unit names a unit of sync.
type Unit string

const (
	// UnitStructure reads a board's structure.
	UnitStructure Unit = "structure"
	// UnitReconcile lists a board's issues and reads the rows it lacks.
	UnitReconcile Unit = "reconcile"
	// UnitDelta reads a board's changed rows from its cursor.
	UnitDelta Unit = "delta"
	// UnitHistory reads the history of the connection's issues that are due.
	UnitHistory Unit = "history"
	// UnitCards reads the cards of a connection's linked issues.
	UnitCards Unit = "cards"
)

// BoardDue names the units a board's tick runs. They run in the order of
// the fields; a unit not named does nothing and records nothing.
type BoardDue struct {
	Structure bool `json:"structure,omitempty"`
	Delta     bool `json:"delta,omitempty"`
	Reconcile bool `json:"reconcile,omitempty"`
	History   bool `json:"history,omitempty"`
}

// LinkedDue names the units a connection's linked-issues tick runs, in
// the order of the fields.
type LinkedDue struct {
	Cards   bool `json:"cards,omitempty"`
	History bool `json:"history,omitempty"`
}

// UnitResult is what one unit of a tick stored. A unit that fails part
// way keeps what it stored before the failure.
type UnitResult struct {
	Unit    Unit    `json:"unit"`
	Changes Changes `json:"changes"`
	// More reports a unit that stopped at its bound with work left; the
	// next tick that names it continues.
	More bool `json:"more,omitempty"`
}

// Outcome is how one tick ended.
type Outcome struct {
	// Ran are the units the tick ran, in order. When Failure is set the
	// last is the unit that failed and the units after it did not run.
	Ran []UnitResult `json:"ran"`
	// Failure is the tracker failure that stopped the tick; nil when none.
	// It is also recorded in the scope's sync state and settled as the
	// connection's problem.
	Failure *Failure `json:"failure,omitempty"`
	// RetryAt is when the tracker said its rate limit lifts; zero unless
	// Failure is FailureRateLimited.
	RetryAt time.Time `json:"retry_at,omitzero"`
	// Ended reports that the scope's mirror ended before or while the tick
	// ran: the connection is turned off or removed, or its mirror was
	// cleared. It is not a failure; the scope has nothing more to sync.
	Ended bool `json:"ended,omitempty"`
}

// Changes is the union of what every unit of the tick stored.
func (o Outcome) Changes() Changes {
	var log changeLog
	for _, r := range o.Ran {
		log.add(r.Changes)
	}
	return log.changes()
}

// More reports whether a unit the tick ran stopped at its bound.
func (o Outcome) More() bool {
	for _, r := range o.Ran {
		if r.More {
			return true
		}
	}
	return false
}

// Changes is what a unit or a resource read stored, by issue ID, each
// list sorted with no repeats. Rows and Removed are rows of the board the
// unit or read named.
type Changes struct {
	// Cards are the issues whose card was stored new or changed.
	Cards []tracker.IssueID `json:"cards,omitempty"`
	// Rows are the issues whose row on the board was stored new or changed.
	Rows []tracker.IssueID `json:"rows,omitempty"`
	// Added are the issues of Rows whose row is new on the board.
	Added []tracker.IssueID `json:"added,omitempty"`
	// Removed are the issues whose row on the board was removed.
	Removed []tracker.IssueID `json:"removed,omitempty"`
	// Catalog reports a stored user or status that changed, whichever read
	// stored it: every board of the connection shows them.
	Catalog bool `json:"catalog,omitempty"`
	// Structure reports a change to the board's structure: its columns,
	// estimate, sprints, or the users and statuses read with them (which
	// Catalog reports too).
	Structure bool `json:"structure,omitempty"`
	// History are the issues whose stored changes were replaced, or whose
	// record's status time a ticket read moved.
	History []tracker.IssueID `json:"history,omitempty"`
	// Details are the issues whose stored ticket body changed or moved to
	// another version, and Dropped those whose body left the mirror to keep
	// the open tickets' and the most recently opened.
	Details []tracker.IssueID `json:"details,omitempty"`
	Dropped []tracker.IssueID `json:"dropped,omitempty"`
	// Visibility are the issues whose record reads began or stopped
	// serving with no write of it: the last open ticket that kept one
	// closed (store DropMirrorTicket), or a ticket opened again before the
	// cleanup removed it kept it again (store KeepMirrorTicket). A view
	// that shows one as another's parent or link shows it without or with
	// its card.
	Visibility []tracker.IssueID `json:"visibility,omitempty"`
}

// Empty reports changes that name nothing.
func (c Changes) Empty() bool {
	return len(c.Cards)+len(c.Rows)+len(c.Removed)+len(c.History)+len(c.Details)+len(c.Dropped)+len(c.Visibility) == 0 &&
		!c.Catalog && !c.Structure
}

// add merges o into c: one merge. A loop over reads folds them with a
// changeLog instead.
func (c *Changes) add(o Changes) {
	c.Cards = unite(c.Cards, o.Cards)
	c.Rows = unite(c.Rows, o.Rows)
	c.Added = unite(c.Added, o.Added)
	c.Removed = unite(c.Removed, o.Removed)
	c.History = unite(c.History, o.History)
	c.Details = unite(c.Details, o.Details)
	c.Dropped = unite(c.Dropped, o.Dropped)
	c.Visibility = unite(c.Visibility, o.Visibility)
	c.Catalog = c.Catalog || o.Catalog
	c.Structure = c.Structure || o.Structure
}

// changeLog folds the Changes of a run of reads into one: add appends
// each read's IDs and changes orders them once, so a run of n IDs costs
// one sort of n. Folding with Changes.add copies and sorts everything
// gathered so far at every read, which grows with the reads times the
// IDs.
type changeLog struct{ c Changes }

func (l *changeLog) add(o Changes) {
	l.c.Cards = append(l.c.Cards, o.Cards...)
	l.c.Rows = append(l.c.Rows, o.Rows...)
	l.c.Added = append(l.c.Added, o.Added...)
	l.c.Removed = append(l.c.Removed, o.Removed...)
	l.c.History = append(l.c.History, o.History...)
	l.c.Details = append(l.c.Details, o.Details...)
	l.c.Dropped = append(l.c.Dropped, o.Dropped...)
	l.c.Visibility = append(l.c.Visibility, o.Visibility...)
	l.c.Catalog = l.c.Catalog || o.Catalog
	l.c.Structure = l.c.Structure || o.Structure
}

// changes is everything added, each list sorted without repeats.
func (l *changeLog) changes() Changes {
	c := &l.c
	c.Cards, c.Rows, c.Added, c.Removed = ordered(c.Cards), ordered(c.Rows), ordered(c.Added), ordered(c.Removed)
	c.History, c.Details, c.Dropped, c.Visibility = ordered(c.History), ordered(c.Details), ordered(c.Dropped), ordered(c.Visibility)
	return *c
}

// unite is the sorted union of a and b without repeats.
func unite(a, b []tracker.IssueID) []tracker.IssueID {
	if len(b) == 0 {
		return a
	}
	return ordered(slices.Concat(a, b))
}

// ordered is ids sorted without repeats, in place.
func ordered(ids []tracker.IssueID) []tracker.IssueID {
	slices.Sort(ids)
	return slices.Compact(ids)
}

// changesOf is a store change set in neutral IDs.
func changesOf(ch store.MirrorChanges) Changes {
	return Changes{
		Cards: issueIDs(ch.Cards), Rows: issueIDs(ch.Rows), Added: issueIDs(ch.Added), Removed: issueIDs(ch.Removed), Catalog: ch.Catalog,
	}
}

// issueIDs is ids as issue IDs, sorted without repeats; nil for none.
func issueIDs(ids []string) []tracker.IssueID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]tracker.IssueID, len(ids))
	for i, id := range ids {
		out[i] = tracker.IssueID(id)
	}
	return ordered(out)
}

// idStrings is ids as text, in order.
func idStrings[T ~string](ids []T) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}
