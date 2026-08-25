package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usageledger"
	"agent-overflow/internal/workflow/engine"
)

// The run map's one read: a whole run TREE as metadata, in one call.
//
// The detail view (`WorkflowGetItem`) answers for one run and carries its
// evidence — envelopes, outputs, artifacts. This answers for a campaign: the
// root and every run it called, each with the skeleton of the definition it
// FROZE plus the records of what has actually happened. Nothing an agent wrote
// crosses this wire (no envelopes, no narratives, no diffs), so a forty-wave
// campaign is a few hundred small rows however much its models produced.
//
// TWO round trips answer it, whatever the tree's size. The first resolves the
// tree's root from the run the caller named (`WorkItemTreeRoot`, §5.9). The
// second is `store.ReadWorkItemTree`, which runs SIX statements — the tree's
// runs, its attempts, its units, its armed auto-resumes, and its ledger summed
// and split — inside ONE read transaction, so every number on the answer is a
// fact about one WAL snapshot rather than about six. Every statement resolves
// membership through the store's recursive CTE
// (`internal/store/work_item_tree.go`) rather than walking the linkage a round
// trip at a time in Go, and rather than round-tripping a campaign's id list
// back into a bind array.
//
// The runs STREAM through a visitor rather than arriving as a slice, and that
// is a retention decision: each run carries the definition it froze, capped at
// 4MiB apiece, so a materialised 4096-member tree would hold gigabytes for the
// length of one fetch. The projection below reads each snapshot, keeps the few
// hundred bytes it draws, and drops the blob before the next row is scanned.
//
// Each fetch decodes every member's frozen snapshot to project its skeleton,
// which is real work repeated on every repaint. That amplification is ACCEPTED
// rather than memoised: `engine/refresh.go` re-freezes a run's snapshot on
// rerun and on a definition refresh, so a skeleton cache keyed by run id would
// serve the definition the run USED to have — a map that quietly disagrees with
// the run it is drawing is worse than one that costs a decode.

// WorkflowRunMapView is the whole tree, parent-linked. Runs are ordered root
// first, then by distance from the root and creation order within a level, so a
// consumer can build the tree in one pass without sorting and a parent is
// always seen before its children. The distance is derived from the LINKAGE the
// read walked, not from the persisted `call_depth`, so the promise does not
// rest on a column a corrupt row could make lie.
type WorkflowRunMapView struct {
	// RootItemID is the tree root this map is FOR, which need not be the id the
	// caller asked about: any run in the tree resolves to the same map, so a
	// stale nav entry or a deep link to a child normalises instead of erroring.
	RootItemID string              `json:"rootItemId"`
	Runs       []WorkflowRunMapRun `json:"runs"`
	// Refusal is set — with Runs empty — when the map cannot be drawn for a
	// reason RETRYING CANNOT FIX. See WorkflowRunMapRefusal.
	Refusal *WorkflowRunMapRefusal `json:"refusal,omitempty"`
}

// WorkflowRunMapRefusal is a map this backend will never answer, said in a
// shape a client can act on: a code to branch on and a sentence to render.
//
// It rides the RESULT rather than the method's error for two reasons. The
// transport strips a method error's text for any non-loopback caller (one
// correlation id, no prose — `internal/transport/dispatcher.go`), so an error
// return literally cannot carry a user-facing sentence to a remote client;
// and every code here is PERMANENT, while the entity store's answer to a
// thrown error is a backoff ladder that re-asks the same unanswerable question
// forever. An unexpected failure — a store that will not read, a ledger group
// that will not price — stays an error, because retrying IS the right response
// to those.
type WorkflowRunMapRefusal struct {
	// Code is the machine-readable class. Every one of them is permanent: the
	// client must stop re-sourcing the key until something else changes.
	Code string `json:"code"`
	// Message is the sentence to show. It names the run, never a path or an
	// internal type.
	Message string `json:"message"`
}

