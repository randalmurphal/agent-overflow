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
// against the rows that SURVIVE claude's resume load. Two properties
// decide whether `--resume-session-at <uuid>` is accepted:
//
//  1. The row must be on the ACTIVE BRANCH — the chain claude
//     reconstructs by walking parentUuid back from the file's last
//     uuid-bearing transcript row. The graph and file order can
//     disagree because the CLI writes some rows late with stale
//     parents — deferred `system/api_error` rows are the known case
//     (incident 2026-06-10; see sessionfork/rechain.go and
//     claude-wire.md §"Session JSONL: deferred system/api_error rows").
//
//  2. The row must survive the CLI's resume deserialization filters
//     (deserializeMessages, conversationRecovery.ts): assistant rows
//     whose client tool_use blocks all lack a tool_result, orphaned
//     thinking-only rows, and whitespace-only rows are dropped BEFORE
//     the CLI validates the cursor, so a uuid that is physically the
//     file's branch tip can still hard-fail resume ("No message found
//     with message.uuid of: ...", emitted as a pre-init
//     result{error_during_execution}). A host crash mid-tool-execution
//     produces exactly that tail (incident 2026-08-03: BSOD killed a
//     34-minute Bash, leaving the leaf a dangling tool_use row). The
//     filter mirror lives in sessionleaf_resumefilters.go.
//
// Row admission mirrors the fork transform's transcript set
// (user/assistant/attachment/system/progress with a non-empty uuid,
// sidechains excluded): empirically, claude's own walk traverses
// system rows (the api_error rows WERE the branch tip in the incident
// file) and ignores non-transcript furniture (custom-title / mode /
// queue-operation rows never broke a verified resume).
//
// Memory: one map bounded by the scan's existing row cap
// (maxClaudeSessionLeafRows) — cold path only.
type claudeBranchIndex struct {
	rows    map[string]*claudeBranchRow
	tipUUID string
	// nextSeq numbers rows in ingest (file) order. Chain order and file
	// order can disagree (late-written stale-parent rows), and the fork
	// pin repair needs the FILE-order bound: "at or before the pin"
	// means at or before its position in the transcript, not its depth
	// on the branch. See forkResumeCursorFromScan.
	nextSeq int
}

// claudeBranchRow is what the index keeps per transcript row: the
// parent edge for the branch walk plus the content-derived flags the
// resume-filter mirror needs. Raw message bytes are parsed immediately
// and discarded — claudeSessionRow.Message aliases the scanner's
// reusable buffer and must not outlive the line.
type claudeBranchRow struct {
	uuid    string
	parent  string
	rowType string
	isMeta  bool
	seq     int
	flags   claudeRowContentFlags
}

func newClaudeBranchIndex() *claudeBranchIndex {
	return &claudeBranchIndex{rows: make(map[string]*claudeBranchRow)}
}

// alreadySeen mirrors claudeLeafTracker's uuid dedup: replay-echo rows
// re-persist an existing uuid at the file tail (observed for isReplay
// assistant envelopes), and letting the duplicate redefine the branch
// tip would point the walk at a stale interior chain. First sighting
// wins for both the parent edge and tip selection.
func (b *claudeBranchIndex) alreadySeen(uuid string) bool {
	_, ok := b.rows[uuid]
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
	indexed := &claudeBranchRow{
		uuid:    uuid,
		parent:  strings.TrimSpace(row.ParentUUID),
		rowType: row.Type,
		isMeta:  row.IsMeta,
		seq:     b.nextSeq,
	}
	b.nextSeq++
	if row.Type == "user" || row.Type == "assistant" {
		indexed.flags = parseClaudeRowContentFlags(row.Message)
	}
	b.rows[uuid] = indexed
	b.tipUUID = uuid
}

// activeChain returns the active branch in root→tip order — the row
// sequence claude's own loadTranscriptFile → buildConversationChain
// walk materializes as the resumable message list. The step bound
// guards against parent cycles in a corrupt file: an acyclic chain can
// visit at most every indexed row once, so walking past len(rows)
// steps proves a cycle without allocating a visited set.
func (b *claudeBranchIndex) activeChain() []*claudeBranchRow {
	maxSteps := len(b.rows)
	chain := make([]*claudeBranchRow, 0, maxSteps)
	cur := b.tipUUID
	for steps := 0; cur != "" && steps <= maxSteps; steps++ {
		row, ok := b.rows[cur]
		if !ok {
			break
		}
		chain = append(chain, row)
		cur = row.parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// survivingResumeCursors runs the resume-filter mirror over the active
// chain and returns the set of uuids `--resume-session-at` will accept,
// plus the deepest such uuid (the natural repair pick). Rows in exclude
// are screened from both — the caller passes assistant rows with
// unresolved server-side tools, which the CLI's filters keep but which
// are not valid continuation leaves (the API rejects a dangling
// server_tool_use on the resumed context).
func (b *claudeBranchIndex) survivingResumeCursors(exclude map[string]struct{}) (map[string]struct{}, string) {
	survivors := applyClaudeResumeFilters(b.activeChain())
	set := make(map[string]struct{}, len(survivors))
	deepest := ""
	for _, s := range survivors {
		if !s.cursorSafe {
			continue
		}
		if _, excluded := exclude[s.uuid]; excluded {
			continue
		}
		set[s.uuid] = struct{}{}
		deepest = s.uuid
	}
	return set, deepest
}

// repairLeafForActiveBranch validates the tracker's file-order leaf
// against the set of cursors claude's resume will actually accept and
// substitutes the deepest surviving row when the pick would hard-fail.
// Healthy files (the overwhelming majority) take the no-op path: their
// file-order leaf IS the deepest surviving branch row.
func repairLeafForActiveBranch(state SessionLeafState, idx *claudeBranchIndex) SessionLeafState {
	if state.CanonicalLeafUUID == "" {
		return state
	}
	exclude := make(map[string]struct{}, len(state.UnresolvedServerToolUUIDs))
	for _, uuid := range state.UnresolvedServerToolUUIDs {
		exclude[uuid] = struct{}{}
	}
	set, deepest := idx.survivingResumeCursors(exclude)
	if _, ok := set[state.CanonicalLeafUUID]; ok {
		return state
	}
	state.CanonicalLeafUUID = deepest
	return state
}

// ResumeAtOnActiveBranch reports whether resumeAt is a cursor that
// `claude --resume <sessionID> --resume-session-at <resumeAt>` will
// accept: a user/assistant row on the active parentUuid branch of the
// session's JSONL that survives the CLI's resume deserialization
// filters. Used by the spawn path to validate EXPLICIT resume-at
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
	set, _ := idx.survivingResumeCursors(nil)
	_, ok := set[resumeAt]
	return ok, nil
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
