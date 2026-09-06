package itemmeta

import (
	"strings"
	"testing"
)

func TestTransferCodexSessionsRewritesOnlyExecutionReferences(t *testing.T) {
	raw := `{"large":9007199254740993,"agent_path":"old","codex_child_terminal_statuses":{"old":"completed"},"codex_child_resume_generations":{"old":3},"input":{"tool":"spawn_agent","receiverThreadIds":["old"],"receiverAgents":[{"threadId":"old","agentNickname":"old"}],"agentsStates":{"old":{"status":"completed","message":"old"}},"prompt":"old"}}`
	got, err := TransferCodexSessions(raw, map[string]string{"old": "new"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`"large":9007199254740993`, `"agent_path":"new"`, `"receiverThreadIds":["new"]`, `"threadId":"new"`, `"agentNickname":"old"`, `"prompt":"old"`, `"message":"old"`, `"new":3`} {
		if !strings.Contains(got, value) {
			t.Errorf("missing %s in %s", value, got)
		}
	}
	unrelated := `{"input":{"tool":"exec_command","receiverThreadIds":["old"],"prompt":"old"}}`
	got, err = TransferCodexSessions(unrelated, map[string]string{"old": "new"})
	if err != nil || strings.Contains(got, "new") {
		t.Fatalf("rewrote unrelated command: %s %v", got, err)
	}
	if _, err := TransferCodexSessions(`{"codex_child_terminal_statuses":{"old":"completed","new":"failed"}}`, map[string]string{"old": "new"}); err == nil {
		t.Fatal("accepted colliding child ownership")
	}
}
