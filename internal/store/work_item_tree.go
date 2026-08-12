package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// The run TREE's shape, resolved in SQLite rather than walked one round trip at
// a time in Go. Two directions, both recursive CTEs over `work_items.parent_item_id`
// (§3a call linkage):
//
//   - UPWARD, from any run to the root every tree-wide fact belongs to.
//   - DOWNWARD, from the root to every run it called, transitively.
//
// The downward CTE is also what the batched run-map reads
// (`work_item_run_map.go`) and the tree spend reads (`usage_ledger.go`) narrow
// themselves with: `WHERE item_id IN (SELECT id FROM tree)` keeps the membership
// in ONE definition and keeps a whole campaign's id list out of a bind array —
// a forty-wave tree would otherwise be forty binds per read, against a limit
// (SQLITE_MAX_VARIABLE_NUMBER) nothing here checks.
//
// Both directions are BOUNDED and refuse rather than truncate. Corrupt linkage
// is reachable (the schema's CHECKs make a parent reference all-or-nothing, not
// acyclic), and a read that hangs or silently answers for half a tree is worse
// than one that says it cannot answer. `app_workflow_runmap.go` supplies the
// bounds; the engine's absolute call depth is the source of the depth one.

// The three typed refusals a tree read can answer with. They are typed because
// the app has to say WHICH one happened: each is a PERMANENT state — retrying
// cannot make a tree smaller, a chain shorter, or a cycle acyclic — and a
// caller that cannot tell them from a transient store failure either retries
// forever or reports every one of them as the same shrug.
var (
	// ErrWorkItemTreeTooLarge reports a tree with more members than the caller's
	// cap allows. A user seeing it has a campaign nothing in this codebase was
	// sized for rather than a failure.
	ErrWorkItemTreeTooLarge = errors.New("work item tree has more runs than the read allows")
	// ErrWorkItemTreeTooDeep reports linkage longer than the caller's depth cap,
	// in either direction. Past `engine.MaxCallDepth` the chain is either corrupt
	// or a recursion the engine itself would have refused to extend.
	ErrWorkItemTreeTooDeep = errors.New("work item tree linkage is deeper than the read allows")
	// ErrWorkItemTreeCyclicLinkage reports a run that is its own ancestor. The
	// schema's CHECKs make a parent reference all-or-nothing, not acyclic, so a
	// cycle is writable — and a tree containing one has no ordering in which
	// every parent precedes its children, which is the promise
	// `ReadWorkItemTree` makes to every consumer that builds the tree in one
	// pass. Answering with a violating order would be worse than refusing.
	ErrWorkItemTreeCyclicLinkage = errors.New("work item tree linkage is cyclic")
)

// WorkItemTreeRun is one member of a run tree, narrowed to what a whole-tree
// read needs: linkage, live state, timing, the definition the run FROZE, and
// its declared budget.
//
// It is a distinct type rather than a sparsely-populated WorkItem for the
// reason WorkItemNode is: the columns it drops — goal, seeds, disposition,
// digest, worktree/branch, source — would read as blank rather than absent, and
// a caller that trusted one would be wrong with no error. Dropping them is the
// point: a tree read decodes every member, and goal/digest/disposition are
// model-authored prose no map draws.
type WorkItemTreeRun struct {
	ID string
	// ProjectID is the project whose profile supplies what the run itself did
	// not declare. It rides the tree read because the budget in force for a tree
	// falls back to `reliability.per_item_budget` of the ROOT's project, so a
	// reader that has the tree but not the project cannot resolve the same
	// ceiling the engine enforces.
	ProjectID     string
	WorkflowID    string
	ParentItemID  string
	ParentPhaseID string
	ParentUnitID  string
	ParentAttempt int
	CallDepth     int
	State         string
	Reason        string
	SoftStop      bool
	StartedAt     int64
	EndedAt       int64
	// Snapshot is the frozen definition this run started against. Each member
	// carries its OWN: a definition refresh between waves is reachable, so the
	// root's snapshot cannot speak for its children.
	Snapshot json.RawMessage
	// Budget is the run's declared ceiling. Only the root's means anything — a
	// budget is enforced against the whole tree — but it is read for every
	// member because narrowing it to one row would need a second query.
	Budget json.RawMessage
}

// workItemTreeCTE walks one run tree DOWNWARD from its root through the call
// linkage (§3a). Budgets are enforced against the root across the whole tree,
// so the spend a ceiling is compared against is the sum over every run the root
// called, transitively.
//
// UNION rather than UNION ALL, and no depth column: with membership as the only
// output, set semantics terminate a cycle structurally — an id already in the
// working set is never re-expanded — so this form is safe on corrupt linkage
// without a bound. `workItemTreeDepthCTE` is the form that has to carry one.
//
// The anchor is the supplied id itself rather than a work_items lookup, so the
// root's own ledger rows are always counted. Anchoring on the row would report
// zero spend for an id with no run record — a budget that silently never fires
// instead of an error — and ledger rows are deliberately FK-free (they outlive
// the runs they attribute).
const workItemTreeCTE = `WITH RECURSIVE tree(id) AS (
    SELECT ?
    UNION
    SELECT child.id FROM work_items AS child JOIN tree
      ON child.parent_item_id = tree.id AND child.parent_item_id <> ''
)
`

