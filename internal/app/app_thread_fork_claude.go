// Claude half of the fork saga (app_thread_fork.go): session-JSONL
// slicing at a turn / message cut and the mid-turn capture that pins a
// live-source tail fork's lazy cut. A slice keeps every message uuid, so
// the fork's rows keep the provider ids they carry in the source.
package app

import (
	"errors"
	"fmt"
	"log"
	"os"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// forkClaudeThread wires Claude's resume state for the new fork.
//
// Every tail fork pins the lazy --fork-session cut before the fork's cut.
// The source may advance while the fork waits for its first send, including
// when the source was idle at fork time. An unstarted fork retains its
// inherited pin. The first send repairs that pin against CLI resume filters.
//
// Fork at point: slice the source JSONL ourselves (the official
// recipe — see internal/provider/claude/sessionfork). The new
// <newID>.jsonl on disk is a complete, resume-loadable session
// truncated through the END of atTurnIndex's turn (so the previous
// turn's full assistant response is preserved — slicing at the user
// prompt itself would leave Claude waiting to respond on resume,
// which is the wrong semantics). SessionRef points at the new ID
// directly — no --fork-session needed since the JSONL is already the
// fork. This path is unchanged mid-turn: an anchored fork always cuts
// strictly below the in-flight turn (the caller normalizes anything
// higher to a tail fork), so its anchor rows are old and already on
// disk, and ParseTranscript tolerates the source's torn final line.
func (a *App) forkClaudeThread(source store.Thread, atTurnIndex *int, midTurnCut *claudeMidTurnCut) (forkResumeState, error) {
	// The mid-turn cut is decided BEFORE the missing-session-ref
	// refusal: mid-turn, a tail fork of a thread whose session has not
	// landed yet is the sanctioned degenerate case (fresh provider
	// thread on first send), not an error. Its presence already implies
	// atTurnIndex == nil — the caller only captures one for a tail fork.
	if midTurnCut != nil {
		if midTurnCut.degenerate() {
			return forkResumeState{}, nil
		}
		return forkResumeState{
			PendingForkRef: midTurnCut.SessionRef,
			PinnedResumeAt: midTurnCut.Leaf,
		}, nil
	}

	tail := atTurnIndex == nil
	if !tail {
		lastTurn, err := a.store.LastTurnIndex(source.ID)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork thread: load last turn index: %w", err)
		}
		// Forking at or past the last turn is equivalent to fork-at-tail.
		tail = *atTurnIndex >= lastTurn
	}

	if source.SessionRef == "" {
		return forkResumeState{}, fmt.Errorf("fork thread: source thread %q is missing a Claude session reference", source.ID)
	}

	if tail {
		// Tripwire, not a reachable path: ForkThread captures a mid-turn
		// cut whenever a live session is registered, so arriving here with
		// one means a caller skipped the capture — and the unpinned lazy
		// path below would snapshot the transcript at the fork's FIRST
		// SEND, minutes or turns after the timeline was cut (the
		// 2026-08-22 skew incident). Refuse loudly instead of silently
		// deferring the cut.
		if _, ok := a.activeClaudeSession(source.ID); ok {
			return forkResumeState{}, fmt.Errorf(
				"fork thread: source thread %s has a live Claude session but no mid-turn cut was captured — refusing the unpinned lazy --fork-session path, which would snapshot at first send instead of now",
				source.ID,
			)
		}
		return forkResumeState{}, fmt.Errorf("fork thread: Claude tail cut was not captured")
	}

	projectsDir, err := a.claudeProjectsDir()
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread: %w", err)
	}
	srcPath, err := sessionfork.LocateSessionFile(projectsDir, source.SessionRef, source.WorkspacePath)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread: locate claude session: %w", err)
	}

	// Prefer UUID-keyed slicing when the user_text at turn
	// `*atTurnIndex+1` carries a stamped wire UUID — the slice is then
	// immune to synthetic-entry ordinal drift (e.g. /compact). Falls
	// back to the ordinal walk for legacy rows that pre-date the
	// stamp.
	newID, newPath, err := a.writeForkedClaudeSession(srcPath, source.ID, *atTurnIndex)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread: write forked session: %w", err)
	}
	return forkResumeState{
		SessionRef: newID,
		Cleanup: func() error {
			// Best-effort: a missing file is OK (already cleaned up elsewhere).
			if err := os.Remove(newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("fork thread cleanup: remove %s: %w", newPath, err)
			}
			return nil
		},
	}, nil
}

