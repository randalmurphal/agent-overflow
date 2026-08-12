package store

import (
	"context"
	"fmt"
)

// The batched, WHOLE-TREE read behind the run map (`app_workflow_runmap.go`).
// Every other workflow read in this package answers for one run; a campaign's
// map is a whole tree at once, and asking these questions one run at a time is
// one round trip per wave for a view that repaints on every transition.
//
// They live together rather than in the per-table files because what they have
// in common is the caller's shape — a tree root — not the table: each is the
// `WHERE item_id IN (SELECT id FROM tree)` form of a read whose single-run
// sibling stays where its CRUD is. The membership subquery is
// `workItemTreeCTE` (`work_item_tree.go`), so the rows these return and the
// runs the tree scan returns cannot describe different trees — and no
// campaign's id list ever becomes a bind array. Each returns rows ordered by
// item first, so a caller can group them without sorting.
//
// `ReadWorkItemTree` is the ONE exported entry, and every piece runs inside its
// single read transaction. That is not tidiness: six independent reads are six
// WAL snapshots, and a run created between the first and the third contributes
// attempt rows that belong to no run the answer contains — a half-drawn tree
// the assembly can only discard in silence. Under WAL a read transaction pins
// its snapshot at the first statement, so one transaction is what makes the
// runs, their attempts, their units, their armed resumes and their dollars
// facts about the same instant.

// WorkItemPhaseStatus is one phase attempt narrowed to its STATE: where it is,
// when it ran, and the two facts a reader needs to explain a rest — the
// engine's park cause and whether a human touched the attempt. It carries no
// envelope, no gate trace, and no input context, because the map renders a
// whole tree's attempts and a payload column would make the answer's size a
// function of how much the models wrote.
//
// InterventionKind is the `kind` of the persisted intervention document, empty
// when the attempt has none. It is projected in SQLite rather than decoded here
// for the same reason: the note a human gate decision carries is unbounded, and
// the map wants the discriminator, never the prose.
type WorkItemPhaseStatus struct {
	ItemID           string
	PhaseID          string
	Attempt          int
	ThreadID         string
	Status           string
	ParkCause        string
	InterventionKind string
	StartedAt        int64
	EndedAt          int64
}

// WorkItemUnitStatus is one fan-out unit (or join) narrowed the same way: its
// coordinate, its status, and its timing. Envelopes, feedback, and the unit's
// branch/worktree stay out — the map draws a column per unit, not its contents.
type WorkItemUnitStatus struct {
	ItemID      string
	PhaseID     string
	Attempt     int
	UnitID      string
	UnitIndex   int
	Kind        string
	Provider    string
	ThreadID    string
	Status      string
	UnitAttempt int
	StartedAt   int64
	EndedAt     int64
}

// WorkItemTreeRead is one run tree's records — everything the map draws except
// the runs themselves, which stream through `ReadWorkItemTree`'s visitor
// because each of them carries a multi-megabyte frozen definition and these do
// not. Every field is an answer about the SAME instant: they are read inside
// one transaction.
type WorkItemTreeRead struct {
	// AutoResumes carries only the members with something armed.
	AutoResumes   []WorkItemAutoResume
	PhaseStatuses []WorkItemPhaseStatus
	UnitStatuses  []WorkItemUnitStatus
	// Usage and UsageDetail are the tree's ledger, summed and split by
	// (model, cost_source) respectively. The split is what lets the caller price
	// the rows whose wire reported no cost; this package owns no rate table.
	Usage       WorkItemUsage
	UsageDetail []UsageDetailRow
}

// ReadWorkItemTree answers for one whole run tree in ONE read transaction: the
// runs stream to visit (root first, parent before child), and everything else
// comes back in the returned record.
//
// The visitor exists for retention, not for style. Each run carries the
// definition it FROZE, capped at 4MiB apiece, so a slice of a 4096-member tree
// would hold gigabytes for the length of one fetch; streaming keeps peak
// retention at one snapshot, provided the visitor projects each blob and keeps
// no reference to it. Everything else here is envelope-free by construction, so
// it is bounded by row count alone.
//
// The caps are checked FIRST, by the run scan, which is what keeps the other
// five statements off a tree this codebase refuses to answer for. Every refusal
// is typed (`ErrWorkItemTreeTooLarge`, `ErrWorkItemTreeTooDeep`,
// `ErrWorkItemTreeCyclicLinkage`) so the caller can say which one happened.
//
// `visit` runs INSIDE the transaction, with the scan's rows still open on that
// connection: it must project what it is handed and return. A store call from
// inside it would be a second statement on a connection already streaming one.
func (s *Store) ReadWorkItemTree(
	ctx context.Context, rootID string, maxDepth, maxMembers int, visit func(WorkItemTreeRun) error,
) (WorkItemTreeRead, error) {
	if rootID == "" {
		return WorkItemTreeRead{}, fmt.Errorf("store: read work item tree: empty root id")
	}
	tx, err := s.reader().BeginTx(ctx, nil)
	if err != nil {
		return WorkItemTreeRead{}, fmt.Errorf("store: begin read work item tree %s: %w", rootID, err)
	}
	// Read-only: the read pool's connections carry query_only(1), and nothing
	// here writes. Rollback is the whole cleanup.
	defer tx.Rollback()

	if err := scanWorkItemTreeRuns(tx, rootID, maxDepth, maxMembers, visit); err != nil {
		return WorkItemTreeRead{}, err
	}
	var read WorkItemTreeRead
	if read.AutoResumes, err = listWorkItemTreeAutoResumes(tx, rootID); err != nil {
		return WorkItemTreeRead{}, err
	}
	if read.PhaseStatuses, err = listWorkItemTreePhaseStatuses(tx, rootID); err != nil {
		return WorkItemTreeRead{}, err
	}
	if read.UnitStatuses, err = listWorkItemTreeUnitStatuses(tx, rootID); err != nil {
		return WorkItemTreeRead{}, err
	}
	if read.Usage, err = queryWorkItemTreeUsage(tx, rootID); err != nil {
		return WorkItemTreeRead{}, err
	}
	if read.UsageDetail, err = queryWorkItemTreeUsageDetail(tx, rootID); err != nil {
		return WorkItemTreeRead{}, err
	}
	return read, nil
}

