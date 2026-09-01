// Package store manages SQLite persistence for threads, items, and heavy payloads.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // SQLite driver
)

// Store wraps SQLite and provides all persistence operations.
//
// Two pools back it. db is the single-connection writer: every write,
// migration, snapshot restore, checkpoint, and vacuum runs there, which
// is what lets RestoreFrom's temporary foreign_keys toggle behave as if
// it were global. The connection-scoped PRAGMAs both pools depend on
// (foreign_keys, busy_timeout, synchronous, query_only) ride the DSN so
// they survive connection recycling — see dsn.go.
//
// read is a small read-only pool so UI reads run
// against WAL snapshots instead of queuing behind streaming flush
// transactions; its connections carry query_only(1), so a mis-routed
// write fails loudly instead of contending. read is nil when the read
// pool is unavailable (:memory: databases, where a second open would be
// a different database, and non-WAL fallback mounts, where concurrent
// readers could hit SQLITE_BUSY that the single pool never produces) —
// reader() then falls back to the writer, restoring today's serialized
// behavior exactly.
type Store struct {
	db   *sql.DB
	read *sql.DB
	// readsQuiesced routes reads back to the writer pool while VACUUM
	// needs the exclusive lock — see quiesceReads.
	readsQuiesced atomic.Bool
}

// New opens (or creates) the SQLite database at the given path and runs migrations.
// Pass ":memory:" for tests.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", poolDSN(dbPath, writerConnPragmas))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: run migrations: %w", err)
	}
	if err := ensureStoreIdentity(db); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{db: db}

	// Reclaim the WAL file before anything can read from it. Every other
	// checkpoint the app runs is PASSIVE, which recycles WAL pages for
	// reuse but never shrinks the file — so a WAL that ballooned during a
	// session the process did not survive (a Windows Job Object kill, an
	// OOM, a crash) stays at its high-water mark for the rest of the
	// database's life. TRUNCATE resets it to zero bytes but needs a moment
	// with no open read transaction; boot is the one place that moment is
	// structurally free, since the read pool is not open yet and no RPC has
	// been served. Failure is logged, never fatal: an oversized WAL is a
	// disk-space problem, not a reason to refuse to start.
	bootCheckpoint, bootErr := s.TruncateCheckpoint()
	s.logCheckpoint("boot", bootCheckpoint, bootErr)

	if read, err := openReadPool(db, dbPath); err != nil {
		db.Close()
		return nil, err
	} else {
		s.read = read
	}
	return s, nil
}

// openReadPool opens the read-only pool for a file-backed WAL database,
// or returns nil (no pool, reads fall back to the writer) when the
// database is in-memory or WAL didn't take. Never returns a non-nil
// pool that hasn't served a probe query: a read pool that cannot read
// is a config error worth failing startup over.
func openReadPool(db *sql.DB, dbPath string) (*sql.DB, error) {
	if dbPath == ":memory:" || strings.Contains(dbPath, "mode=memory") {
		return nil, nil
	}
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return nil, fmt.Errorf("store: probe journal_mode: %w", err)
	}
	if journalMode != "wal" {
		// Rollback journaling (NFS / read-only mount fallback): a
		// writer's EXCLUSIVE lock would surface as reader busy errors
		// the single pool structurally never produces. Keep serializing.
		return nil, nil
	}
	// _pragma values apply per connection (verified against
	// modernc.org/sqlite v1.56.0). journal_mode is a property of the
	// database file, so read connections inherit WAL.
	read, err := sql.Open("sqlite", poolDSN(dbPath, readerConnPragmas))
	if err != nil {
		return nil, fmt.Errorf("store: open read pool: %w", err)
	}
	read.SetMaxOpenConns(4)
	read.SetMaxIdleConns(4)
	var probe int
	if err := read.QueryRow("SELECT 1").Scan(&probe); err != nil {
		read.Close()
		return nil, fmt.Errorf("store: read pool probe: %w", err)
	}
	if err := verifyConnPragmas(read, readerConnPragmas); err != nil {
		read.Close()
		return nil, err
	}
	return read, nil
}

