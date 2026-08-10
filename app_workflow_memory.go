package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/memory"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// Campaign memory (Packet L). A run tree accumulates what its work learned so
// later waves stop relearning it, and every element of the tree gets a bounded
// digest of that in its prompt.
//
// WRITE OWNERSHIP IS THE APP'S, entirely. The engine carries no memory config
// and holds no writer interface: it already does not know where a narrative
// file goes, where a worktree is cut, or where the config root is, and the two
// channels a note arrives on are both app-side seams — the scoped RPC the CLI
// speaks, and the envelope-lift `finish` already performs for `narrative`. The
// pure half (what a note is, where the log lives, how it renders) is
// `internal/workflow/memory`; this file is the run-shaped half: which tree a
// note belongs to, what provenance it is stamped with, and who may write it.
//
// The tree is keyed by the ROOT run because a campaign is the tree, not the run:
// a recursive self-calling spine and every lane it fans out to share one memory,
// which is the whole point.

// workflowMemoryTree is one run's resolved memory coordinates.
type workflowMemoryTree struct {
	// RootID is the run tree's root, which names the directory.
	RootID string
	// NotesPath is the append-only log.
	NotesPath string
	// Wave is the writing run's caller-chain depth relative to the root. It is
	// the engine's own `call_depth`, read off the row rather than recounted.
	Wave int
}

// workflowMemoryTreeFor resolves the memory tree one run writes into and reads
// from. Linkage is immutable, so this is a stable fact about the run.
func (a *App) workflowMemoryTreeFor(item store.WorkItem) (workflowMemoryTree, error) {
	ancestry, err := a.workflowAncestry(item)
	if err != nil {
		return workflowMemoryTree{}, fmt.Errorf("workflow memory tree for run %s: %w", item.ID, err)
	}
	return a.workflowMemoryTreeOf(ancestry)
}

// workflowMemoryTreeOf is the same answer from an ancestry the caller has
// ALREADY walked. Prompt assembly needs the goal chain and this tree from one
// linkage walk; resolving them independently paid for a depth-forty walk twice
// per element, which is what `workflowMemoryDigest`'s "this side never repeats
// it" was always meant to describe.
func (a *App) workflowMemoryTreeOf(ancestry []store.WorkItem) (workflowMemoryTree, error) {
	if len(ancestry) == 0 {
		return workflowMemoryTree{}, fmt.Errorf("workflow memory tree: empty ancestry")
	}
	root, item := ancestry[0], ancestry[len(ancestry)-1]
	notesPath, err := memory.NotesPath(a.workflowDataRoot(), root.ID)
	if err != nil {
		return workflowMemoryTree{}, err
	}
	return workflowMemoryTree{RootID: root.ID, NotesPath: notesPath, Wave: item.CallDepth}, nil
}

// workflowMemoryDigest renders the campaign-memory block one element's prompt
// carries, from the tree its run resolved to. It is called on every agent
// phase, unit, join, and takeover finalize, through `workflowPromptAncestry` —
// which is what owns the ancestry walk the tree comes from, so this side never
// repeats it.
//
// A failure to read is LOGGED and yields an empty digest rather than failing the
// attempt. Memory is context, not contract: an element that runs without it does
// the work with less to go on, while an element that never starts does none —
// and a run whose config root moved would otherwise be unable to take a single
// turn.
func (a *App) workflowMemoryDigest(tree workflowMemoryTree) workflowrunner.MemoryDigest {
	notes, skipped, err := memory.ReadNotes(tree.NotesPath)
	if err != nil {
		log.Printf("workflow memory: read %s: %v", tree.NotesPath, err)
		return ""
	}
	for _, entry := range skipped {
		log.Printf("workflow memory: %s line %d skipped: %s", tree.NotesPath, entry.Line, entry.Reason)
	}
	return workflowrunner.MemoryDigest(memory.Render(notes, memory.RenderOptions{
		NotesPath: tree.NotesPath, Budget: memory.DefaultInjectionBytes,
	}))
}

