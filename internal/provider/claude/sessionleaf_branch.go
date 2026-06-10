package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"agent-overflow/internal/provider/claude/sessionfork"
)

// claudeBranchIndex accumulates the session file's parentUuid graph
// during a cold scan so the chosen resume-at leaf can be validated
// against the ACTIVE BRANCH — the chain Claude itself reconstructs by
// walking parentUuid back from the file's LAST uuid-bearing transcript
// row. `--resume-session-at` is validated by the CLI against that
// branch only; passing any other uuid hard-fails at startup ("No
// message found with message.uuid of: ...", emitted as a pre-init
// result{error_during_execution}).
//
// The graph and file order can disagree because the CLI writes some
// rows late with stale parents — deferred `system/api_error` rows are
// the known case (incident 2026-06-10; see sessionfork/rechain.go and
// claude-wire.md §"Session JSONL: deferred system/api_error rows").
// The file-order leaf the tracker picks is then OFF the branch, and a
// scanner that doesn't check would brick every resume of the session.
//
// Row admission mirrors the fork transform's transcript set
// (user/assistant/attachment/system/progress with a non-empty uuid,
// sidechains excluded): empirically, claude's own walk traverses
// system rows (the api_error rows WERE the branch tip in the incident
// file) and ignores non-transcript furniture (custom-title / mode /
// queue-operation rows never broke a verified resume).
//
// Memory: two maps bounded by the scan's existing row cap
// (maxClaudeSessionLeafRows) — cold path only.
type claudeBranchIndex struct {
	parentByUUID  map[string]string
	contentByUUID map[string]struct{}
	tipUUID       string
}

func newClaudeBranchIndex() *claudeBranchIndex {
	return &claudeBranchIndex{
		parentByUUID:  make(map[string]string),
		contentByUUID: make(map[string]struct{}),
	}
}

// alreadySeen mirrors claudeLeafTracker's uuid dedup: replay-echo rows
// re-persist an existing uuid at the file tail (observed for isReplay
// assistant envelopes), and letting the duplicate redefine the branch
// tip would point the walk at a stale interior chain. First sighting
// wins for both the parent edge and tip selection.
func (b *claudeBranchIndex) alreadySeen(uuid string) bool {
	_, ok := b.parentByUUID[uuid]
	return ok
}

func (b *claudeBranchIndex) ingestLine(line []byte) {
	var row claudeSessionRow
	if err := json.Unmarshal(line, &row); err != nil {
		return
	}
	b.ingestRow(row)
}

// ingestRow is the decode-free core of ingestLine, shared with
// scanSessionLeafReader's single-decode path. Row admission uses
// sessionfork.TranscriptTypes — the fork transform and this validator
// must agree on what claude's walk sees (invariant 28).
func (b *claudeBranchIndex) ingestRow(row claudeSessionRow) {
	if row.IsSidechain {
		return
	}
	if _, ok := sessionfork.TranscriptTypes[row.Type]; !ok {
		return
	}
	uuid := strings.TrimSpace(row.UUID)
	if uuid == "" || b.alreadySeen(uuid) {
		return
	}
	b.parentByUUID[uuid] = strings.TrimSpace(row.ParentUUID)
	if row.Type == "user" || row.Type == "assistant" {
		b.contentByUUID[uuid] = struct{}{}
	}
	b.tipUUID = uuid
}

// onActiveBranch reports whether uuid is reachable by walking
// parentUuid from the file's last uuid-bearing transcript row. The
// step bound guards against parent cycles in a corrupt file: an
// acyclic chain can visit at most every indexed row once, so walking
// past len(parentByUUID) steps proves a cycle without allocating a
// visited set.
func (b *claudeBranchIndex) onActiveBranch(uuid string) bool {
	if uuid == "" {
		return false
	}
	maxSteps := len(b.parentByUUID)
	cur := b.tipUUID
	for steps := 0; cur != "" && steps <= maxSteps; steps++ {
		if cur == uuid {
			return true
		}
		cur = b.parentByUUID[cur]
	}
	return false
}