// reader returns the pool read-only accessors run their queries on:
// the read pool when it exists and reads aren't quiesced, the writer
// otherwise. Each call independently picks its pool, which is what lets
// quiesceReads drain the read pool without coordinating with in-flight
// accessors. Most reads are single statements; the multi-statement
// reads whose parts must describe ONE WAL snapshot (SyncThreadWindow,
// ReadWorkItemTree, ThreadTitleContextItems) hold a short read-only
// BeginTx instead — quiesceReads' drain wait covers those the same way,
// with its deadline as the backstop.
func (s *Store) reader() *sql.DB {
	if s.read == nil || s.readsQuiesced.Load() {
		return s.db
	}
	return s.read
}

// quiesceReads routes new reads to the writer pool, waits for in-flight
// read-pool queries to drain, runs fn, then restores read-pool routing.
// It exists for VACUUM: the exclusive lock it needs can never be starved
// by the single writer pool, and quiescing preserves that property with
// the read pool in play — during fn, every read queues behind the writer
// exactly as it did when the store held one connection.
// readQuiesceSettleTick is both the drain poll interval and the one-tick
// pause quiesceReads takes before its first poll. See the comment there.
const readQuiesceSettleTick = 5 * time.Millisecond

func (s *Store) quiesceReads(fn func() error) error {
	if s.read == nil {
		return fn()
	}
	s.readsQuiesced.Store(true)
	defer s.readsQuiesced.Store(false)

	// One settle tick before the first poll. `readsQuiesced` routes every
	// NEW reader() call to the writer, but a goroutine that already took
	// the read pool out of reader() and has not yet issued its statement
	// is counted by neither side: it will never consult readsQuiesced
	// again, and it is not yet in Stats().InUse. Polling immediately can
	// therefore see a drained pool and hand fn the exclusive lock while
	// that statement is still on its way. The tick gives the handoff a
	// moment to become visible.
	//
	// This is DELIBERATELY a mitigation, not a fix, and the window is not
	// closed. Closing it means the pool hands out a release handle rather
	// than a bare *sql.DB, so quiescing can wait on a count it actually
	// owns — which means threading acquire/release through roughly a
	// hundred read accessors and turning reader() from a one-line selector
	// into a lifecycle. That is not worth it here, because losing the race
	// is bounded and non-corrupting: the late statement opens a read
	// transaction against the same WAL database fn is rebuilding, so
	// either it waits out fn's exclusive lock or fn waits out its read
	// mark, and whichever waits is absorbed by `busy_timeout`. Nothing
	// reads torn state, nothing is written twice, and the cost is a
	// microsecond-scale stall on a path that already runs a VACUUM.
	time.Sleep(readQuiesceSettleTick)

	// In-flight read-pool work is short — single statements, plus the
	// few bounded read-only transactions reader()'s doc lists; poll
	// until it drains. The deadline is a backstop — if something wedges,
	// fn still runs and the writer's busy_timeout takes over, matching
	// the pre-read-pool worst case.
	deadline := time.Now().Add(5 * time.Second)
	for s.read.Stats().InUse > 0 && time.Now().Before(deadline) {
		time.Sleep(readQuiesceSettleTick)
	}
	return fn()
}

// Close closes the database connections, truncating the WAL on the way
// out.
//
// Order is load-bearing. The read pool closes first because TRUNCATE
// cannot reset a WAL any connection still holds a read mark on, and the
// checkpoint has to run on the writer, which therefore closes last. A
// failed or blocked checkpoint is logged and shutdown continues: quitting
// must not depend on reclaiming disk space, and the next boot's
// checkpoint picks up whatever this one left behind.
func (s *Store) Close() error {
	var errs []error
	if s.read != nil {
		if err := s.read.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: close read pool: %w", err))
		}
	}
	closeCheckpoint, checkpointErr := s.TruncateCheckpoint()
	s.logCheckpoint("close", closeCheckpoint, checkpointErr)
	if err := s.db.Close(); err != nil {
		errs = append(errs, fmt.Errorf("store: close writer: %w", err))
	}
	return errors.Join(errs...)
}

// PassiveCheckpoint triggers a non-blocking WAL checkpoint. PASSIVE
// returns immediately without waiting for readers; any pages it can't
// reclaim stay in the WAL and the next call catches up. Safe to call
// from any goroutine. Returns the underlying error from SQLite —
// callers typically log and continue (the checkpoint is opportunistic;
// the autocheckpoint and the next idle-boundary call will retry).
//
// Why we need this on top of wal_autocheckpoint: the default autocheckpoint
// fires when the WAL crosses ~1000 pages (~4MB), but it runs synchronously
// on the next write transaction and bails when any reader transaction is
// open. In a streaming workload the writer is continuously busy and
// readers (the dashboard + active thread paging) overlap with bursts —
// the autocheckpoint window rarely opens. Calling PassiveCheckpoint
// from turn-completion (when streaming is known to be idle for the
// thread) gives the WAL a deterministic opportunity to recycle.
func (s *Store) PassiveCheckpoint() error {
	_, err := s.db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
	return err
}