// The refusal codes. They are a closed set; a new one is a wire change the
// frontend has to learn, which is the point of not spelling them as prose.
const (
	// WorkflowRunMapRefusalNotFound: the named run has no row. Ordinary rather
	// than exceptional — a stale nav entry, a deleted project's records, a deep
	// link into a run somebody discarded.
	WorkflowRunMapRefusalNotFound = "not-found"
	// WorkflowRunMapRefusalTooLarge: the tree has more members than
	// `maxWorkflowRunMapMembers`. A truncated map is the worst of the three
	// outcomes — the reader cannot see that the part they were looking for is
	// missing — so the read refuses and this says so.
	WorkflowRunMapRefusalTooLarge = "too-large"
	// WorkflowRunMapRefusalCorruptLinkage: the call linkage is deeper than
	// `engine.MaxCallDepth` or closes a cycle. The schema's CHECKs make a parent
	// reference all-or-nothing, not acyclic, so both are writable — and neither
	// can be ordered parent-before-child, which is the promise every consumer of
	// this view builds its tree on.
	WorkflowRunMapRefusalCorruptLinkage = "corrupt-linkage"
)

// WorkflowRunMapRun is one run of the tree: who it is, where it sits, what its
// frozen definition says will happen, and what has.
type WorkflowRunMapRun struct {
	ItemID     string `json:"itemId"`
	WorkflowID string `json:"workflowId"`
	// Parent linkage is present only on a called run (§3a): the caller, its call
	// phase, and the attempt of it that invoked this run. ParentUnitID narrows
	// that to one fan-out unit when the call was declared on a unit.
	//
	// The ROOT can carry one too, and that is how an ORPHAN says so: a run whose
	// named parent's row is gone resolves to itself as the tree root, with the
	// dangling reference left on the answer. A parent id naming no run in `runs`
	// is exactly that state and needs no field of its own.
	ParentItemID  string `json:"parentItemId,omitempty"`
	ParentPhaseID string `json:"parentPhaseId,omitempty"`
	ParentUnitID  string `json:"parentUnitId,omitempty"`
	ParentAttempt int    `json:"parentAttempt,omitempty"`
	CallDepth     int    `json:"callDepth,omitempty"`

	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	SoftStop     bool   `json:"softStop"`
	StartedAt    int64  `json:"startedAt,omitempty"`
	EndedAt      int64  `json:"endedAt,omitempty"`
	AutoResumeAt int64  `json:"autoResumeAt,omitempty"`

	// Skeleton is this run's OWN frozen definition, projected to what the map
	// draws. It is never the root's: a definition refresh between waves is
	// reachable, and the waves then legitimately differ.
	Skeleton []WorkflowRunMapSkeletonPhase `json:"skeleton"`
	// SkeletonMissing marks records-only mode: the frozen definition could not
	// supply phases. The map then renders this run's recorded attempts with no
	// ghosts and no loop affordance — degrading is the contract, since a run's
	// history is readable whatever its snapshot says.
	SkeletonMissing bool `json:"skeletonMissing"`
	// SkeletonError is why, when the reason was CORRUPTION rather than absence:
	// the column held bytes that would not decode as a snapshot. An absent
	// snapshot is ordinary (the run failed before its first phase entry, so it
	// never froze one) and leaves this empty. The two are one flag and one
	// sentence rather than one flag alone because a reader who cannot tell them
	// apart is told a corrupt record is normal history.
	SkeletonError string `json:"skeletonError,omitempty"`
	// TailSelfCall reports that the last skeleton phase calls this run's own
	// workflow — the edge a recursive campaign iterates on, and the one call
	// shape the map flattens into waves rather than nesting as composition.
	TailSelfCall bool `json:"tailSelfCall"`

	Phases []WorkflowRunMapPhaseAttempt `json:"phases"`
	Units  []WorkflowRunMapUnit         `json:"units"`

	// Spend and Budget are the ROOT's alone and absent on every called run: a
	// budget is enforced against the tree (§12), so one pair per tree is the only
	// one that means anything.
	//
	// Spend is the tree's composed dollars, halves apart, through the one ledger
	// pricing rule every dollar surface folds through — so the map's number and
	// `WorkflowGetItem`'s are the same number, and a total carrying unpriced rows
	// says so instead of presenting a lower bound as exact.
	Spend *WorkflowRunSpend `json:"spend,omitempty"`
	// Budget is the ceiling in force, nil when there is none — which is most
	// runs. It is `engine.ResolveBudget`'s answer, the enforcement's own, so it
	// includes the ceiling a run inherits from its project profile
	// (`reliability.per_item_budget`) rather than only the one it declared. A
	// map that read the column alone rendered a profile-defaulted campaign as
	// unbounded right up to the moment the engine parked it at its ceiling.
	Budget *WorkflowAgentRunBudget `json:"budget,omitempty"`
}

