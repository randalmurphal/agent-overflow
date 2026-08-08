package sessionimport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/importir"
	claudesessions "agent-overflow/internal/provider/claude/sessionimport"
	"agent-overflow/internal/provider/codex/rollout"
	"agent-overflow/internal/store"
)

// Update statuses. These strings are the wire contract — the frontend
// branches on them — so they live here rather than being re-spelled in the
// app layer.
const (
	// UpdateNotImported: the thread was created by AO, not imported.
	UpdateNotImported = "not-imported"
	// UpdateUpToDate: the source file holds nothing this thread does not.
	UpdateUpToDate = "up-to-date"
	// UpdateAvailable: the source grew and the tail can be appended.
	UpdateAvailable = "updates-available"
	// UpdateDivergedLocal: the thread was resumed inside AO after it was
	// imported, so its timeline and the file's are two different futures.
	UpdateDivergedLocal = "diverged-local"
	// UpdateSourceMissing: the session file is gone.
	UpdateSourceMissing = "source-missing"
	// UpdateSourceDiverged: the file is there but no longer contains the
	// position this thread was imported up to.
	UpdateSourceDiverged = "source-diverged"
)

// Update is one answer to "has this imported thread's source file grown".
//
// It is BOTH the answer and the plan: the exported fields are what a check
// reports, and the unexported ones are the rows an apply commits. One
// function computes both so a check can never disagree with the apply that
// follows it — the check is the apply minus the two writes.
type Update struct {
	ThreadID string
	Status   string
	// Detail is user-facing prose. Always set for a status the user has to
	// act on; empty is allowed only for up-to-date.
	Detail string
	// NewItems and NewTurns are the exact row counts an apply would write,
	// because the batch is already built.
	NewItems int
	NewTurns int
	Warnings []importir.Warning

	thread   store.Thread
	state    store.ThreadImportState
	batch    store.ImportBatch
	cursor   Cursor
	leafUUID string
}

// refusalError carries user-facing prose while still answering
// errors.Is(err, errSourceDiverged) / errSourceMissing. The prose is the
// whole message — a wrapped sentinel would append "source diverged" to a
// sentence written for the user.
type refusalError struct {
	detail string
	status error
}

func (e *refusalError) Error() string { return e.detail }
func (e *refusalError) Unwrap() error { return e.status }

// The two conditions a provider tail can report that are STATUSES rather
// than failures: the file no longer addresses the position this thread
// stopped at, and the file this app can read is not there at all.
var (
	errSourceDiverged = errors.New("sessionimport: source diverged")
	errSourceMissing  = errors.New("sessionimport: source missing")
)

func divergedf(format string, args ...any) error {
	return &refusalError{detail: fmt.Sprintf(format, args...), status: errSourceDiverged}
}

// missingf is the source-missing prose. It names the RECORDED path, never a
// resolved one: the user knows the file by where the import found it.
func missingf(sourcePath string) error {
	return &refusalError{
		detail: fmt.Sprintf(
			"The session file this thread was imported from is no longer at %s.", sourcePath),
		status: errSourceMissing,
	}
}

// Appliable reports whether ApplyUpdate would write anything.
func (u Update) Appliable() bool { return u.Status == UpdateAvailable }