// CheckpointResult is the three-column answer PRAGMA wal_checkpoint
// returns. SQLite reports a checkpoint it could not complete as a result
// ROW, not an error, so a caller that only checks err cannot tell a
// reclaimed WAL from an abandoned one.
type CheckpointResult struct {
	// Busy is true when SQLite could not get the locks the mode needed
	// and gave up (after busy_timeout). The WAL is left alone.
	Busy bool
	// WALFrames is the number of frames left in the WAL afterwards, or
	// -1 on a database that is not in WAL mode.
	WALFrames int64
	// Checkpointed is the number of frames moved into the main database.
	Checkpointed int64
}

// TruncateCheckpoint moves the whole WAL into the main database and
// truncates the WAL file to zero bytes.
//
// This is the only checkpoint mode that shrinks the file. PASSIVE
// recycles WAL pages so the file stops GROWING, which is why the hot
// paths use it, but the file itself keeps whatever high-water mark a
// burst pushed it to — a WAL that hit 300MB during a large backfill
// stays a 300MB file until something truncates it.
//
// The cost is exclusivity: TRUNCATE waits for every reader to finish
// (measured: an open read transaction costs the full busy_timeout and
// then returns Busy with nothing reclaimed), so it runs inside
// quiesceReads like VACUUM does — in-flight read-pool queries drain and
// new reads route to the writer for the duration. Callers pick the
// moment; it does not belong on a user-facing path.
func (s *Store) TruncateCheckpoint() (CheckpointResult, error) {
	var res CheckpointResult
	err := s.quiesceReads(func() error {
		var busy int64
		if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").
			Scan(&busy, &res.WALFrames, &res.Checkpointed); err != nil {
			return err
		}
		res.Busy = busy != 0
		return nil
	})
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("store: truncate checkpoint: %w", err)
	}
	return res, nil
}

// logCheckpoint reports a TruncateCheckpoint outcome at a lifecycle
// moment where the caller has decided not to fail on it. Both failure
// shapes are visible: an error, and the silent one where SQLite answered
// with busy=1 and reclaimed nothing.
func (s *Store) logCheckpoint(moment string, res CheckpointResult, err error) {
	switch {
	case err != nil:
		log.Printf("store: %s WAL checkpoint: %v", moment, err)
	case res.Busy:
		log.Printf("store: %s WAL checkpoint blocked by an open read; %d frames left in the WAL", moment, res.WALFrames)
	}
}

// Freed pages accumulate on the freelist forever (auto_vacuum is off —
// measured 3.8x slower large deletes from pointer-map maintenance), so
// space is reclaimed with a plain VACUUM at controlled moments instead.
// The two thresholds gate that: both must hold before a VACUUM is worth
// its cost (an exclusive lock for roughly a second per live GB). The
// fraction keeps a mostly-live file from being rewritten to reclaim
// scraps; the absolute floor keeps small databases from vacuuming over
// megabytes.
const (
	vacuumMinFreelistFraction = 0.2
	vacuumMinFreelistBytes    = 64 << 20
)

// VacuumIfFragmented runs VACUUM when the freelist exceeds both
// thresholds above, and reports whether it ran. Callers pick the
// moment: VACUUM takes an exclusive lock for its duration and briefly
// needs up to twice the live data size on disk, so it belongs after a
// sweep that actually deleted history, never on a user-facing path.
func (s *Store) VacuumIfFragmented() (bool, error) {
	return s.vacuumIfFragmented(vacuumMinFreelistBytes, vacuumMinFreelistFraction)
}

func (s *Store) vacuumIfFragmented(minFreeBytes int64, minFreeFraction float64) (bool, error) {
	var pageSize, pageCount, freelistCount int64
	for _, p := range []struct {
		pragma string
		dest   *int64
	}{
		{"page_size", &pageSize},
		{"page_count", &pageCount},
		{"freelist_count", &freelistCount},
	} {
		if err := s.db.QueryRow("PRAGMA " + p.pragma).Scan(p.dest); err != nil {
			return false, fmt.Errorf("store: vacuum probe %s: %w", p.pragma, err)
		}
	}
	if pageCount == 0 ||
		freelistCount*pageSize < minFreeBytes ||
		float64(freelistCount)/float64(pageCount) < minFreeFraction {
		return false, nil
	}
	if err := s.quiesceReads(func() error {
		_, err := s.db.Exec("VACUUM")
		return err
	}); err != nil {
		return false, fmt.Errorf("store: vacuum: %w", err)
	}
	return true, nil
}