// recordWorkflowMemory appends validated drafts to a run's tree with app-stamped
// provenance. It is the ONE writer: both channels — the CLI verb and the
// envelope lift — land here, so neither can stamp a note the other could not.
func (a *App) recordWorkflowMemory(item store.WorkItem, provenance memory.Provenance, drafts []memory.Draft) (int, error) {
	if len(drafts) == 0 {
		return 0, nil
	}
	tree, err := a.workflowMemoryTreeFor(item)
	if err != nil {
		return 0, err
	}
	provenance.RunID = item.ID
	provenance.Wave = tree.Wave
	now := time.Now().UnixMilli()
	written := 0
	for _, draft := range drafts {
		note, err := memory.NewNote(draft, provenance, now)
		if err != nil {
			return written, err
		}
		if err := memory.Append(tree.NotesPath, note); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// recordEnvelopeMemory lands the notes an element carried in its envelope's
// `memory` field. It runs at the same seam the narrative is lifted at, so an
// element that ended its turn correctly has its notes recorded before anything
// downstream sees the envelope — and a failure here never fails the attempt,
// for the same reason a missing digest does not: the work is done and the
// envelope is valid, and losing an unrecordable note is strictly better than
// losing the turn that produced it.
func (a *App) recordEnvelopeMemory(key engine.RunKey, drafts []memory.Draft) {
	if len(drafts) == 0 {
		return
	}
	item, err := a.store.GetWorkItem(key.ItemID)
	if err != nil {
		log.Printf("workflow memory: load run %s for envelope notes: %v", key.ItemID, err)
		return
	}
	provenance := memory.Provenance{PhaseID: key.PhaseID, Attempt: key.Attempt, UnitID: key.UnitID}
	written, err := a.recordWorkflowMemory(item, provenance, drafts)
	if err != nil {
		log.Printf("workflow memory: record %d envelope notes for %s/%s: %v",
			len(drafts)-written, key.ItemID, key.PhaseID, err)
	}
}

// removeWorkflowMemoryTree deletes one root run's memory directory. It is
// called exactly where run RECORDS are deleted (project deletion), because the
// tree is that history's companion: a directory whose root run no longer exists
// is unreachable by every read verb and by every injection.
//
// Discard deliberately does NOT call it. Discard removes worktrees and branches
// and leaves the run rows in place, so the memory a discarded campaign
// accumulated stays readable exactly as its narratives and envelopes do.
func (a *App) removeWorkflowMemoryTree(rootRunID string) error {
	dir, err := memory.Dir(a.workflowDataRoot(), rootRunID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove workflow memory %s: %w", rootRunID, err)
	}
	return nil
}

// workflowMemoryProvenance resolves the element coordinates behind a scoped
// token. A phase token names its (item, phase); the ATTEMPT and the unit are
// facts about the live session, so they come off the row the session's thread is
// attached to — a unit row first, exactly as `workflowPhaseForThread` does,
// because a unit thread has no phase row of its own.
//
// A session whose rows have gone (a torn-down attempt still holding a token)
// yields the phase coordinate with no attempt rather than an error: the note is
// still this phase's, and refusing to record it would lose a lesson over a
// missing line number.
func (a *App) workflowMemoryProvenance(threadID, phaseID string) memory.Provenance {
	provenance := memory.Provenance{PhaseID: phaseID}
	unit, found, err := a.store.GetWorkItemUnitByThread(threadID)
	if err != nil {
		log.Printf("workflow memory: resolve unit for thread %s: %v", threadID, err)
		return provenance
	}
	if found {
		provenance.UnitID = unit.UnitID
		provenance.Attempt = unit.Attempt
		return provenance
	}
	phase, found, err := a.store.GetWorkItemPhaseByThread(threadID)
	if err != nil {
		log.Printf("workflow memory: resolve phase for thread %s: %v", threadID, err)
		return provenance
	}
	if found {
		provenance.Attempt = phase.Attempt
	}
	return provenance
}

// describeMemoryFindings renders draft validation for a CLI caller. The CLI
// prints the error verbatim, so it has to name every rule broken and the value
// that broke it.
func describeMemoryFindings(action string, err error) error {
	var validation *memory.ValidationError
	if !errors.As(err, &validation) {
		return err
	}
	parts := make([]string, 0, len(validation.Findings))
	for _, finding := range validation.Findings {
		parts = append(parts, strings.TrimPrefix(finding.Path, ".")+" "+finding.Message)
	}
	return fmt.Errorf("%s: %s", action, strings.Join(parts, "; "))
}