// deepestContentOnBranch walks from the tip toward the root and
// returns the first user/assistant row that is not excluded — the
// deepest valid resume-at cursor on the active branch. Returns ""
// when the branch holds no usable content row (callers then omit
// --resume-session-at entirely, claude's own default-leaf semantics).
// Same step-bound cycle guard as onActiveBranch.
func (b *claudeBranchIndex) deepestContentOnBranch(exclude map[string]struct{}) string {
	maxSteps := len(b.parentByUUID)
	cur := b.tipUUID
	for steps := 0; cur != "" && steps <= maxSteps; steps++ {
		if _, isContent := b.contentByUUID[cur]; isContent {
			if _, excluded := exclude[cur]; !excluded {
				return cur
			}
		}
		cur = b.parentByUUID[cur]
	}
	return ""
}

// repairLeafForActiveBranch validates the tracker's file-order leaf
// against the active branch and substitutes the deepest on-branch
// user/assistant row when the pick is off-branch. Healthy files (the
// overwhelming majority) take the no-op path: their file-order leaf IS
// the branch leaf.
func repairLeafForActiveBranch(state SessionLeafState, idx *claudeBranchIndex) SessionLeafState {
	if state.CanonicalLeafUUID == "" || idx.onActiveBranch(state.CanonicalLeafUUID) {
		return state
	}
	exclude := make(map[string]struct{}, len(state.UnresolvedServerToolUUIDs))
	for _, uuid := range state.UnresolvedServerToolUUIDs {
		exclude[uuid] = struct{}{}
	}
	state.CanonicalLeafUUID = idx.deepestContentOnBranch(exclude)
	return state
}

// ResumeAtOnActiveBranch reports whether resumeAt is a user/assistant
// row on the active parentUuid branch of the session's JSONL — i.e. a
// uuid that `claude --resume <sessionID> --resume-session-at <resumeAt>`
// will accept. Used by the spawn path to validate EXPLICIT resume-at
// cursors (the live-tracker context-repair restart passes a
// wire-derived leaf that can disagree with the file). See invariant 28.
//
// Unlike repairLeafForActiveBranch, this check does NOT screen the
// cursor against unresolved server-tool rows: the only explicit-cursor
// producer is the live claudeLeafTracker, which never advances its
// canonical leaf onto an assistant row with unresolved server tools in
// the first place. If a new explicit-cursor source appears, it must
// either inherit that guarantee or this validator grows the exclusion.
func ResumeAtOnActiveBranch(sessionID, workspacePath, resumeAt string) (bool, error) {
	resumeAt = strings.TrimSpace(resumeAt)
	if resumeAt == "" {
		return false, fmt.Errorf("claude: empty resume-at uuid")
	}
	path, err := sessionfork.LocateSessionFile(sessionID, workspacePath)
	if err != nil {
		return false, err
	}
	idx, err := scanBranchIndexFile(path)
	if err != nil {
		return false, err
	}
	if _, isContent := idx.contentByUUID[resumeAt]; !isContent {
		return false, nil
	}
	return idx.onActiveBranch(resumeAt), nil
}

// scanBranchIndexFile builds a claudeBranchIndex from the session JSONL
// at path, under the same size/row caps the leaf scan enforces.
func scanBranchIndexFile(path string) (*claudeBranchIndex, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("claude: stat session file for branch index: %w", err)
	}
	if st.Size() > maxClaudeSessionLeafFileBytes {
		return nil, fmt.Errorf("claude: session file too large for branch index: %d bytes", st.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("claude: open session file for branch index: %w", err)
	}
	defer f.Close()

	idx := newClaudeBranchIndex()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), maxClaudeSessionLeafLineBytes)
	rows := 0
	for scanner.Scan() {
		rows++
		if rows > maxClaudeSessionLeafRows {
			return nil, fmt.Errorf("claude: session file has more than %d rows during branch index scan", maxClaudeSessionLeafRows)
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		idx.ingestLine(line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("claude: scan branch index: %w", err)
	}
	return idx, nil
}