// claudeMidTurnCut is the transcript cut for a Claude tail fork,
// resolved BEFORE the SQLite fork so the pin and the fork's timeline
// describe the same moment (see ForkThread). A zero SourcePath or Leaf
// is the sanctioned degenerate case: the fork starts a FRESH provider
// thread on its first send and the inherited prompt is its whole
// transcript.
type claudeMidTurnCut struct {
	SessionRef    string
	WorkspacePath string
	SourcePath    string
	Leaf          string
	// ProjectsDir is the provider-home-resolved `<home>/.claude/projects`
	// SourcePath was located under (App.claudeProjectsDir). Carried on the
	// cut so the cold scan reads the same home the locate did.
	ProjectsDir string
}

func (c claudeMidTurnCut) degenerate() bool {
	return c.SourcePath == "" || c.Leaf == ""
}

// captureClaudeMidTurnCut resolves where a mid-turn tail fork will cut
// the source transcript. It runs before the SQLite fork and performs no
// writes.
//
// The leaf comes from the live session's stdout tracker
// (CanonicalLeafUUID). When no session is registered — an open turn row
// with a dead process, the crash window before the boot sweep settles
// it — the cold scan over the on-disk file answers instead; that is
// exactly what ScanSessionLeaf is for.
//
// Exactly three shapes are allowed to answer "no cut", and each is a
// genuinely-early state rather than a failure:
//
//   - the thread has no Claude session reference yet;
//   - LocateSessionFile reports ErrSessionFileNotFound (the CLI has not
//     created the transcript yet);
//   - ScanSessionLeaf SUCCEEDS and finds no settled leaf in the file.
//
// Every other error — a projects-dir/home resolution failure, a stat or
// open error, a transcript over the scanner's size or row bounds —
// FAILS the fork. Those are real I/O faults, and laundering one into a
// context-less fork with a fully-populated timeline would hand the user
// a thread that silently lost its history (core principle 5), while the
// idle and anchored sibling paths hard-error on the very same class.
func (a *App) captureClaudeMidTurnCut(source store.Thread) (claudeMidTurnCut, error) {
	const op = "fork thread mid-turn"

	sourceRef := source.ResolvedSessionRef()
	if sourceRef == "" {
		log.Printf("%s: thread %s has no Claude session reference yet — fork starts a fresh provider thread", op, source.ID)
		return claudeMidTurnCut{}, nil
	}
	projectsDir, err := a.claudeProjectsDir()
	if err != nil {
		return claudeMidTurnCut{}, fmt.Errorf("%s: %w", op, err)
	}
	srcPath, err := sessionfork.LocateSessionFile(projectsDir, sourceRef, source.WorkspacePath)
	if err != nil {
		if errors.Is(err, sessionfork.ErrSessionFileNotFound) {
			log.Printf("%s: thread %s session %s not on disk yet — fork starts a fresh provider thread", op, source.ID, sourceRef)
			return claudeMidTurnCut{}, nil
		}
		return claudeMidTurnCut{}, fmt.Errorf("%s: locate claude session %s: %w", op, sourceRef, err)
	}

	cut := claudeMidTurnCut{SessionRef: sourceRef, WorkspacePath: source.WorkspacePath, SourcePath: srcPath, ProjectsDir: projectsDir}
	// A fork of an unstarted fork inherits its saved cut, even if the
	// original source has gained more messages since that cut was taken.
	if source.PendingForkRef != "" && source.PendingForkResumeAt != "" {
		cut.Leaf = source.PendingForkResumeAt
		return cut, nil
	}
	if sess, ok := a.activeClaudeSession(source.ID); ok {
		if leaf := sess.CanonicalLeafUUID(); leaf != "" {
			cut.Leaf = leaf
			return cut, nil
		}
	}
	// The cold scan is deliberately the fallback: it re-reads the whole
	// file, and the live tracker answers on the overwhelmingly common
	// path.
	leaf, err := a.scanClaudeSessionLeaf(op, cut)
	if err != nil {
		return claudeMidTurnCut{}, err
	}
	if leaf == "" {
		log.Printf("%s: thread %s session %s has no settled leaf — fork starts a fresh provider thread", op, source.ID, sourceRef)
		return claudeMidTurnCut{}, nil
	}
	cut.Leaf = leaf
	return cut, nil
}

// scanClaudeSessionLeaf is the cold-scan read, with the failure/absence
// split captureClaudeMidTurnCut documents: ("", nil) means the file
// parsed and holds no settled leaf; an error is a real I/O fault (stat,
// open, over the scanner's byte or row bound) and fails the fork.
func (a *App) scanClaudeSessionLeaf(op string, cut claudeMidTurnCut) (string, error) {
	state, err := claude.ScanSessionLeaf(cut.ProjectsDir, cut.SessionRef, cut.WorkspacePath)
	if err != nil {
		return "", fmt.Errorf("%s: scan claude session leaf for %s: %w", op, cut.SessionRef, err)
	}
	return state.CanonicalLeafUUID, nil
}

