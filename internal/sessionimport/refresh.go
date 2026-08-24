package sessionimport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/chatmodel"
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
	// UpdateAvailable: new history and/or a model-profile repair can be applied.
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

	thread         store.Thread
	state          store.ThreadImportState
	batch          store.ImportBatch
	cursor         Cursor
	leafUUID       string
	identity       rollout.SourceIdentity
	targetProfile  store.Thread
	profileChanged bool
}

// tailResult is one provider's answer to "what does the source file hold past
// this thread's cursor". One shape for both providers so PlanUpdate does not
// carry a widening tuple of out-parameters; the fields a given provider has
// nothing to say about stay zero (Claude sets no offset or identity, Codex no
// leaf uuid).
type tailResult struct {
	events    []importir.Event
	warnings  []importir.Warning
	leafUUID  string
	endOffset int64
	profile   importir.ModelProfile
	// identity is the source fingerprint recorded for the NEXT refresh
	// (store migration v67). Codex only; see codexTail.
	identity rollout.SourceIdentity
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

// Appliable reports whether ApplyUpdate would write history or restore the
// provider-recorded model profile.
func (u Update) Appliable() bool { return u.Status == UpdateAvailable }

// RestoresModelProfile reports whether applying this plan would restore model,
// effort, fast-mode, or context-window fields from the provider session.
func (u Update) RestoresModelProfile() bool { return u.profileChanged }

// ApplyResult reports what an update actually changed. RestoredModelProfile
// can be false even when the plan offered a repair: the compare-and-swap
// deliberately loses to a newer model selection made after planning.
type ApplyResult struct {
	Items                int
	Turns                int
	RestoredModelProfile bool
}

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
	thread, err := d.Store.GetThread(threadID)
	if err != nil {
		return Update{}, fmt.Errorf("sessionimport: load thread %s: %w", threadID, err)
	}

	// Each provider owns its own path resolution: a Codex `source_path` has
	// to pass the containment proof before it is stat'd at all
	// (rollout/AGENTS.md — the
	// check is lexical precisely so it runs before the file is touched), so
	// there is no shared pre-stat to hoist up here.
	var tail tailResult
	switch state.Provider {
	case ProviderClaude:
		tail, err = claudeTail(state)
	case ProviderCodex:
		tail, err = codexTail(ctx, d, state, thread.Model == "")
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

	update := Update{
		ThreadID: threadID,
		Status:   UpdateUpToDate,
		Warnings: tail.warnings,
		thread:   thread,
		state:    state,
		leafUUID: tail.leafUUID,
		identity: tail.identity,
		cursor:   CursorOf(state),
	}
	if thread.Model == "" {
		update.targetProfile = applyRecordedProfile(thread, tail.profile)
		update.profileChanged = tail.profile.Model != "" && !chatmodel.SameModelFields(thread, update.targetProfile)
	}
	if len(tail.events) == 0 {
		if update.profileChanged {
			update.Status = UpdateAvailable
			update.Detail = "The model settings recorded in the session file can be restored."
		}
		return update, nil
	}

	batch, buildWarnings, err := NewWriter(d.Store, thread).Build(tail.events)
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
	update.cursor = NewCursor(batch, tail.events).Advance(CursorOf(state))
	if tail.endOffset > update.cursor.SourceOffset {
		update.cursor.SourceOffset = tail.endOffset
	}
	update.NewItems = len(batch.Rows)
	update.NewTurns = len(batch.Turns)
	if update.NewItems == 0 && update.NewTurns == 0 {
		// The tail was real but produced no rows (skipped reasoning blocks,
		// unknown envelope types). There is nothing for the user to apply;
		// the next check re-reads the same few lines and decides the same way.
		if !update.profileChanged {
			return update, nil
		}
		update.Status = UpdateAvailable
		update.Detail = "The model settings recorded in the session file can be restored."
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
func ApplyUpdate(d Deps, update Update) (ApplyResult, error) {
	if d.Store == nil {
		return ApplyResult{}, fmt.Errorf("sessionimport: update apply has no store")
	}
	if !update.Appliable() {
		detail := update.Detail
		if detail == "" {
			detail = "There is nothing new in the session file."
		}
		return ApplyResult{}, errors.New(detail)
	}
	if update.NewItems > 0 || update.NewTurns > 0 {
		if err := d.Store.ApplyImportBatch(update.thread.ID, update.batch); err != nil {
			return ApplyResult{}, err
		}
	}
	result := ApplyResult{Items: update.NewItems, Turns: update.NewTurns}
	if update.profileChanged {
		applied, err := d.Store.CompareAndSwapModelProfile(update.thread, update.targetProfile)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("sessionimport: restore model profile for %s: %w", update.thread.ID, err)
		}
		result.RestoredModelProfile = applied
	}

	state := update.state
	update.cursor.Apply(&state)
	if update.leafUUID != "" {
		// The branch this thread tracks now ends somewhere else. Recording
		// the new leaf keeps the NEXT refresh anchored on the same branch by
		// its own identity rather than by an ancestor it happens to share.
		state.LeafUUID = update.leafUUID
	}
	if update.identity.MetaHash != "" {
		// Re-stamped on every apply, which is also what BACKFILLS a thread
		// imported before migration v67: its stored hash is empty, the
		// comparison was skipped, and from here on the guard is armed.
		state.SourceMetaHash = update.identity.MetaHash
		state.SourceHistoryMode = update.identity.HistoryMode
	}
	// RefreshedAt is bookkeeping — when AO last read the file — not history,
	// and the same allowed exception ImportedAt is.
	state.RefreshedAt = time.Now().UnixMilli()
	if err := d.Store.SetThreadImportState(state); err != nil {
		return ApplyResult{}, fmt.Errorf("sessionimport: advance cursor for %s: %w", update.thread.ID, err)
	}
	return result, nil
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
func claudeTail(state store.ThreadImportState) (tailResult, error) {
	// A deleted transcript is a STATUS the user can act on, not the raw open
	// error LoadSession would return, so it is asked about first.
	if _, err := os.Stat(state.SourcePath); err != nil {
		return tailResult{}, missingf(state.SourcePath)
	}
	loaded, err := claudesessions.LoadSession(state.SourcePath)
	if err != nil {
		return tailResult{}, fmt.Errorf("sessionimport: re-read %s: %w", state.SourcePath, err)
	}
	defer loaded.Close()

	anchor := strings.TrimSpace(state.LeafUUID)
	index, ok := claudeBranchFor(loaded.Branches, anchor)
	if !ok {
		return tailResult{}, divergedf(
			"The imported branch is no longer in %s — the session file has been rewritten since the import.",
			state.SourcePath)
	}
	branch, err := loaded.ConvertBranch(index)
	if err != nil {
		return tailResult{}, fmt.Errorf("sessionimport: re-read %s: %w", state.SourcePath, err)
	}
	tail := tailResult{
		warnings: append(append([]importir.Warning(nil), loaded.Warnings...), branch.Warnings...),
		leafUUID: branch.LeafUUID,
		profile:  branch.Profile,
	}

	cursor := strings.TrimSpace(state.LastSourceUUID)
	if cursor == "" {
		// Nothing was imported from this branch, so everything on it is new.
		tail.events = branch.Events
		return tail, nil
	}
	last := -1
	for i := range branch.Events {
		if branch.Events[i].SourceUUID == cursor {
			last = i
		}
	}
	if last < 0 {
		return tailResult{}, divergedf(
			"The last imported message is no longer in %s — the session file has been rewritten since the import.",
			state.SourcePath)
	}
	tail.events = branch.Events[last+1:]
	return tail, nil
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

// codexTail reads the rollout from the byte offset the import stopped at.
//
// TWO divergence tests, and neither subsumes the other:
//
//   - SOURCE IDENTITY. The sha256 of the file's first line and its declared
//     `history_mode` are compared against what the import recorded. This is
//     what catches a rollout Codex REWROTE in place: since 0.147 a thread can
//     be migrated from `legacy` to `paginated` history, and the migration
//     canonicalises the whole file and publishes it atomically over the same
//     path (codex-rs thread-store/src/local/rollout_migration.rs). The result
//     is usually the same size or larger — so the size test below passes —
//     while every byte offset in it addresses a different record.
//   - APPEND-ONLY POSITION. A file smaller than the cursor, or a cursor that
//     no longer follows a record boundary, is a file that was replaced or
//     truncated (rollout.ErrSourceShrank).
//
// A pre-v67 row records no fingerprint at all. Empty means UNKNOWN, not
// mismatched: the comparison is skipped and the next successful apply
// backfills it (ApplyUpdate), because reporting every thread imported before
// the column existed as diverged would be a worse answer than the size test
// alone, which is what those threads have always had.
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
	ctx context.Context, d Deps, state store.ThreadImportState, repairProfile bool,
) (tailResult, error) {
	sourcePath, err := rollout.PathInHome(d.CodexHome, state.SourcePath)
	if err != nil {
		return tailResult{}, divergedf(
			"%s is not inside this machine's Codex home, so its history can no longer be read for this thread.",
			state.SourcePath)
	}
	// Contained, so it may be opened: a deleted rollout is source-missing
	// rather than the raw open error Parse would return.
	//
	// ONE handle serves every read below — the identity fingerprint, the tail
	// parse, and the profile repair. Opening the path per read leaves a window
	// in which Codex publishes a migrated rollout over it (thread-store writes
	// the canonicalised file and renames it into place), and a refresh that
	// straddles that rename proves the identity of the OLD file and then reads
	// its tail out of the NEW one, at offsets that address different records.
	// A held fd keeps naming the inode it opened, which is what makes the two
	// answers describe the same file.
	file, err := os.Open(sourcePath)
	if err != nil {
		return tailResult{}, missingf(state.SourcePath)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return tailResult{}, missingf(state.SourcePath)
	}
	// An unreadable header splits on whether this row HAS a fingerprint:
	//
	//   - Unfingerprinted (pre-v67 row, or one whose header was never
	//     readable): UNKNOWN, not diverged — the same posture the FIRST
	//     import takes (import_one.go, "an unfingerprinted thread simply
	//     refreshes under the size test alone"). A hard error would make a
	//     refresh fail where the import that created the row succeeded, and —
	//     worse — the zero identity carries an EMPTY history mode, which
	//     codexSourceIdentityMatches would read as a legacy→paginated
	//     migration and report as permanent divergence on a file that is
	//     merely being appended to right now. The comparison is skipped
	//     rather than run against a value nothing read, and the zero identity
	//     never reaches the state row: ApplyUpdate only writes a fingerprint
	//     whose MetaHash is non-empty.
	//   - Fingerprinted, resuming from a real offset: FAIL CLOSED. The thread
	//     has a recorded identity precisely so a rewritten file cannot be
	//     resumed from, and skipping the check because the header could not be
	//     read is skipping it exactly when the file is least trustworthy.
	read, readErr := rollout.ReadSourceIdentityAt(file, sourcePath, state.SourceSessionID)
	identity, err := codexIdentityForRefresh(state, read, readErr)
	if err != nil {
		return tailResult{}, err
	}
	if repairProfile && state.LastSourceOffset == stat.Size() {
		profile, err := rollout.ReadLatestProfileAt(ctx, file, sourcePath)
		if err != nil {
			return tailResult{}, fmt.Errorf("sessionimport: recover model profile from %s: %w", state.SourcePath, err)
		}
		return tailResult{endOffset: state.LastSourceOffset, profile: profile, identity: identity}, nil
	}
	parsed, err := rollout.Parse(ctx, rollout.ParseOptions{
		File:       file,
		Path:       sourcePath,
		SessionID:  state.SourceSessionID,
		FromOffset: state.LastSourceOffset,
	})
	if errors.Is(err, rollout.ErrSourceShrank) {
		return tailResult{}, divergedf(
			"%s no longer contains the position this thread was imported up to — the rollout was replaced, truncated, or rewritten by a Codex history migration, so its history no longer continues this thread.",
			state.SourcePath)
	}
	if err != nil {
		return tailResult{}, fmt.Errorf("sessionimport: re-read %s: %w", state.SourcePath, err)
	}
	parsed = rollout.ProjectReviewChildren(ctx, d.CodexHome, state.SourceSessionID, parsed)
	profile := parsed.Profile
	if repairProfile && profile.Model == "" {
		profile, err = rollout.ReadLatestProfileAt(ctx, file, sourcePath)
		if err != nil {
			return tailResult{}, fmt.Errorf("sessionimport: recover model profile from %s: %w", state.SourcePath, err)
		}
	}
	return tailResult{
		events:    parsed.Events,
		warnings:  parsed.Warnings,
		endOffset: parsed.EndOffset,
		profile:   profile,
		identity:  identity,
	}, nil
}

// codexIdentityForRefresh turns an identity read into the value the rest of
// the refresh uses, or into the refusal that ends it. The two failure postures
// it separates are described on codexTail; this is where the split lives so
// the rule has one home and one test.
func codexIdentityForRefresh(
	state store.ThreadImportState, identity rollout.SourceIdentity, readErr error,
) (rollout.SourceIdentity, error) {
	if readErr == nil {
		if err := codexSourceIdentityMatches(state, identity); err != nil {
			return rollout.SourceIdentity{}, err
		}
		return identity, nil
	}
	if state.SourceMetaHash != "" && state.LastSourceOffset > 0 {
		// A FINGERPRINTED row is a different case from the unfingerprinted
		// one: this thread has a recorded identity AND a position inside the
		// file, and the one test that could prove the position still addresses
		// the same history did not run. Tolerating that resumes from a byte
		// offset in a file nothing vouched for — precisely the splice the
		// fingerprint exists to prevent. It is worded as divergence because
		// the user's remedy is the same: re-import the session.
		return rollout.SourceIdentity{}, divergedf(
			"The header of %s could not be read (%v), so this thread's recorded fingerprint cannot be checked against it.",
			state.SourcePath, readErr)
	}
	// Unfingerprinted: UNKNOWN, and the zero identity never reaches the state
	// row (ApplyUpdate only writes a non-empty MetaHash).
	return rollout.SourceIdentity{}, nil
}

// codexSourceIdentityMatches is the source-identity half of the divergence
// test. It answers source-diverged prose, never a bare error, because the
// causes it separates are things the user can reason about: a rollout replaced
// by something else, a rollout the Codex CLI itself rewrote when it migrated
// the thread's history format, and a rollout whose first record can no longer
// be fingerprinted at all.
//
// A RECORDED value of "" is skipped rather than compared — see codexTail. A
// CURRENT value of "" against a recorded one is the opposite case and FAILS
// CLOSED: ReadSourceIdentityAt answers a nil error with an empty MetaHash
// whenever the file has no complete, in-window first line (a first record past
// the bounded head read, a truncated head, an empty file), so treating it as
// "nothing to compare" skips the fingerprint exactly when the file is least
// like the one that was imported. A thread that HAS a fingerprint proved its
// first line was readable at import time; a header that has stopped being
// readable is a header that changed.
func codexSourceIdentityMatches(state store.ThreadImportState, identity rollout.SourceIdentity) error {
	if state.LastSourceOffset <= 0 {
		// Nothing was resumed from, so there is no position to invalidate.
		return nil
	}
	if state.SourceMetaHash == "" {
		// Unfingerprinted (pre-v67, or a header that was never readable):
		// UNKNOWN, not mismatched. The next successful apply backfills it.
		return nil
	}
	if identity.MetaHash == "" {
		return divergedf(
			"The first record of %s can no longer be read as this thread's header, so the fingerprint recorded at import time cannot be checked against it — the rollout was replaced or rewritten, and its byte offsets no longer address this thread's history.",
			state.SourcePath)
	}
	// Compared through historyModeLabel, so an ABSENT mode and an explicit
	// `legacy` are the same mode rather than a migration between them: the
	// field only exists from Codex 0.147 and its enum defaults to Legacy, so
	// a header that spells the default out has not migrated anything. A
	// genuine change to those bytes is still caught — by the fingerprint
	// below, whose prose ("the header has changed") is the accurate one for
	// it.
	recordedMode := historyModeLabel(strings.TrimSpace(state.SourceHistoryMode))
	currentMode := historyModeLabel(strings.TrimSpace(identity.HistoryMode))
	if recordedMode != currentMode {
		return divergedf(
			"%s was rewritten by Codex when it migrated this thread's history from %s to %s, so the position this thread was imported up to no longer exists in it.",
			state.SourcePath, recordedMode, currentMode)
	}
	if state.SourceMetaHash != identity.MetaHash {
		return divergedf(
			"The header of %s has changed since the import — the rollout was replaced or rewritten (a Codex history migration does this), so its history no longer continues this thread.",
			state.SourcePath)
	}
	return nil
}

// historyModeLabel names a `history_mode` for a sentence a user reads. An
// ABSENT mode is legacy: the field only exists from Codex 0.147 and its enum
// defaults to Legacy (rollout.HistoryModeLegacy).
func historyModeLabel(mode string) string {
	if mode == "" {
		return rollout.HistoryModeLegacy
	}
	return mode
}

func applyRecordedProfile(thread store.Thread, profile importir.ModelProfile) store.Thread {
	if profile.Model == "" {
		return thread
	}
	thread.Model = profile.Model
	if profile.ReasoningEffort != "" {
		thread.ReasoningEffort = profile.ReasoningEffort
	}
	if profile.ContextWindow > 0 {
		thread.ContextWindow = profile.ContextWindow
	}
	return chatmodel.SanitizeThread(thread)
}

// countNoun renders "1 new message" / "3 new messages".
func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
