package claude

import (
	"strings"
	"testing"
)

func TestSessionLeafScannerIgnoresOrphanAdvisorAndDuplicateReplay(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"start"}}`,
		`{"type":"assistant","uuid":"a-tools","parentUuid":"u1","message":{"id":"msg-tools","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Agent","input":{}},{"type":"tool_use","id":"tool-2","name":"Agent","input":{}}]}}`,
		`{"type":"assistant","uuid":"a-advisor","parentUuid":"a-tools","message":{"id":"msg-advisor","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}`,
		`{"type":"user","uuid":"u-tool-1","parentUuid":"a-tools","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"done"}]}}`,
		`{"type":"user","uuid":"u-tool-2","parentUuid":"u-tool-1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-2","content":"done"}]}}`,
		`{"type":"assistant","uuid":"a-final","parentUuid":"u-tool-2","message":{"id":"msg-final","role":"assistant","content":[{"type":"text","text":"final"}]}}`,
		`{"type":"result","subtype":"success"}`,
		`{"type":"assistant","uuid":"a-advisor","parentUuid":"a-tools","message":{"id":"msg-advisor","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}`,
	}, "\n")

	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a-final" {
		t.Fatalf("CanonicalLeafUUID = %q, want a-final", state.CanonicalLeafUUID)
	}
	if !state.RequiresResumeAtBeforeUserSend {
		t.Fatal("RequiresResumeAtBeforeUserSend = false, want true")
	}
	if got, want := strings.Join(state.UnresolvedServerToolUUIDs, ","), "a-advisor"; got != want {
		t.Fatalf("UnresolvedServerToolUUIDs = %q, want %q", got, want)
	}
}

func TestSessionLeafScannerHealthyAdvisorResolves(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"advise"}}`,
		`{"type":"assistant","uuid":"a-advisor","parentUuid":"u1","message":{"id":"msg-advisor","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}`,
		`{"type":"assistant","uuid":"a-advisor-result","parentUuid":"a-advisor","message":{"id":"msg-advisor-result","role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"advisor_result","text":"ok"}}]}}`,
		`{"type":"assistant","uuid":"a-final","parentUuid":"a-advisor-result","message":{"id":"msg-final","role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success"}`,
	}, "\n")

	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a-final" {
		t.Fatalf("CanonicalLeafUUID = %q, want a-final", state.CanonicalLeafUUID)
	}
	if state.RequiresResumeAtBeforeUserSend {
		t.Fatal("RequiresResumeAtBeforeUserSend = true, want false")
	}
	if len(state.UnresolvedServerToolUUIDs) != 0 {
		t.Fatalf("UnresolvedServerToolUUIDs = %v, want empty", state.UnresolvedServerToolUUIDs)
	}
}

func TestSessionLeafScannerIgnoresSidechainRows(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"start"}}`,
		`{"type":"assistant","uuid":"a-final","parentUuid":"u1","message":{"id":"msg-final","role":"assistant","content":[{"type":"text","text":"final"}]}}`,
		`{"type":"user","uuid":"side-u","parentUuid":"a-final","isSidechain":true,"message":{"role":"user","content":"side"}}`,
		`{"type":"assistant","uuid":"side-a","parentUuid":"side-u","isSidechain":true,"message":{"id":"side-msg","role":"assistant","content":[{"type":"text","text":"side answer"}]}}`,
		`{"type":"result","subtype":"success"}`,
	}, "\n")

	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a-final" {
		t.Fatalf("CanonicalLeafUUID = %q, want a-final", state.CanonicalLeafUUID)
	}
}

func TestSessionLeafScannerKeepsAssistantUnresolvedUntilAllServerToolsResolve(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"advise"}}`,
		`{"type":"assistant","uuid":"a-advisor","parentUuid":"u1","message":{"id":"msg-advisor","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}},{"type":"server_tool_use","id":"srvtoolu_2","name":"advisor","input":{}}]}}`,
		`{"type":"assistant","uuid":"a-advisor-result","parentUuid":"a-advisor","message":{"id":"msg-advisor-result","role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"advisor_result","text":"ok"}}]}}`,
		`{"type":"assistant","uuid":"a-final","parentUuid":"a-advisor-result","message":{"id":"msg-final","role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success"}`,
	}, "\n")

	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a-final" {
		t.Fatalf("CanonicalLeafUUID = %q, want a-final", state.CanonicalLeafUUID)
	}
	if !state.RequiresResumeAtBeforeUserSend {
		t.Fatal("RequiresResumeAtBeforeUserSend = false, want true")
	}
	if got, want := strings.Join(state.UnresolvedServerToolUUIDs, ","), "a-advisor"; got != want {
		t.Fatalf("UnresolvedServerToolUUIDs = %q, want %q", got, want)
	}
}

func TestSessionLeafScannerResolvesNonAdvisorServerToolResults(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"search"}}`,
		`{"type":"assistant","uuid":"a-search","parentUuid":"u1","message":{"id":"msg-search","role":"assistant","content":[{"type":"web_search_tool_result","tool_use_id":"srvtoolu_search","content":[{"title":"result"}]},{"type":"server_tool_use","id":"srvtoolu_search","name":"web_search","input":{}}]}}`,
		`{"type":"result","subtype":"success"}`,
	}, "\n")

	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a-search" {
		t.Fatalf("CanonicalLeafUUID = %q, want a-search", state.CanonicalLeafUUID)
	}
	if state.RequiresResumeAtBeforeUserSend {
		t.Fatal("RequiresResumeAtBeforeUserSend = true, want false")
	}
	if len(state.UnresolvedServerToolUUIDs) != 0 {
		t.Fatalf("UnresolvedServerToolUUIDs = %v, want empty", state.UnresolvedServerToolUUIDs)
	}
}

func TestLiveLeafTrackerRequiresResumeOnlyAfterTurnComplete(t *testing.T) {
	tracker := newClaudeLeafTracker("")
	tracker.ingestLine([]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"start"}}`))
	tracker.ingestLine([]byte(`{"type":"assistant","uuid":"a-advisor","parentUuid":"u1","message":{"id":"msg-advisor","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}`))
	tracker.ingestLine([]byte(`{"type":"assistant","uuid":"a-final","parentUuid":"u1","message":{"id":"msg-final","role":"assistant","content":[{"type":"text","text":"final"}]}}`))

	if tracker.requiresResumeAtBeforeUserSend() {
		t.Fatal("requiresResumeAtBeforeUserSend before turn complete = true, want false")
	}
	tracker.markTurnComplete()
	if !tracker.requiresResumeAtBeforeUserSend() {
		t.Fatal("requiresResumeAtBeforeUserSend after turn complete = false, want true")
	}
	if got := tracker.canonicalLeaf(); got != "a-final" {
		t.Fatalf("canonicalLeaf = %q, want a-final", got)
	}
}