func (a *App) forkClaudeThreadBeforeMessage(source store.Thread, anchor store.MessageAnchor, anchorItem store.Item) (forkResumeState, error) {
	midTurn, err := claudeMidTurnAnchor(anchorItem)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread from message: %w", err)
	}
	// Only an anchor that OPENS turn 0 keeps nothing — the fork then
	// starts a fresh provider session. A mid-turn-0 anchor (a message
	// queued during the very first turn) keeps that turn's prefix and
	// needs the session slice like any later anchor.
	if anchor.TurnIndex == 0 && !midTurn {
		return forkResumeState{}, nil
	}
	sourceSessionRef := source.ResolvedSessionRef()
	if sourceSessionRef == "" {
		return forkResumeState{}, fmt.Errorf("fork thread from message: source thread %q is missing a Claude session reference", source.ID)
	}
	projectsDir, err := a.claudeProjectsDir()
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread from message: %w", err)
	}
	srcPath, err := sessionfork.LocateSessionFile(projectsDir, sourceSessionRef, source.WorkspacePath)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread from message: locate claude session: %w", err)
	}
	newID, newPath, err := a.writeMessageForkedClaudeSession(srcPath, anchor, anchorItem, midTurn)
	if err != nil {
		return forkResumeState{}, fmt.Errorf("fork thread from message: write forked session: %w", err)
	}
	return forkResumeState{
		SessionRef: newID,
		Cleanup: func() error {
			if err := os.Remove(newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("fork thread from message cleanup: remove %s: %w", newPath, err)
			}
			return nil
		},
	}, nil
}

// writeForkedClaudeSession is the turn-keyed-fork call into
// writeClaudeSessionSlice. The slice anchor is the user_text at
// turn `atTurnIndex+1` — that is the first turn dropped from the
// fork, so its parent is the end of the last kept turn.
func (a *App) writeForkedClaudeSession(srcPath, sourceThreadID string, atTurnIndex int) (string, string, error) {
	anchorUUID := a.lookupTurnAnchorClaudeUUID(sourceThreadID, atTurnIndex+1)
	logCtx := fmt.Sprintf("fork thread (turn %d)", atTurnIndex+1)
	// Turn-keyed forks anchor at a turn boundary by construction, so the
	// ordinal fallback's whole-turn granularity is exact here, and the
	// already-cut parent retry never applies (there is no parent).
	return writeClaudeSessionSlice(srcPath, anchorUUID, "", atTurnIndex, false, logCtx)
}

// writeMessageForkedClaudeSession is the message-keyed-fork call
// into writeClaudeSessionSlice. The slice anchor is the dropped user
// message's wire uuid (claudeSliceAnchorUUID); midTurnAnchor comes from
// the anchor item's position, same as the un-send path.
func (a *App) writeMessageForkedClaudeSession(srcPath string, anchor store.MessageAnchor, anchorItem store.Item, midTurnAnchor bool) (string, string, error) {
	return writeClaudeSessionSlice(
		srcPath, claudeSliceAnchorUUID(anchor, anchorItem), claudeSliceParentUUID(anchor, anchorItem),
		anchor.TurnIndex-1, midTurnAnchor, "fork thread from message",
	)
}

// lookupTurnAnchorClaudeUUID returns the wire UUID stamped on the
// user_text row at turnIndex, or "" if no such row carries a
// stable id. Used by the fork-slice helpers to pick the UUID-keyed
// branch when available.
func (a *App) lookupTurnAnchorClaudeUUID(threadID string, turnIndex int) string {
	items, err := a.store.ListItemsForTurn(threadID, turnIndex)
	if err != nil {
		log.Printf("fork thread: load turn %d items for anchor lookup: %v", turnIndex, err)
		return ""
	}
	for _, it := range items {
		if it.Kind != "user_text" || it.Role != "user" {
			continue
		}
		if store.IsWireOnlyUserItem(it) {
			// Cascade-injected user rows (task_notification echo,
			// future Codex MCP injection) are mid-turn anchors that
			// don't bound a turn boundary — skip them so the lookup
			// picks the AO-authored row that opens the turn.
			continue
		}
		if id := usermessage.ReadProviderItemID(it.Meta); id != "" {
			return id
		}
	}
	return ""
}

// activeClaudeSession is the Claude sibling of activeCodexSession. The
// mid-turn fork reads CanonicalLeafUUID off it — the live stdout
// tracker's answer, which is ahead of the transcript file by however
// long the CLI takes to append.
func (a *App) activeClaudeSession(threadID string) (*claude.Session, bool) {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.Claude == nil {
		return nil, false
	}
	return sess.Claude, true
}
