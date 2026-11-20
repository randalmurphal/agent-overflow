package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The mirror's sync and reads. A scope's tick (SyncBoard, SyncLinked) runs
// the units it is asked for in a fixed order, one at a time, each a
// bounded amount of work, and returns: it starts no goroutine and keeps
// nothing after it returns but the connection it held (Service.holds). A
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

// MirrorStore is the mirror accessors the service uses.
type MirrorStore interface {
	GetOrCreateMirrorRoot(connectionID string) (int64, bool, error)
	GetMirrorRoot(connectionID string) (int64, bool, error)
	DetachMirror(connectionID string) (bool, error)
	FinishMirrorCleanup(pause store.ChunkPause) error

	GetMirrorSync(rootID int64, boardID string) (store.MirrorSync, bool, error)
	RecordMirrorSyncAttempt(rootID int64, boardID string, a store.MirrorSyncAttempt) (bool, error)

	WriteMirrorRowPage(rootID int64, page store.MirrorRowPage, cursor []byte) (store.MirrorChanges, error)
	WriteMirrorRows(rootID int64, page store.MirrorRowPage) (store.MirrorChanges, error)
	UpsertMirrorCards(rootID, sweep int64, cards []store.MirrorCard, catalog store.MirrorCatalog, readAt int64) (store.MirrorChanges, error)
	ReconcileMirrorBoard(rootID int64, boardID string, lifetime int64, listed []string, complete bool, readAt int64) (store.MirrorReconcile, error)
	ReplaceMirrorStructure(rootID int64, st store.MirrorStructure) (store.MirrorStructureWrite, error)
	ReplaceMirrorHistories(rootID, sweep int64, histories []store.MirrorHistory) ([]store.MirrorHistoryResult, error)
	ListMirrorHistoryDue(rootID int64, after string) ([]store.MirrorHistoryDue, bool, error)
	StoreMirrorDetail(rootID, sweep int64, issueID string, openedAt, readAt int64, data []byte) (store.MirrorDetailWrite, error)
	TouchMirrorDetail(rootID int64, issueID string, openedAt int64) (store.MirrorDetailWrite, error)

	GetMirrorDetail(rootID int64, issueID string) (store.MirrorDetail, bool, error)
	GetMirrorBoard(rootID int64, boardID string) (store.MirrorStructure, bool, error)
	GetMirrorCatalog(rootID int64, users, statuses []string) (store.MirrorCatalog, error)
	ListMirrorCards(rootID int64, issueIDs []string) ([]store.MirrorCard, error)
	ListMirrorIssueRows(rootID int64, issueID, afterBoard string) ([]store.MirrorIssueRow, bool, error)
	ListMirrorOffBoardLinkedIssues(rootID int64, after string) ([]string, bool, error)
	ListMirrorSection(rootID int64, boardID string, section store.MirrorSection, after store.MirrorRowKey) (store.MirrorSectionPage, error)
	ReadMirrorBoardView(rootID int64, boardID string, sections []store.MirrorSection) (store.MirrorBoardView, bool, error)
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
	var out Changes
	for _, r := range o.Ran {
		out.add(r.Changes)
	}
	return out
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
	// Removed are the issues whose row on the board was removed.
	Removed []tracker.IssueID `json:"removed,omitempty"`
	// Catalog reports a stored user or status that changed.
	Catalog bool `json:"catalog,omitempty"`
	// Structure reports a change to the board's structure: its columns,
	// estimate, sprints, or the users and statuses read with them.
	Structure bool `json:"structure,omitempty"`
	// History are the issues whose stored changes were replaced.
	History []tracker.IssueID `json:"history,omitempty"`
	// Details are the issues whose stored detail changed, and Dropped
	// those whose detail left the mirror to keep the most recently opened.
	Details []tracker.IssueID `json:"details,omitempty"`
	Dropped []tracker.IssueID `json:"dropped,omitempty"`
}

// Empty reports changes that name nothing.
func (c Changes) Empty() bool {
	return len(c.Cards)+len(c.Rows)+len(c.Removed)+len(c.History)+len(c.Details)+len(c.Dropped) == 0 &&
		!c.Catalog && !c.Structure
}

// add merges o into c.
func (c *Changes) add(o Changes) {
	c.Cards = unite(c.Cards, o.Cards)
	c.Rows = unite(c.Rows, o.Rows)
	c.Removed = unite(c.Removed, o.Removed)
	c.History = unite(c.History, o.History)
	c.Details = unite(c.Details, o.Details)
	c.Dropped = unite(c.Dropped, o.Dropped)
	c.Catalog = c.Catalog || o.Catalog
	c.Structure = c.Structure || o.Structure
}

// unite is the sorted union of a and b without repeats.
func unite(a, b []tracker.IssueID) []tracker.IssueID {
	if len(b) == 0 {
		return a
	}
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// changesOf is a store change set in neutral IDs.
func changesOf(ch store.MirrorChanges) Changes {
	return Changes{
		Cards: issueIDs(ch.Cards), Rows: issueIDs(ch.Rows), Removed: issueIDs(ch.Removed), Catalog: ch.Catalog,
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
	slices.Sort(out)
	return slices.Compact(out)
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