// listWorkItemTreeAutoResumes is ListWorkItemAutoResumes narrowed to one run
// tree. It is a read of its own rather than a column on the run scan for the
// reason `auto_resume_at` is absent from `workItemColumns` (v54): the column
// has two writers and three readers, and every listing that carried it would
// pay for a value almost every run holds as 0. A run with nothing armed is
// absent from the result, not a zero row.
func listWorkItemTreeAutoResumes(q sqlQueryer, rootID string) ([]WorkItemAutoResume, error) {
	if rootID == "" {
		return nil, fmt.Errorf("store: list work item tree auto resumes: empty root id")
	}
	rows, err := q.Query(
		workItemTreeCTE+`SELECT id, auto_resume_at FROM work_items
		 WHERE id IN (SELECT id FROM tree) AND auto_resume_at > 0
		 ORDER BY auto_resume_at ASC, id ASC`,
		rootID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list work item tree auto resumes %s: %w", rootID, err)
	}
	defer rows.Close()
	resumes := make([]WorkItemAutoResume, 0)
	for rows.Next() {
		var resume WorkItemAutoResume
		if err := rows.Scan(&resume.ItemID, &resume.At); err != nil {
			return nil, fmt.Errorf("store: list work item tree auto resumes %s: scan: %w", rootID, err)
		}
		resumes = append(resumes, resume)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list work item tree auto resumes %s: iterate: %w", rootID, err)
	}
	return resumes, nil
}

// listWorkItemTreePhaseStatuses returns every attempt of every run in the tree,
// grouped by run and then in the per-run order `ListWorkItemPhases` uses, so a
// tree read and a single-run read describe the same timeline.
func listWorkItemTreePhaseStatuses(q sqlQueryer, rootID string) ([]WorkItemPhaseStatus, error) {
	if rootID == "" {
		return nil, fmt.Errorf("store: list work item tree phase statuses: empty root id")
	}
	// NULLIF keeps `json_extract` off the empty string every attempt without an
	// intervention holds — SQLite reports malformed JSON for it, while a NULL
	// input answers NULL. CAST is what makes any `kind` a caller could have
	// written scannable as text rather than a driver-level type error.
	rows, err := q.Query(
		workItemTreeCTE+`SELECT item_id, phase_id, attempt, thread_id, status, park_cause,
		 COALESCE(CAST(json_extract(NULLIF(intervention, ''), '$.kind') AS TEXT), ''),
		 started_at, ended_at
		 FROM work_item_phases
		 WHERE item_id IN (SELECT id FROM tree)
		 ORDER BY item_id ASC, started_at ASC, phase_id ASC, attempt ASC`,
		rootID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list work item tree phase statuses %s: %w", rootID, err)
	}
	defer rows.Close()
	statuses := make([]WorkItemPhaseStatus, 0)
	for rows.Next() {
		var phase WorkItemPhaseStatus
		if err := rows.Scan(
			&phase.ItemID, &phase.PhaseID, &phase.Attempt, &phase.ThreadID,
			&phase.Status, &phase.ParkCause, &phase.InterventionKind,
			&phase.StartedAt, &phase.EndedAt,
		); err != nil {
			return nil, fmt.Errorf("store: list work item tree phase statuses %s: scan: %w", rootID, err)
		}
		statuses = append(statuses, phase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list work item tree phase statuses %s: iterate: %w", rootID, err)
	}
	return statuses, nil
}

// listWorkItemTreeUnitStatuses returns every unit row of every run in the tree,
// grouped by run and then in `ListWorkItemUnits`' order — phase, attempt,
// launch index — so a fan-out's columns read left to right in the order the
// engine expanded them.
func listWorkItemTreeUnitStatuses(q sqlQueryer, rootID string) ([]WorkItemUnitStatus, error) {
	if rootID == "" {
		return nil, fmt.Errorf("store: list work item tree unit statuses: empty root id")
	}
	rows, err := q.Query(
		workItemTreeCTE+`SELECT item_id, phase_id, attempt, unit_id, unit_index, kind, provider,
		 thread_id, status, unit_attempt, started_at, ended_at
		 FROM work_item_units
		 WHERE item_id IN (SELECT id FROM tree)
		 ORDER BY item_id ASC, phase_id ASC, attempt ASC, unit_index ASC, unit_id ASC`,
		rootID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list work item tree unit statuses %s: %w", rootID, err)
	}
	defer rows.Close()
	statuses := make([]WorkItemUnitStatus, 0)
	for rows.Next() {
		var unit WorkItemUnitStatus
		if err := rows.Scan(
			&unit.ItemID, &unit.PhaseID, &unit.Attempt, &unit.UnitID, &unit.UnitIndex,
			&unit.Kind, &unit.Provider, &unit.ThreadID, &unit.Status, &unit.UnitAttempt,
			&unit.StartedAt, &unit.EndedAt,
		); err != nil {
			return nil, fmt.Errorf("store: list work item tree unit statuses %s: scan: %w", rootID, err)
		}
		statuses = append(statuses, unit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list work item tree unit statuses %s: iterate: %w", rootID, err)
	}
	return statuses, nil
}