// PlanUpdate reads the source file behind an imported thread and works out
// what a refresh would append.
//
// It writes nothing, which is what lets the check RPC and the apply RPC share
// it. The cost is one file read plus one store-pure Build; both are bounded by
// the tail the file grew, except on Claude, where the transcript has to be
// re-read whole to rebuild the branch DAG.
//
// The four refusal statuses are ordered by what the user can do about them:
// a thread AO created (not-imported), a thread the user continued in AO
// (diverged-local), a file that is gone (source-missing), and a file that no
// longer contains the position this thread stopped at (source-diverged). Only
// the last two can change back on their own.
func PlanUpdate(ctx context.Context, d Deps, threadID string) (Update, error) {
	if d.Store == nil {
		return Update{}, fmt.Errorf("sessionimport: update check has no store")
	}
	if strings.TrimSpace(threadID) == "" {
		return Update{}, fmt.Errorf("sessionimport: update check has no thread id")
	}
	if err := ctx.Err(); err != nil {
		return Update{}, err
	}

	state, found, err := d.Store.GetThreadImportState(threadID)
	if err != nil {
		return Update{}, err
	}
	if !found {
		return Update{
			ThreadID: threadID,
			Status:   UpdateNotImported,
			Detail:   "This thread was created in Agent Overflow, not imported from a provider session.",
		}, nil
	}

	diverged, err := Diverged(d.Store, state)
	if err != nil {
		return Update{}, err
	}
	if diverged {
		return Update{
			ThreadID: threadID,
			Status:   UpdateDivergedLocal,
			Detail: "This thread has been continued in Agent Overflow since it was imported. " +
				"Appending the session's newer messages would interleave two different conversations, so the refresh was refused.",
		}, nil
	}

	// The tail runs BEFORE the thread row is loaded, and each provider owns
	// its own path resolution: a Codex `source_path` has to pass the
	// containment proof before it is stat'd at all (rollout/AGENTS.md — the
	// check is lexical precisely so it runs before the file is touched), so
	// there is no shared pre-stat to hoist up here.
	var (
		events    []importir.Event
		warnings  []importir.Warning
		leafUUID  string
		endOffset int64
	)
	switch state.Provider {
	case ProviderClaude:
		events, warnings, leafUUID, err = claudeTail(state)
	case ProviderCodex:
		events, warnings, endOffset, err = codexTail(ctx, d, state)
	default:
		err = fmt.Errorf("sessionimport: thread %s records unknown import provider %q", threadID, state.Provider)
	}
	switch {
	case errors.Is(err, errSourceMissing):
		return Update{
			ThreadID: threadID,
			Status:   UpdateSourceMissing,
			Detail:   err.Error(),
		}, nil
	case errors.Is(err, errSourceDiverged):
		return Update{
			ThreadID: threadID,
			Status:   UpdateSourceDiverged,
			Detail:   err.Error(),
		}, nil
	case err != nil:
		return Update{}, err
	}

	thread, err := d.Store.GetThread(threadID)
	if err != nil {
		return Update{}, fmt.Errorf("sessionimport: load thread %s: %w", threadID, err)
	}

	update := Update{
		ThreadID: threadID,
		Status:   UpdateUpToDate,
		Warnings: warnings,
		thread:   thread,
		state:    state,
		leafUUID: leafUUID,
	}
	if len(events) == 0 {
		return update, nil
	}

	batch, buildWarnings, err := NewWriter(d.Store, thread).Build(events)
	if err != nil {
		// The tail did not convert. The usual cause is an import taken while
		// the session was mid-turn: the writer sealed the open turn and
		// force-closed its tools, so the completions that follow the cursor
		// name launches this thread already settled. Refusing is the honest
		// answer — a partial append would produce a timeline neither the file
		// nor AO ever held.
		return Update{}, fmt.Errorf(
			"sessionimport: thread %s cannot append its source's newer messages (import the session again to pick them up): %w",
			threadID, err)
	}
	update.Warnings = append(update.Warnings, buildWarnings...)
	update.batch = batch
	update.cursor = NewCursor(batch, events).Advance(CursorOf(state))
	if endOffset > update.cursor.SourceOffset {
		update.cursor.SourceOffset = endOffset
	}
	update.NewItems = len(batch.Rows)
	update.NewTurns = len(batch.Turns)
	if update.NewItems == 0 && update.NewTurns == 0 {
		// The tail was real but produced no rows (skipped reasoning blocks,
		// unknown envelope types). There is nothing for the user to apply;
		// the next check re-reads the same few lines and decides the same way.
		return update, nil
	}
	update.Status = UpdateAvailable
	update.Detail = fmt.Sprintf("%s and %s can be added from the session file.",
		countNoun(update.NewItems, "new message"), countNoun(update.NewTurns, "turn"))
	return update, nil
}

// ApplyUpdate commits the rows PlanUpdate built and advances the cursor.
//
// It refuses anything but an appliable plan rather than silently doing
// nothing: every other status is a condition the caller has to show the user,
// and a no-op return would read as "refreshed" in the UI.
func ApplyUpdate(d Deps, update Update) (int, int, error) {
	if d.Store == nil {
		return 0, 0, fmt.Errorf("sessionimport: update apply has no store")
	}
	if !update.Appliable() {
		detail := update.Detail
		if detail == "" {
			detail = "There is nothing new in the session file."
		}
		return 0, 0, errors.New(detail)
	}
	if err := d.Store.ApplyImportBatch(update.thread.ID, update.batch); err != nil {
		return 0, 0, err
	}

	state := update.state
	update.cursor.Apply(&state)
	if update.leafUUID != "" {
		// The branch this thread tracks now ends somewhere else. Recording
		// the new leaf keeps the NEXT refresh anchored on the same branch by
		// its own identity rather than by an ancestor it happens to share.
		state.LeafUUID = update.leafUUID
	}
	// RefreshedAt is bookkeeping — when AO last read the file — not history,
	// and the same allowed exception ImportedAt is.
	state.RefreshedAt = time.Now().UnixMilli()
	if err := d.Store.SetThreadImportState(state); err != nil {
		return 0, 0, fmt.Errorf("sessionimport: advance cursor for %s: %w", update.thread.ID, err)
	}
	return update.NewItems, update.NewTurns, nil
}

