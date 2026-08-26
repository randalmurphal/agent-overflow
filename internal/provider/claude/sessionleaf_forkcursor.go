package claude

import (
	"fmt"
	"os"
	"strings"

	"agent-overflow/internal/provider/claude/sessionfork"
)

// ForkResumeCursor is the answer to "where may a pinned lazy fork cut
// the source transcript". Produced by ResolveForkResumeCursor.
//
// Cursor is the uuid to pass as `--resume-session-at`: the pin itself
// when the CLI's resume will accept it, otherwise the deepest
// filter-surviving row at or before the pin's file position. Empty
// means the file (or its prefix up to the pin) holds no row the CLI
// would accept as a cursor.
//
// PinOnDisk reports whether the pin uuid was found in the transcript at
// all. False is the stdout-to-disk append gap: the leaf was observed on
// the live wire but the CLI has not appended its row yet, so the caller
// should wait briefly and re-resolve. In that state Cursor is the
// deepest surviving row of the file as it stands — every row on disk
// precedes a row that is not on disk, so using it after the wait is
// exhausted cuts slightly EARLIER than the pin, never later.
type ForkResumeCursor struct {
	Cursor    string
	PinOnDisk bool
}

// ResolveForkResumeCursor computes the `--resume-session-at` cursor for
// a fork start whose cut was pinned at fork time (a mid-turn tail fork:
// PendingForkRef = the source session, PendingForkResumeAt = the leaf
// captured when Fork was clicked). The CLI applies its resume
// deserialization filters BEFORE the cursor lookup, so a pin that is
// physically in the file can still hard-fail resume pre-init ("No
// message found with message.uuid of: ..." — spike-verified on 2.1.237
// with a dangling tool_use leaf, which is exactly the row a mid-turn
// capture tends to land on). Repairing here, against the SOURCE file at
// spawn time, is what turns the pin into a cursor the CLI will accept.
//
// The repair never moves the cut FORWARD: a substitute cursor must sit
// at or before the pin's position in the file, because the source may
// have kept streaming since the fork and rows behind the pin belong to
// the source's future, not the fork's snapshot. "Position" is file
// order (ingest seq), not branch depth — late-written stale-parent rows
// make the two disagree.
//
// Errors are real I/O faults (locate, stat, open, a transcript over the
// scanner's bounds) and should fail the session start loudly; the
// missing-pin case is NOT an error — it returns PinOnDisk=false so the
// caller can wait out the append gap.
func ResolveForkResumeCursor(projectsDir, sessionID, workspacePath, pin string) (ForkResumeCursor, error) {
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return ForkResumeCursor{}, fmt.Errorf("claude: empty fork resume pin")
	}
	path, err := sessionfork.LocateSessionFile(projectsDir, sessionID, workspacePath)
	if err != nil {
		return ForkResumeCursor{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return ForkResumeCursor{}, fmt.Errorf("claude: stat session file for fork cursor: %w", err)
	}
	if st.Size() > maxClaudeSessionLeafFileBytes {
		return ForkResumeCursor{}, fmt.Errorf("claude: session file too large for fork cursor: %d bytes", st.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return ForkResumeCursor{}, fmt.Errorf("claude: open session file for fork cursor: %w", err)
	}
	defer f.Close()

	tracker, branch, err := scanSessionTrackerAndBranch(f)
	if err != nil {
		return ForkResumeCursor{}, err
	}
	return forkResumeCursorFromScan(tracker.stateForColdResume(), branch, pin), nil
}

// forkResumeCursorFromScan is the pure half of ResolveForkResumeCursor.
// It walks the filter survivors of the active branch in root→tip order
// and keeps the deepest cursor-safe row bounded by the pin's file
// position; the pin itself wins outright when it survives. Rows with
// unresolved server-side tools are screened exactly as
// repairLeafForActiveBranch screens them — the CLI's filters keep them
// but the API rejects a resume onto one.
func forkResumeCursorFromScan(state SessionLeafState, idx *claudeBranchIndex, pin string) ForkResumeCursor {
	exclude := make(map[string]struct{}, len(state.UnresolvedServerToolUUIDs))
	for _, uuid := range state.UnresolvedServerToolUUIDs {
		exclude[uuid] = struct{}{}
	}
	pinRow, pinOnDisk := idx.rows[pin]

	best := ""
	for _, s := range applyClaudeResumeFilters(idx.activeChain()) {
		if !s.cursorSafe {
			continue
		}
		if _, excluded := exclude[s.uuid]; excluded {
			continue
		}
		if s.uuid == pin {
			return ForkResumeCursor{Cursor: pin, PinOnDisk: true}
		}
		if pinOnDisk {
			row, ok := idx.rows[s.uuid]
			if !ok || row.seq > pinRow.seq {
				continue
			}
		}
		best = s.uuid
	}
	return ForkResumeCursor{Cursor: best, PinOnDisk: pinOnDisk}
}
