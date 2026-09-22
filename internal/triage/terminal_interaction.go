package triage

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Empty terminal polls update process state only. Non-empty stdin records a
// completed interaction without retaining potentially sensitive input bytes.

// terminalInteractionMeta is the Meta shape populated by
// buildTerminalInteractionMeta in the Codex parser. Only the fields we
// actually read are listed; unknown keys are tolerated.
type terminalInteractionMeta struct {
	ProcessID string `json:"process_id"`
	Stdin     string `json:"stdin"`
}

func decodeTerminalInteractionMeta(raw json.RawMessage) (terminalInteractionMeta, error) {
	var decoded terminalInteractionMeta
	if len(raw) == 0 {
		return decoded, nil
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return decoded, fmt.Errorf("decode terminal interaction: %w", err)
	}
	return decoded, nil
}

func (r *Router) handleTerminalInteraction(evt provider.ProviderEvent) error {
	meta, err := decodeTerminalInteractionMeta(evt.Meta)
	if err != nil {
		return err
	}
	// The notification arrives after the poll returns, potentially after its
	// turn was interrupted. Preserve process state without creating history.
	r.markCodexUnifiedExecProcessBackgrounded(evt.ThreadID, meta.ProcessID)
	if evt.Content == "" && meta.Stdin == "" {
		return nil
	}
	turnIndex, ok := r.openTurnIndex(evt.ThreadID)
	if !ok || !r.hasCodexBackgroundTerminalForProcess(evt.ThreadID, meta.ProcessID) {
		return nil
	}
	now := eventTimestampMillis(evt)
	seq := r.nextTerminalInteractionSequence(evt.ThreadID, turnIndex, meta.ProcessID)
	metaMap := map[string]any{
		"process_id": meta.ProcessID,
		"kind":       "terminal_interaction",
		"has_stdin":  true,
	}
	if command := r.codexTerminalCommandForProcess(evt.ThreadID, meta.ProcessID); command != "" {
		metaMap["command"] = command
	}
	metaBlob, err := json.Marshal(metaMap)
	if err != nil {
		return fmt.Errorf("terminal interaction metadata: %w", err)
	}
	summary := "Interacted with background terminal"
	if command := r.codexTerminalSummaryForProcess(evt.ThreadID, meta.ProcessID); command != "" {
		summary += ": " + command
	}
	return r.persistItem(store.Item{
		ID:        fmt.Sprintf("interacted:%s:%d:%d", meta.ProcessID, turnIndex, seq),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      string(provider.ItemTerminalInteraction),
		Role:      "assistant",
		Status:    statusCompleted,
		Summary:   summary,
		ParentID:  eventParentID(evt),
		Meta:      string(metaBlob),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil)
}

// nextTerminalInteractionSequence returns a monotonically-increasing counter
// per (thread, turn, processID). Stored on the Router so it survives
// clearOpenTurn and multi-result boundaries; CleanupThread / selective
// turn re-init reset sweep the key by prefix.
func (r *Router) nextTerminalInteractionSequence(threadID string, turnIndex int, processID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(threadID)
	if st.terminalInteractionSeq == nil {
		st.terminalInteractionSeq = make(map[string]int)
	}
	key := terminalInteractionSeqKey(turnIndex, processID)
	seq := st.terminalInteractionSeq[key]
	st.terminalInteractionSeq[key] = seq + 1
	return seq
}

// terminalInteractionSeqKey builds the key the sequence counter is stored
// under WITHIN the thread's state. Mirrors the `<turn>|<scope>` shape
// used by the other per-turn counters there.
func terminalInteractionSeqKey(turnIndex int, processID string) string {
	return fmt.Sprintf("%d|%s", turnIndex, processID)
}
