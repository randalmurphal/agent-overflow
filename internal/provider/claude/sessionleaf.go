package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"agent-overflow/internal/provider/claude/sessionfork"
)

const (
	maxClaudeSessionLeafLineBytes = 32 * 1024 * 1024
	maxClaudeSessionLeafFileBytes = 512 * 1024 * 1024
	maxClaudeSessionLeafRows      = 500_000
)

// SessionLeafState is the Claude transcript continuation state AO needs
// before resuming or sending into a session. CanonicalLeafUUID is the latest
// settled top-level transcript row. UnresolvedServerToolUUIDs are assistant
// rows that contain server-side tool calls without a matching server-side
// result; they are transcript history, but not valid continuation leaves.
type SessionLeafState struct {
	CanonicalLeafUUID              string
	UnresolvedServerToolUUIDs      []string
	RequiresResumeAtBeforeUserSend bool
}

type claudeLeafTracker struct {
	mu sync.Mutex

	canonicalLeafUUID string
	seenUUIDs         map[string]struct{}
	pendingByToolID   map[string]string
	unresolvedByUUID  map[string]map[string]struct{}
	requiresResumeAt  bool
}

func newClaudeLeafTracker(seedCanonicalLeaf string) *claudeLeafTracker {
	return &claudeLeafTracker{
		canonicalLeafUUID: strings.TrimSpace(seedCanonicalLeaf),
		seenUUIDs:         make(map[string]struct{}),
		pendingByToolID:   make(map[string]string),
		unresolvedByUUID:  make(map[string]map[string]struct{}),
	}
}

func (t *claudeLeafTracker) ingestLine(line []byte) {
	if t == nil {
		return
	}
	var env struct {
		Type            string          `json:"type"`
		UUID            string          `json:"uuid"`
		ParentUUID      string          `json:"parentUuid"`
		ParentToolUseID string          `json:"parent_tool_use_id"`
		IsSidechain     bool            `json:"isSidechain"`
		Message         json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	if env.IsSidechain {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	switch env.Type {
	case "assistant":
		t.ingestAssistantLocked(env.UUID, env.ParentUUID, env.ParentToolUseID, env.Message)
	case "user":
		t.ingestUserLocked(env.UUID)
	case "result":
		t.markTurnCompleteLocked()
	}
}

func (t *claudeLeafTracker) ingestUserLocked(uuid string) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" || t.alreadySeenLocked(uuid) {
		return
	}
	t.markSeenLocked(uuid)
	t.canonicalLeafUUID = uuid
}

func (t *claudeLeafTracker) ingestAssistantLocked(uuid, parentUUID, parentToolUseID string, message json.RawMessage) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" || strings.TrimSpace(parentToolUseID) != "" || t.alreadySeenLocked(uuid) {
		return
	}
	t.markSeenLocked(uuid)

	var msg struct {
		Content []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	}
	if len(message) > 0 {
		if err := json.Unmarshal(message, &msg); err != nil {
			return
		}
	}

	serverResultIDs := make(map[string]struct{})
	for _, block := range msg.Content {
		toolUseID := strings.TrimSpace(block.ToolUseID)
		if toolUseID != "" {
			serverResultIDs[toolUseID] = struct{}{}
		}
	}
	for _, block := range msg.Content {
		switch block.Type {
		case "server_tool_use", "mcp_tool_use":
			toolUseID := strings.TrimSpace(block.ID)
			if toolUseID == "" {
				continue
			}
			if _, hasResultInSameMessage := serverResultIDs[toolUseID]; hasResultInSameMessage {
				continue
			}
			t.pendingByToolID[toolUseID] = uuid
			if t.unresolvedByUUID[uuid] == nil {
				t.unresolvedByUUID[uuid] = make(map[string]struct{})
			}
			t.unresolvedByUUID[uuid][toolUseID] = struct{}{}
		}
	}

	// Claude treats any assistant content block with tool_use_id as a
	// server-side tool result, including advisor_tool_result,
	// web_search_tool_result, and MCP result variants.
	for toolUseID := range serverResultIDs {
		t.resolveServerToolUseLocked(toolUseID)
	}

	if len(t.unresolvedByUUID[uuid]) > 0 {
		return
	}
	t.canonicalLeafUUID = uuid
	if len(t.pendingByToolID) == 0 {
		t.requiresResumeAt = false
	}
}

func (t *claudeLeafTracker) resolveServerToolUseLocked(toolUseID string) {
	toolUseID = strings.TrimSpace(toolUseID)
	if toolUseID == "" {
		return
	}
	assistantUUID, ok := t.pendingByToolID[toolUseID]
	if !ok {
		return
	}
	delete(t.pendingByToolID, toolUseID)
	delete(t.unresolvedByUUID[assistantUUID], toolUseID)
	if len(t.unresolvedByUUID[assistantUUID]) == 0 {
		delete(t.unresolvedByUUID, assistantUUID)
	}
}