// runMigrations is defined in migrate.go.

// ThreadOrigin is a workspace's git coordinates at one moment: the moment a
// thread was created. It is a historical record, not live state — the live
// branch of a checkout is Thread.Branch, which moves with the working tree.
//
// Every field is optional and empty means "not known", never "none" and never
// an error. A workspace outside a repository, a detached HEAD, a repository
// with no remote, and any thread created before migration v74 all produce
// empty values, and a consumer that cannot proceed without them has to say so
// itself rather than assume they are there.
//
// It exists so a thread can be forked or transferred later: reproducing where
// a thread grew from needs the repository, the branch and the commit, and by
// the time anyone asks, the branch has moved and the workspace may hold
// something else. Observed once, at creation, because that is the only moment
// the answer is still true.
type ThreadOrigin struct {
	Branch     string `json:"branch,omitempty"`
	RemoteURL  string `json:"remoteUrl,omitempty"`
	HeadCommit string `json:"headCommit,omitempty"`
}

// IsZero reports whether nothing at all was observed.
func (o ThreadOrigin) IsZero() bool {
	return o.Branch == "" && o.RemoteURL == "" && o.HeadCommit == ""
}

// Thread represents a conversation thread.
//
// ID is minted by internal/entityid and is unique across BACKENDS, not
// just within this database: a client attached to more than one backend
// keys its stores, its IndexedDB replica and its deep links by this
// string alone (docs/specs/remote-access.md §10).
//
// ProjectPath is not persisted on threads — ProjectID is the FK to the
// projects table — and three per-thread composer controls
// (ReasoningEffort, FastMode, ContextWindow) are persisted so two threads
// sharing a project can diverge on these axes.
type Thread struct {
	ID             string `json:"id"`
	ProjectID      string `json:"projectId"`
	ProjectPath    string `json:"projectPath"`
	Title          string `json:"title"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	WorkspacePath  string `json:"workspacePath"`
	WorktreePath   string `json:"worktreePath,omitempty"`
	Branch         string `json:"branch,omitempty"`
	PRRef          string `json:"prRef,omitempty"`
	SessionRef     string `json:"sessionRef,omitempty"`
	PendingForkRef string `json:"pendingForkRef,omitempty"`
	// PendingForkResumeAt pins the transcript cut for a lazy Claude fork:
	// the source-session leaf uuid captured when the fork was taken. The
	// fork's first session start passes it (repaired against the CLI's
	// resume filters) as --resume-session-at alongside --fork-session so
	// the cut lands where the timeline was cloned, not wherever the source
	// has grown to by first send. Empty = unpinned (idle-source lazy fork,
	// every non-fork thread). Cleared with PendingForkRef by both
	// session-ref writers.
	PendingForkResumeAt        string `json:"pendingForkResumeAt,omitempty"`
	Mode                       string `json:"mode"`
	ReasoningEffort            string `json:"reasoningEffort"`
	FastMode                   bool   `json:"fastMode"`
	ContextWindow              int    `json:"contextWindow"`
	AutoCompactStandardPercent int    `json:"autoCompactStandardPercent"`
	AutoCompactExtendedPercent int    `json:"autoCompactExtendedPercent"`
	// RuntimeMode is one of provider.RuntimeMode's three values. Kept as
	// a plain string on the struct so store/ doesn't import provider/
	// (which would create a cycle) — provider.NormalizeRuntimeMode is the
	// authoritative normalizer at the binding boundary.
	RuntimeMode        string `json:"runtimeMode"`
	DiscussionID       string `json:"discussionId,omitempty"`
	ParentThreadID     string `json:"parentThreadId,omitempty"`
	ForkedFromThreadID string `json:"forkedFromThreadId,omitempty"`
	LastTokenUsage     string `json:"lastTokenUsage,omitempty"`
	CreatedAt          int64  `json:"createdAt"`
	UpdatedAt          int64  `json:"updatedAt"`
	// LatestTurnCompletedAt is the newest completed turn timestamp for
	// sidebar read-state. Unlike UpdatedAt, it ignores metadata-only changes
	// such as session refs, title/model changes, and settings edits.
	LatestTurnCompletedAt *int64 `json:"latestTurnCompletedAt,omitempty"`
	Archived              bool   `json:"archived"`
	// LastReadAt is the Unix-ms timestamp of when the user last viewed
	// the thread. New rows are seeded with a creation-time baseline so a
	// later completion can be detected as unread even if the user switched
	// away before the first turn settled. NULL (nil) means "never tracked"
	// and is treated as read by the UI so pre-migration rows don't all show
	// as unread on first launch. Set by MarkThreadRead, stamped to zero by
	// MarkThreadUnread, and auto-refreshed when the user switches into a
	// thread.
	LastReadAt *int64 `json:"lastReadAt,omitempty"`
	// PinnedAt is the Unix-ms timestamp of when the user pinned the
	// thread. NULL (nil) = unpinned. Pinned threads sort into two manual
	// attention groups above needs-attention in the sidebar. Set by
	// PinThread; cleared by UnpinThread.
	PinnedAt *int64 `json:"pinnedAt,omitempty"`
	// PinGroup is PinGroupFront or PinGroupBack while pinned. NULL keeps
	// pre-v71 pinned rows in the front-burner group without a backfill;
	// unpinned rows always store NULL. Owned by the narrow pin mutators.
	PinGroup *int `json:"pinGroup,omitempty"`
	// WorktreeSetupState is the durable half of the per-project worktree
	// setup run this thread's worktree was cut with (migration v47):
	// "running", "failed", or "" for nothing to say — never ran, succeeded,
	// was cancelled, or the thread has since left that worktree. The
	// streaming panel state is in-memory and dies with the process; this is
	// what a restart still knows, and what the sidebar's "Setup failed" pill
	// falls back to.
	WorktreeSetupState string `json:"worktreeSetupState"`
	// ImportSource names the provider whose on-disk session this thread was
	// imported from (migration v50): "claude", "codex", or "" for a thread
	// AO created itself. Provenance, not a badge — it is what gates the
	// "Check for Provider Updates" affordance, which SessionRef cannot do
	// because every thread that has run a turn has one. Written once by the
	// import and deliberately absent from updateThreadSetSQL, so no
	// whole-row UpdateThread can rewrite it.
	ImportSource string `json:"importSource"`
	// CreatedByDevice names the screen that started this thread — the
	// durable per-browser-profile device id the connection carries
	// (transport.ClientIdentity), empty when the backend created the thread
	// itself or when the caller had no connection behind it.
	//
	// Provenance, not an audit trail: written once at creation and
	// deliberately absent from updateThreadSetSQL, like ImportSource above.
	// A column holds one answer, so re-stamping it on every mutation would
	// destroy the fact it exists to keep and still not record a history.
	CreatedByDevice string `json:"createdByDevice,omitempty"`
	// Origin is where this thread's workspace stood in git when the thread
	// was created. Write-once for the same reason and by the same mechanism
	// as the two fields above.
	Origin ThreadOrigin `json:"origin"`
	// HasActionableProposedPlan is derived for sidebar boot state. It is
	// true when the latest assistant proposed plan is completed and has
	// not been implemented yet. It is not a persisted threads column.
	HasActionableProposedPlan bool `json:"hasActionableProposedPlan"`
	// HasIncompleteTurn is derived from the newest unseen turn row: an
	// in-flight turn (completed_at=NULL) whose start the user hasn't
	// seen, or a settled stop_reason='interrupted' turn whose end the
	// user hasn't seen (boot-swept crashes land here —
	// RecoverCrashedTurns settles NULL rows as interrupted before the
	// frontend ever loads). Either way the sidebar should show
	// Interrupted, not live Working. It is not a persisted threads
	// column.
	HasIncompleteTurn bool `json:"hasIncompleteTurn"`
	// IsDraft is true when no items have been persisted for the thread.
	// Used by the sidebar to render a draft indicator and by the project
	// sort projection to exclude drafts from "last activity" so creating
	// or configuring an unsent thread does not move the project to the
	// top. It is not a persisted threads column.
	IsDraft bool `json:"isDraft"`
}

type ThreadContextSettings struct {
	Provider                   string
	Model                      string
	ProjectID                  string
	ContextWindow              int
	AutoCompactStandardPercent int
	AutoCompactExtendedPercent int
}

// ThreadWorkspaceRef is the narrow thread shape needed for workspace/worktree
// ownership checks. It deliberately includes archived threads because archived
// rows can be restored later and must not point at a removed worktree.
type ThreadWorkspaceRef struct {
	ID            string
	WorkspacePath string
	WorktreePath  string
}

// Project represents a user-defined grouping of threads rooted at a
// directory. Threads belong to a project via the project_id FK; the
// project's path is the canonical workspace root, though individual
// threads may operate in a worktree that diverges from project.path.
//
// ID carries the same cross-backend uniqueness contract as Thread.ID
// (internal/entityid). Path does not: two machines checked out at the
// same path are two projects.
type Project struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Color        string `json:"color,omitempty"`
	SortPosition int    `json:"sortPosition"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	Archived     bool   `json:"archived"`
	// RemoteURL and RootCommit are the checkout's DERIVED repository
	// identity, computed by the backend that owns the disk: the `origin`
	// remote exactly as git reports it, and the lexicographically smallest
	// root commit of HEAD. They exist so a client attached to several
	// backends can recognise the same repository checked out on two
	// machines as one project. Nothing here normalises the URL — the
	// client owns that, because it is the side doing the matching.
	//
	// Empty is a first-class value, never an error: a non-git directory, a
	// repository with no origin, an unborn HEAD, and every row written
	// before migration v79 all read as "not known".
	RemoteURL  string `json:"remoteURL,omitempty"`
	RootCommit string `json:"rootCommit,omitempty"`
}