// claudeTail re-reads a transcript and returns the events that follow this
// thread's cursor on the branch it was cut from.
//
// A transcript is a DAG, so "did it grow" is a question about ONE branch. The
// thread records the leaf it was cut at, and a branch that extends it still
// contains that leaf — so the candidates are the branches whose chain holds
// it, and the answer is the file-order-last of those (BuildBranches sorts by
// leaf file position, which is the same rule the live resume path calls the
// active branch). A leaf that appears on no branch means the file was
// replaced or rewritten under us.
//
// Only that ONE branch is converted. Which branch answers the question is a
// property of the skeleton chains alone, and a check that converted every
// branch would pay a whole import to answer "did anything change" — on a
// file that is routinely hundreds of megabytes, on every context-menu open.
func claudeTail(state store.ThreadImportState) ([]importir.Event, []importir.Warning, string, error) {
	// A deleted transcript is a STATUS the user can act on, not the raw open
	// error LoadSession would return, so it is asked about first.
	if _, err := os.Stat(state.SourcePath); err != nil {
		return nil, nil, "", missingf(state.SourcePath)
	}
	loaded, err := claudesessions.LoadSession(state.SourcePath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("sessionimport: re-read %s: %w", state.SourcePath, err)
	}
	defer loaded.Close()

	anchor := strings.TrimSpace(state.LeafUUID)
	index, ok := claudeBranchFor(loaded.Branches, anchor)
	if !ok {
		return nil, nil, "", divergedf(
			"The imported branch is no longer in %s — the session file has been rewritten since the import.",
			state.SourcePath)
	}
	branch, err := loaded.ConvertBranch(index)
	if err != nil {
		return nil, nil, "", fmt.Errorf("sessionimport: re-read %s: %w", state.SourcePath, err)
	}
	warnings := append(append([]importir.Warning(nil), loaded.Warnings...), branch.Warnings...)

	cursor := strings.TrimSpace(state.LastSourceUUID)
	if cursor == "" {
		// Nothing was imported from this branch, so everything on it is new.
		return branch.Events, warnings, branch.LeafUUID, nil
	}
	last := -1
	for i := range branch.Events {
		if branch.Events[i].SourceUUID == cursor {
			last = i
		}
	}
	if last < 0 {
		return nil, nil, "", divergedf(
			"The last imported message is no longer in %s — the session file has been rewritten since the import.",
			state.SourcePath)
	}
	return branch.Events[last+1:], warnings, branch.LeafUUID, nil
}

// claudeBranchFor picks the branch a thread's refresh follows: the LAST
// branch whose chain contains the recorded leaf. Later in the slice means
// later in the file, which is the branch a resume would land on when one leaf
// grew into several. It answers an INDEX, so the caller converts exactly one.
//
// An empty anchor (a thread imported before the leaf was recorded, or a
// transcript that named none) falls back to the file's last branch, which is
// the same one the live resume path picks.
func claudeBranchFor(branches []claudesessions.Branch, anchorUUID string) (int, bool) {
	if len(branches) == 0 {
		return 0, false
	}
	if anchorUUID == "" {
		return len(branches) - 1, true
	}
	for i := len(branches) - 1; i >= 0; i-- {
		for _, row := range branches[i].Chain {
			if row.UUID == anchorUUID {
				return i, true
			}
		}
	}
	return 0, false
}

// codexTail reads the rollout from the byte offset the import stopped at. A
// rollout is append-only, so a file smaller than the cursor is a file that was
// replaced.
//
// `source_path` is re-proved to be inside the CURRENT Codex home BEFORE the
// file is touched at all — not merely before it is opened. It was contained
// when the import wrote it, but the home is app-level state that can move (a
// credential-home override, a WSL relocation, a restored backup), and a
// refresh must not open a path the running configuration would never have
// listed. The proof is lexical for exactly that reason (rollout/AGENTS.md):
// stat'ing first would report the existence and size of an arbitrary path
// back to the user through the refresh status. A moved home reads as
// source-diverged, which is the honest answer: the file this thread was
// imported from is not one this app can see.
func codexTail(
	ctx context.Context, d Deps, state store.ThreadImportState,
) ([]importir.Event, []importir.Warning, int64, error) {
	sourcePath, err := rollout.PathInHome(d.CodexHome, state.SourcePath)
	if err != nil {
		return nil, nil, 0, divergedf(
			"%s is not inside this machine's Codex home, so its history can no longer be read for this thread.",
			state.SourcePath)
	}
	// Contained, so it may be stat'd: a deleted rollout is source-missing
	// rather than the raw open error Parse would return.
	if _, err := os.Stat(sourcePath); err != nil {
		return nil, nil, 0, missingf(state.SourcePath)
	}
	parsed, err := rollout.Parse(ctx, rollout.ParseOptions{
		Path:       sourcePath,
		SessionID:  state.SourceSessionID,
		FromOffset: state.LastSourceOffset,
	})
	if errors.Is(err, rollout.ErrSourceShrank) {
		return nil, nil, 0, divergedf(
			"%s is smaller than it was at import — the rollout was replaced or truncated, so its history no longer continues this thread.",
			state.SourcePath)
	}
	if err != nil {
		return nil, nil, 0, fmt.Errorf("sessionimport: re-read %s: %w", state.SourcePath, err)
	}
	return parsed.Events, parsed.Warnings, parsed.EndOffset, nil
}

// countNoun renders "1 new message" / "3 new messages".
func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