// WorkflowRunMapSkeletonPhase is one phase of a frozen definition, projected to
// what the map needs to draw a node that has not happened yet. It is a
// projection rather than the raw snapshot deliberately: prompts, schemas, and
// envelopes are the largest thing a run persists and none of it is drawable.
type WorkflowRunMapSkeletonPhase struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Shape is the resolved shape — single, fan-out, or call — never the blank
	// an unannotated phase is authored with.
	Shape string `json:"shape"`
	// CallTarget is the workflow a call phase invokes, empty for every other
	// shape.
	CallTarget string `json:"callTarget,omitempty"`
	IsCheck    bool   `json:"isCheck"`
	// MaxDepth is the call edge's authored recursion bound, 0 when it declares
	// none — which is a real state, not a missing one: the run's budget is then
	// the only thing bounding the chain.
	MaxDepth int `json:"maxDepth,omitempty"`
}

// WorkflowRunMapPhaseAttempt is one recorded phase attempt. Cause is the
// ENGINE's diagnosis of a park (the workspace that would not cut, the budget
// that ran out); InterventionKind is the `kind` of the intervention persisted on
// the attempt, which today is `taken-over` or nothing — a human gate decision
// records its decision and note rather than a kind, so it surfaces through the
// run's own reason instead.
type WorkflowRunMapPhaseAttempt struct {
	PhaseID          string `json:"phaseId"`
	Attempt          int    `json:"attempt"`
	Status           string `json:"status"`
	Cause            string `json:"cause,omitempty"`
	InterventionKind string `json:"interventionKind,omitempty"`
	ThreadID         string `json:"threadId,omitempty"`
	StartedAt        int64  `json:"startedAt"`
	EndedAt          int64  `json:"endedAt,omitempty"`
}

// WorkflowRunMapUnit is one fan-out unit (or join) of one phase attempt. A unit
// row exists from the moment its attempt expands, so a queued branch is a real
// record rather than something the map has to guess at.
type WorkflowRunMapUnit struct {
	PhaseID   string `json:"phaseId"`
	Attempt   int    `json:"attempt"`
	UnitID    string `json:"unitId"`
	UnitIndex int    `json:"unitIndex"`
	Kind      string `json:"kind"`
	// Provider names the resource a pending unit is waiting capacity on.
	Provider    string `json:"provider,omitempty"`
	Status      string `json:"status"`
	UnitAttempt int    `json:"unitAttempt"`
	ThreadID    string `json:"threadId,omitempty"`
	StartedAt   int64  `json:"startedAt,omitempty"`
	EndedAt     int64  `json:"endedAt,omitempty"`
}

// maxWorkflowRunMapMembers is the most runs one map will answer for. Depth is
// already bounded by `engine.MaxCallDepth` (256), so this bounds the other
// dimension: 4096 is that depth at a realistic mean fan of sixteen calls per
// run — comfortably above any campaign this codebase has run, and low enough
// that the answer stays a few hundred kilobytes rather than a heap spike.
//
// Exceeding it REFUSES, as `WorkflowRunMapRefusalTooLarge`.
const maxWorkflowRunMapMembers = 4096

// WorkflowGetRunMap answers for the whole tree the supplied run belongs to. Any
// run in the tree is a valid argument — the root is resolved server-side (§5.9).
//
// It has a CLASSIFIED error contract. The three refusals a corrupt or oversized
// or simply absent tree produces come back as `view.Refusal` with a nil error,
// because each is an expected, permanent, user-facing state (see
// WorkflowRunMapRefusal). Everything else — a store that will not read, a
// ledger group with an unknown cost source — is an error, and the caller is
// right to retry those.
func (a *App) WorkflowGetRunMap(ctx context.Context, itemID string) (WorkflowRunMapView, error) {
	if a.store == nil {
		return WorkflowRunMapView{}, fmt.Errorf("workflow store unavailable")
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return WorkflowRunMapView{}, fmt.Errorf("workflow run map: item id is required")
	}
	rootID, err := a.workflowRunTreeRoot(itemID)
	if err != nil {
		return workflowRunMapRefusalFor(itemID, err)
	}
	tree, err := a.gatherWorkflowRunTree(ctx, rootID)
	if err != nil {
		return workflowRunMapRefusalFor(itemID, err)
	}
	// The scan anchors on the root and refuses an empty tree, so this cannot
	// fire — and it is checked rather than assumed because the alternative is
	// resolving a ceiling for a run with no id, which reads as "no budget".
	if tree.root.ItemID != rootID {
		return WorkflowRunMapView{}, fmt.Errorf("workflow run map: tree read for %s did not carry its root", rootID)
	}
	spend, budget, err := a.workflowRunMapMoney(ctx, tree.root, tree.usage, tree.usageDetail)
	if err != nil {
		return WorkflowRunMapView{}, err
	}
	view := WorkflowRunMapView{RootItemID: rootID, Runs: tree.runs}
	for index := range view.Runs {
		run := &view.Runs[index]
		run.AutoResumeAt = tree.resumeAt[run.ItemID]
		run.Phases = slicesx.OrEmpty(tree.phasesByItem[run.ItemID])
		run.Units = slicesx.OrEmpty(tree.unitsByItem[run.ItemID])
		if run.ItemID == rootID {
			run.Spend, run.Budget = &spend, budget
		}
	}
	return view, nil
}