// workItemTreeDepthCTE is workItemTreeCTE carrying each member's distance from
// the root, which is what lets the run listing order parents before children
// from the LINKAGE rather than from the persisted `call_depth` column — a
// column a corrupt row could make lie, and the "a parent is always seen before
// its children" promise the wire shape makes is not one to hang on data.
//
// The depth makes the tuples distinct, so UNION no longer terminates a cycle by
// itself; `?2` is the depth bound that does. It admits depths 0..bound+1, one
// past the caller's limit, so a tree that exceeded the bound is DETECTABLE
// (a member at bound+1) rather than silently trimmed at it.
const workItemTreeDepthCTE = `WITH RECURSIVE tree(id, depth) AS (
    SELECT ?1, 0
    UNION
    SELECT child.id, tree.depth + 1 FROM work_items AS child JOIN tree
      ON child.parent_item_id = tree.id AND child.parent_item_id <> ''
     WHERE tree.depth <= ?2
)
`

// WorkItemTreeRoot resolves any run to the root of the tree it belongs to.
//
// The named run must EXIST — the anchor is its row, so an unknown id is
// `sql.ErrNoRows` rather than a one-run tree, because there is no tree to
// answer for. A run whose named parent's row is GONE resolves to itself: the
// returned node's ParentItemID is then non-empty, which is how the caller
// tells an orphan from a true root and can log it.
//
// maxDepth bounds the ancestor chain; a chain that reaches it — including one
// a linkage cycle made infinite — is refused with ErrWorkItemTreeTooDeep rather
// than answered from a truncated walk.
func (s *Store) WorkItemTreeRoot(itemID string, maxDepth int) (WorkItemNode, error) {
	if itemID == "" {
		return WorkItemNode{}, fmt.Errorf("store: work item tree root: empty work item id")
	}
	if maxDepth <= 0 {
		return WorkItemNode{}, fmt.Errorf("store: work item tree root %s: max depth must be positive, got %d", itemID, maxDepth)
	}
	var node WorkItemNode
	var depth int
	err := s.reader().QueryRow(
		`WITH RECURSIVE ancestors(id, parent_item_id, call_depth, depth) AS (
		     SELECT id, parent_item_id, call_depth, 0 FROM work_items WHERE id = ?1
		     UNION ALL
		     SELECT parent.id, parent.parent_item_id, parent.call_depth, ancestors.depth + 1
		       FROM work_items AS parent JOIN ancestors
		         ON parent.id = ancestors.parent_item_id
		      WHERE ancestors.parent_item_id <> '' AND ancestors.depth <= ?2
		 )
		 SELECT id, parent_item_id, call_depth, depth FROM ancestors
		  ORDER BY depth DESC LIMIT 1`,
		itemID, maxDepth,
	).Scan(&node.ID, &node.ParentItemID, &node.CallDepth, &depth)
	if err != nil {
		return WorkItemNode{}, fmt.Errorf("store: work item tree root %s: %w", itemID, err)
	}
	if depth > maxDepth {
		return WorkItemNode{}, fmt.Errorf(
			"store: work item tree root %s: %w (cap %d)", itemID, ErrWorkItemTreeTooDeep, maxDepth)
	}
	return node, nil
}