func (t *claudeLeafTracker) markTurnComplete() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.markTurnCompleteLocked()
}

func (t *claudeLeafTracker) markTurnCompleteLocked() {
	if len(t.pendingByToolID) > 0 && t.canonicalLeafUUID != "" {
		t.requiresResumeAt = true
	}
}

func (t *claudeLeafTracker) canonicalLeaf() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.canonicalLeafUUID
}

func (t *claudeLeafTracker) requiresResumeAtBeforeUserSend() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requiresResumeAt && t.canonicalLeafUUID != ""
}

func (t *claudeLeafTracker) stateForColdResume() SessionLeafState {
	if t == nil {
		return SessionLeafState{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	requires := t.requiresResumeAt
	if len(t.pendingByToolID) > 0 && t.canonicalLeafUUID != "" {
		requires = true
	}
	return SessionLeafState{
		CanonicalLeafUUID:              t.canonicalLeafUUID,
		UnresolvedServerToolUUIDs:      sortedServerToolUUIDs(t.unresolvedByUUID),
		RequiresResumeAtBeforeUserSend: requires,
	}
}

func (t *claudeLeafTracker) alreadySeenLocked(uuid string) bool {
	_, ok := t.seenUUIDs[uuid]
	return ok
}

func (t *claudeLeafTracker) markSeenLocked(uuid string) {
	t.seenUUIDs[uuid] = struct{}{}
}

func sortedServerToolUUIDs(refs map[string]map[string]struct{}) []string {
	if len(refs) == 0 {
		return nil
	}
	uuids := make([]string, 0, len(refs))
	for uuid := range refs {
		uuids = append(uuids, uuid)
	}
	sort.Strings(uuids)
	return uuids
}

// ScanSessionLeaf reconstructs the canonical settled leaf for a Claude
// session JSONL. It is intended for cold start/recovery only; live sessions
// use claudeLeafTracker fed directly from stdout.
func ScanSessionLeaf(sessionID, workspacePath string) (SessionLeafState, error) {
	path, err := sessionfork.LocateSessionFile(sessionID, workspacePath)
	if err != nil {
		return SessionLeafState{}, err
	}
	return scanSessionLeafFile(path)
}

func scanSessionLeafFile(path string) (SessionLeafState, error) {
	st, err := os.Stat(path)
	if err != nil {
		return SessionLeafState{}, fmt.Errorf("claude: stat session leaf file: %w", err)
	}
	if st.Size() > maxClaudeSessionLeafFileBytes {
		return SessionLeafState{}, fmt.Errorf("claude: session leaf file too large: %d bytes", st.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return SessionLeafState{}, fmt.Errorf("claude: open session leaf file: %w", err)
	}
	defer f.Close()

	return scanSessionLeafReader(f)
}

func scanSessionLeafReader(r io.Reader) (SessionLeafState, error) {
	tracker := newClaudeLeafTracker("")
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), maxClaudeSessionLeafLineBytes)
	rows := 0
	for scanner.Scan() {
		rows++
		if rows > maxClaudeSessionLeafRows {
			return SessionLeafState{}, fmt.Errorf("claude: session leaf file has more than %d rows", maxClaudeSessionLeafRows)
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		tracker.ingestLine(line)
	}
	if err := scanner.Err(); err != nil {
		return SessionLeafState{}, fmt.Errorf("claude: scan session leaf: %w", err)
	}
	return tracker.stateForColdResume(), nil
}

func findReplayUserParent(sessionID, workspacePath, replayUUID string) (string, bool, error) {
	replayUUID = strings.TrimSpace(replayUUID)
	if replayUUID == "" {
		return "", false, nil
	}
	path, err := sessionfork.LocateSessionFile(sessionID, workspacePath)
	if err != nil {
		return "", false, err
	}
	if st, err := os.Stat(path); err != nil {
		return "", false, fmt.Errorf("claude: stat session file for replay parent: %w", err)
	} else if st.Size() > maxClaudeSessionLeafFileBytes {
		return "", false, fmt.Errorf("claude: session file too large for replay parent lookup: %d bytes", st.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, fmt.Errorf("claude: open session file for replay parent: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), maxClaudeSessionLeafLineBytes)
	rows := 0
	for scanner.Scan() {
		rows++
		if rows > maxClaudeSessionLeafRows {
			return "", false, fmt.Errorf("claude: session file has more than %d rows during replay parent lookup", maxClaudeSessionLeafRows)
		}
		line := scanner.Bytes()
		var env struct {
			Type       string `json:"type"`
			UUID       string `json:"uuid"`
			ParentUUID string `json:"parentUuid"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		if env.Type == "user" && env.UUID == replayUUID {
			return strings.TrimSpace(env.ParentUUID), true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("claude: scan replay parent: %w", err)
	}
	return "", false, nil
}