// Item represents a persisted timeline entry.
type Item struct {
	ID          string `json:"id"`
	ThreadID    string `json:"threadId"`
	TurnIndex   int    `json:"turnIndex"`
	ItemIndex   int    `json:"itemIndex"`
	Kind        string `json:"kind"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	PayloadID   string `json:"payloadId,omitempty"`
	PayloadKind string `json:"payloadKind,omitempty"`
	PayloadMeta string `json:"payloadMeta,omitempty"`
	// PayloadPreviewSpans is the linked payload's preview_spans column:
	// a version-stamped highlight span blob (JSON, shape owned by the
	// app layer) covering the inline-diff preview patches in
	// PayloadMeta. Rides item list reads so cold-mounted diff cards
	// paint highlighted without an RPC; empty means "not computed" and
	// the frontend falls back to the highlight RPC path.
	PayloadPreviewSpans string `json:"payloadPreviewSpans,omitempty"`
	InputPayloadID      string `json:"inputPayloadId,omitempty"`
	ParentID            string `json:"parentId,omitempty"`
	IsBackground        bool   `json:"isBackground,omitempty"`
	CompletionOf        string `json:"completionOf,omitempty"`
	ToolName            string `json:"toolName,omitempty"`
	Decision            string `json:"decision,omitempty"`
	Meta                string `json:"meta,omitempty"`
	CreatedAt           int64  `json:"createdAt"`
	UpdatedAt           int64  `json:"updatedAt"`
}

// Payload represents heavy content stored for on-demand loading.
type Payload struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Meta      string `json:"meta"`
	Data      []byte `json:"data,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// PayloadMeta is the meta-only view (no data blob).
type PayloadMeta struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Meta      string `json:"meta"`
	CreatedAt int64  `json:"createdAt"`
}

// ChatBarFavorite is one starred entry in the composer model menu.
// Kind is "model" or "discussion". Provider is set only for model
// favorites; Value is the model slug or discussion definition id.
type ChatBarFavorite struct {
	Kind      string `json:"kind"`
	Provider  string `json:"provider,omitempty"`
	Value     string `json:"value"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"createdAt"`
}

// ChatModelProfile remembers the last selected chat-bar settings for a
// provider/model pair. New draft threads seed from the most recently
// updated profile, and model switches restore that model's profile.
type ChatModelProfile struct {
	Provider                   string `json:"provider"`
	Model                      string `json:"model"`
	ReasoningEffort            string `json:"reasoningEffort"`
	FastMode                   bool   `json:"fastMode"`
	ContextWindow              int    `json:"contextWindow"`
	AutoCompactStandardPercent int    `json:"autoCompactStandardPercent"`
	AutoCompactExtendedPercent int    `json:"autoCompactExtendedPercent"`
	RuntimeMode                string `json:"runtimeMode"`
	UpdatedAt                  int64  `json:"updatedAt"`
}

// -- helpers --

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}