// scanWorkItemTreeRuns hands the root and every run below it to visit, root
// first and then by distance from the root, so a consumer builds the tree in
// one pass and a parent is always seen before its children. Ties within a level
// are broken by creation order, which is the order the calls were made in.
//
// It STREAMS rather than returning a slice, and that is the whole reason it has
// this shape: every member carries its frozen definition, which is capped at
// `engine.MaxSnapshotBytes` (4MiB) apiece. A slice of 4096 of those would
// retain gigabytes for the duration of one fetch, on a read that repaints. Peak
// retention here is ONE snapshot: each row's blob is scanned into a fresh
// string, handed to visit, and unreferenced before the next row is scanned —
// which holds only while visit keeps no reference to `Snapshot`, and every
// caller projects it and drops it.
//
// The root's row must exist — the caller resolved it through WorkItemTreeRoot,
// so an empty answer means the tree was deleted mid-read and is reported rather
// than returned as an empty map of a campaign that plainly ran.
//
// Every bound refuses loudly and typed: maxDepth is the linkage bound (see
// workItemTreeDepthCTE), maxMembers caps the answer's SIZE — the one a
// legitimate-but-enormous campaign reaches first — and a cycle is refused
// outright. Truncating any of them would hand a reader a map that silently
// omits, or mis-links, the part they were looking for.
func scanWorkItemTreeRuns(
	q sqlQueryer, rootID string, maxDepth, maxMembers int, visit func(WorkItemTreeRun) error,
) error {
	if rootID == "" {
		return fmt.Errorf("store: scan work item tree runs: empty root id")
	}
	if maxDepth <= 0 || maxMembers <= 0 {
		return fmt.Errorf(
			"store: scan work item tree runs %s: max depth and max members must be positive, got %d and %d",
			rootID, maxDepth, maxMembers)
	}
	if visit == nil {
		return fmt.Errorf("store: scan work item tree runs %s: nil visitor", rootID)
	}
	// MIN(depth) collapses the several depths a cycle stamps on one id back to
	// one row per run, so corrupt linkage costs recursion inside the bound and
	// never duplicates a run in the answer.
	rows, err := q.Query(
		workItemTreeDepthCTE+`SELECT w.id, w.project_id, w.workflow_id, w.parent_item_id, w.parent_phase_id,
		 w.parent_unit_id, w.parent_attempt, w.call_depth, w.state, w.reason, w.soft_stop,
		 w.started_at, w.ended_at, w.snapshot, w.budget, MIN(tree.depth) AS tree_depth
		 FROM tree JOIN work_items AS w ON w.id = tree.id
		 GROUP BY tree.id
		 ORDER BY tree_depth ASC, w.created_at ASC, w.id ASC
		 LIMIT ?3`,
		rootID, maxDepth, maxMembers+1,
	)
	if err != nil {
		return fmt.Errorf("store: scan work item tree runs %s: %w", rootID, err)
	}
	defer rows.Close()
	// The ordering promise is CHECKED, not assumed: `seen` is every id already
	// handed over, and `awaited` remembers the first member whose parent had not
	// arrived yet. In an acyclic tree a parent's minimum depth is always one less
	// than its child's, so a parent arriving after its child means the linkage
	// closes a cycle — the one shape MIN(depth) cannot order.
	seen := make(map[string]struct{})
	awaited := make(map[string]string)
	members := 0
	for rows.Next() {
		var run WorkItemTreeRun
		var snapshot, budget string
		var softStop, treeDepth int
		if err := rows.Scan(
			&run.ID, &run.ProjectID, &run.WorkflowID, &run.ParentItemID, &run.ParentPhaseID,
			&run.ParentUnitID, &run.ParentAttempt, &run.CallDepth, &run.State, &run.Reason,
			&softStop, &run.StartedAt, &run.EndedAt, &snapshot, &budget, &treeDepth,
		); err != nil {
			return fmt.Errorf("store: scan work item tree runs %s: scan: %w", rootID, err)
		}
		if treeDepth > maxDepth {
			return fmt.Errorf(
				"store: scan work item tree runs %s: %w (cap %d)", rootID, ErrWorkItemTreeTooDeep, maxDepth)
		}
		members++
		if members > maxMembers {
			return fmt.Errorf(
				"store: scan work item tree runs %s: %w (cap %d)", rootID, ErrWorkItemTreeTooLarge, maxMembers)
		}
		if child, ok := awaited[run.ID]; ok {
			return fmt.Errorf(
				"store: scan work item tree runs %s: %w (%s is below its own descendant %s)",
				rootID, ErrWorkItemTreeCyclicLinkage, run.ID, child)
		}
		// A run that names ITSELF as its parent is the degenerate cycle, and the
		// one the "arrives later" check cannot see: the row is its own parent and
		// arrives exactly once. A consumer building a tree from it would nest the
		// run under itself forever.
		if run.ParentItemID == run.ID {
			return fmt.Errorf(
				"store: scan work item tree runs %s: %w (%s is its own parent)",
				rootID, ErrWorkItemTreeCyclicLinkage, run.ID)
		}
		if run.ParentItemID != "" {
			if _, ok := seen[run.ParentItemID]; !ok {
				// A parent that never arrives is an ORPHANED reference, which is a
				// legitimate answer (the parent row was deleted); only one that
				// arrives later is a cycle.
				if _, pending := awaited[run.ParentItemID]; !pending {
					awaited[run.ParentItemID] = run.ID
				}
			}
		}
		seen[run.ID] = struct{}{}
		run.SoftStop = softStop != 0
		run.Snapshot = json.RawMessage(snapshot)
		run.Budget = json.RawMessage(budget)
		if err := visit(run); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: scan work item tree runs %s: iterate: %w", rootID, err)
	}
	if members == 0 {
		// Wrapped as ErrNoRows because that is what it IS: the tree was deleted
		// between the caller's root resolution and this scan, which is the same
		// answer — and the same permanent refusal — as naming a run that never
		// existed.
		return fmt.Errorf("store: scan work item tree runs %s: root run is absent: %w", rootID, sql.ErrNoRows)
	}
	return nil
}