// workflowRunMapRefusalFor classifies one read failure. A refusal the wire has
// a code for becomes an answer; anything else stays an error, so a transient
// failure keeps the retry a permanent one must not get.
func workflowRunMapRefusalFor(itemID string, err error) (WorkflowRunMapView, error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkflowRunMapView{Runs: []WorkflowRunMapRun{}, Refusal: &WorkflowRunMapRefusal{
			Code:    WorkflowRunMapRefusalNotFound,
			Message: fmt.Sprintf("Run %s no longer exists.", itemID),
		}}, nil
	case errors.Is(err, store.ErrWorkItemTreeTooLarge):
		return WorkflowRunMapView{Runs: []WorkflowRunMapRun{}, Refusal: &WorkflowRunMapRefusal{
			Code: WorkflowRunMapRefusalTooLarge,
			Message: fmt.Sprintf(
				"This campaign has more than %d runs, which is more than the map can draw at once.",
				maxWorkflowRunMapMembers),
		}}, nil
	case errors.Is(err, store.ErrWorkItemTreeTooDeep), errors.Is(err, store.ErrWorkItemTreeCyclicLinkage):
		return WorkflowRunMapView{Runs: []WorkflowRunMapRun{}, Refusal: &WorkflowRunMapRefusal{
			Code: WorkflowRunMapRefusalCorruptLinkage,
			Message: fmt.Sprintf(
				"The call linkage around run %s does not describe a tree, so its map cannot be drawn.",
				itemID),
		}}, nil
	}
	return WorkflowRunMapView{}, err
}

// workflowRunTree is one tree's rows, already grouped by the run they belong
// to. It exists so the bound method reads resolve → gather → assemble instead
// of interleaving the reads with the regroup each one needs.
type workflowRunTree struct {
	// runs is root-first and parent-before-child, already projected to the wire:
	// the store's rows carry the frozen snapshots, and nothing here keeps one.
	runs []WorkflowRunMapRun
	// root is what the tree's budget is resolved from — the root row narrowed to
	// the four facts a ceiling is decided by.
	root         engine.BudgetSubject
	resumeAt     map[string]int64
	phasesByItem map[string][]WorkflowRunMapPhaseAttempt
	unitsByItem  map[string][]WorkflowRunMapUnit
	usage        store.WorkItemUsage
	usageDetail  []store.UsageDetailRow
}

// gatherWorkflowRunTree reads the tree and buckets what it returns by the run
// it belongs to. Every read runs inside `ReadWorkItemTree`'s single
// transaction, so a run created mid-read cannot contribute attempt rows the
// assembly would silently discard — the runs and their records are one WAL
// snapshot. The caps are checked by the run scan, which is what keeps the other
// five statements off a tree this codebase refuses to answer for.
//
// The visitor is where snapshot retention is bounded: it projects each run to
// the wire shape and lets the blob go before the next row is scanned.
func (a *App) gatherWorkflowRunTree(ctx context.Context, rootID string) (workflowRunTree, error) {
	tree := workflowRunTree{
		runs:         make([]WorkflowRunMapRun, 0, 16),
		resumeAt:     make(map[string]int64),
		phasesByItem: make(map[string][]WorkflowRunMapPhaseAttempt),
		unitsByItem:  make(map[string][]WorkflowRunMapUnit),
	}
	read, err := a.store.ReadWorkItemTree(
		ctx, rootID, engine.MaxCallDepth, maxWorkflowRunMapMembers,
		func(item store.WorkItemTreeRun) error {
			run := WorkflowRunMapRun{
				ItemID: item.ID, WorkflowID: item.WorkflowID,
				ParentItemID: item.ParentItemID, ParentPhaseID: item.ParentPhaseID,
				ParentUnitID: item.ParentUnitID, ParentAttempt: item.ParentAttempt,
				CallDepth: item.CallDepth,
				State:     item.State, Reason: item.Reason, SoftStop: item.SoftStop,
				StartedAt: item.StartedAt, EndedAt: item.EndedAt,
			}
			run.Skeleton, run.TailSelfCall, run.SkeletonError = workflowRunMapSkeleton(item)
			run.SkeletonMissing = len(run.Skeleton) == 0
			if item.ID == rootID {
				tree.root = engine.BudgetSubject{
					ItemID: item.ID, ProjectID: item.ProjectID,
					Budget: item.Budget, StartedAt: item.StartedAt,
				}
			}
			tree.runs = append(tree.runs, run)
			return nil
		},
	)
	if err != nil {
		return workflowRunTree{}, fmt.Errorf("workflow run map: %w", err)
	}
	for _, resume := range read.AutoResumes {
		tree.resumeAt[resume.ItemID] = resume.At
	}
	for _, phase := range read.PhaseStatuses {
		tree.phasesByItem[phase.ItemID] = append(tree.phasesByItem[phase.ItemID], WorkflowRunMapPhaseAttempt{
			PhaseID: phase.PhaseID, Attempt: phase.Attempt, Status: phase.Status,
			Cause: phase.ParkCause, InterventionKind: phase.InterventionKind,
			ThreadID: phase.ThreadID, StartedAt: phase.StartedAt, EndedAt: phase.EndedAt,
		})
	}
	for _, unit := range read.UnitStatuses {
		tree.unitsByItem[unit.ItemID] = append(tree.unitsByItem[unit.ItemID], WorkflowRunMapUnit{
			PhaseID: unit.PhaseID, Attempt: unit.Attempt, UnitID: unit.UnitID,
			UnitIndex: unit.UnitIndex, Kind: unit.Kind, Provider: unit.Provider,
			Status: unit.Status, UnitAttempt: unit.UnitAttempt, ThreadID: unit.ThreadID,
			StartedAt: unit.StartedAt, EndedAt: unit.EndedAt,
		})
	}
	tree.usage, tree.usageDetail = read.Usage, read.UsageDetail
	return tree, nil
}

// workflowRunTreeRoot resolves the run every tree-wide fact belongs to. The
// linkage is acyclic by construction (§3a) and bounded by the engine's absolute
// call depth; the store's recursive walk carries that bound and REFUSES past
// it, which is what makes corrupt linkage terminate a read instead of hanging
// it — the same posture as `workflowAncestry` and `rootWorkItem`.
//
// A parent id whose row is GONE resolves to the last run that exists, and the
// dangling reference rides that run's `ParentItemID` on the wire: a root naming
// a parent no member of the answer carries IS the orphan state, which is why
// this neither logs it nor invents a field for it. The run the caller named is
// the exception: a request for a run that does not exist is
// `WorkflowRunMapRefusalNotFound`, because there is no tree to answer for.
//
// Membership DOWNWARD from this root is the store CTE's alone
// (`ReadWorkItemTree` and the reads inside it), which is what keeps the map's
// runs and its rows describing one tree. The discard preview's
// `walkWorkflowRunTree` (app_workflow_discard.go) is a separate walk on
// purpose: it drives per-run filesystem inspection, so it wants the rows one at
// a time rather than a set.
func (a *App) workflowRunTreeRoot(itemID string) (string, error) {
	node, err := a.store.WorkItemTreeRoot(itemID, engine.MaxCallDepth)
	if err != nil {
		return "", fmt.Errorf("workflow run map: %w", err)
	}
	return node.ID, nil
}

// workflowRunMapSkeleton projects one run's frozen definition and reports
// whether it tail-self-calls and why it has no skeleton when it has none.
//
// A snapshot that will not decode degrades to no skeleton plus the decode
// failure as user-facing state: the run's records are readable regardless, and
// refusing the whole map over one unreadable definition would take the tree
// away from the person trying to see what happened in it. It is deliberately
// NOT logged — this read runs on every debounced repaint, so a log line here is
// one corrupt row writing to disk forever, and the fact is on the wire where
// somebody can act on it.
//
// Tailness is judged against the RUN ROW's workflow id rather than the frozen
// definition's own, because that is the id a child run carries — which is what
// makes the chain edge the consumer follows and this flag the same statement.
func workflowRunMapSkeleton(item store.WorkItemTreeRun) (phases []WorkflowRunMapSkeletonPhase, tailSelfCall bool, decodeErr string) {
	snapshotPhases, err := workflowSnapshotPhases(item.Snapshot)
	if err != nil {
		return []WorkflowRunMapSkeletonPhase{}, false, err.Error()
	}
	skeleton := make([]WorkflowRunMapSkeletonPhase, 0, len(snapshotPhases))
	for _, phase := range snapshotPhases {
		node := WorkflowRunMapSkeletonPhase{
			ID: phase.ID, Name: phase.Name, Shape: string(phase.EffectiveShape()),
			IsCheck: workflowPhaseIsCheck(phase), MaxDepth: phase.MaxDepth,
		}
		if phase.IsCall() {
			node.CallTarget = phase.CallTarget()
		}
		skeleton = append(skeleton, node)
	}
	if len(skeleton) == 0 {
		return skeleton, false, ""
	}
	tail := skeleton[len(skeleton)-1]
	return skeleton, tail.CallTarget != "" && tail.CallTarget == item.WorkflowID, ""
}

// workflowRunMapMoney prices the root's whole tree and resolves the ceiling in
// force over it.
//
// Both go through the paths the ENFORCEMENT uses: the spend is
// `usageledger.PriceGroups` — the one ledger pricing rule every dollar surface
// folds through — and the ceiling is `engine.ResolveBudget`, the call the engine's
// own budget check is built on, so a profile-supplied default is the map's
// number exactly as it is the park's. The tree is priced ONCE and the resolved
// spend is handed to `ResolveBudget` rather than letting it re-read the ledger,
// which is what keeps a repainting map at the six statements it advertises.
func (a *App) workflowRunMapMoney(
	ctx context.Context, root engine.BudgetSubject,
	usage store.WorkItemUsage, detail []store.UsageDetailRow,
) (WorkflowRunSpend, *WorkflowAgentRunBudget, error) {
	priced, err := usageledger.PriceGroups(detail)
	if err != nil {
		return WorkflowRunSpend{}, nil, fmt.Errorf("workflow run map: run %s spend: %w", root.ItemID, err)
	}
	spend := WorkflowRunSpend{
		CostUSD: priced.TotalUSD(), WireCostUSD: priced.WireUSD,
		EstimatedCostUSD: priced.EstimatedUSD, UnpricedRows: priced.UnpricedRows,
	}
	view, err := engine.ResolveBudget(
		ctx,
		workflowProfileSource{store: a.store, configRoot: a.workflowDataRoot()},
		resolvedTreeSpend{rootItemID: root.ItemID, spend: engine.Spend{
			Tokens:    usage.TotalTokens,
			USD:       priced.TotalUSD(),
			Estimated: priced.Estimated(),
			Unpriced:  priced.UnpricedRows,
		}},
		root, time.Now(),
	)
	if err != nil {
		return WorkflowRunSpend{}, nil, fmt.Errorf("workflow run map: run %s budget: %w", root.ItemID, err)
	}
	return spend, workflowBudgetLine(view, root.ItemID), nil
}

// resolvedTreeSpend answers `ResolveBudget` with a tree spend its caller has
// already read, so one fetch prices the ledger once. It refuses any other run's
// id rather than answering with numbers that are not that run's: the interface
// takes an id, and a memo that ignored it would be a silent wrong answer the
// day a caller resolves two budgets from one fold.
type resolvedTreeSpend struct {
	rootItemID string
	spend      engine.Spend
}

func (s resolvedTreeSpend) TreeSpend(_ context.Context, rootItemID string) (engine.Spend, error) {
	if rootItemID != s.rootItemID {
		return engine.Spend{}, fmt.Errorf(
			"workflow run map spend: asked for run %s, priced run %s", rootItemID, s.rootItemID)
	}
	return s.spend, nil
}
