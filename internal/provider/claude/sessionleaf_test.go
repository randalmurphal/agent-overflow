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

// The live stdout wire never sets isSidechain — it discriminates a
// subagent's rows with a top-level parent_tool_use_id, on `user`
// envelopes (its tool_results) exactly as on `assistant` ones. A
// sidechain uuid must not become the canonical leaf: every consumer
// resolves that leaf against the FILE, which filters sidechains out
// before searching, so the lookup could only ever miss.
func TestLiveLeafTrackerIgnoresSidechainUserRowsByParentToolUseID(t *testing.T) {
	tracker := newClaudeLeafTracker("")
	ingestTrackerLine(tracker, []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"run the agent"}}`))
	ingestTrackerLine(tracker, []byte(`{"type":"assistant","uuid":"a-launch","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"toolu_task","name":"Agent","input":{}}]}}`))
	if got := tracker.canonicalLeaf(); got != "a-launch" {
		t.Fatalf("canonicalLeaf after launch = %q, want a-launch", got)
	}

	// The subagent's own prompt and tool_result envelopes. Both are
	// `user` rows on the parent's stream, both carry parent_tool_use_id,
	// neither carries isSidechain.
	ingestTrackerLine(tracker, []byte(`{"type":"user","uuid":"side-u","parentUuid":"a-launch","parent_tool_use_id":"toolu_task","message":{"role":"user","content":"subagent prompt"}}`))
	ingestTrackerLine(tracker, []byte(`{"type":"user","uuid":"side-result","parentUuid":"side-u","parent_tool_use_id":"toolu_task","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_inner","content":"done"}]}}`))
	if got := tracker.canonicalLeaf(); got != "a-launch" {
		t.Fatalf("canonicalLeaf after sidechain user rows = %q, want a-launch", got)
	}

	// The main chain's own tool_result for the Agent launch still moves it.
	ingestTrackerLine(tracker, []byte(`{"type":"user","uuid":"u-result","parentUuid":"a-launch","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_task","content":"391"}]}}`))
	if got := tracker.canonicalLeaf(); got != "u-result" {
		t.Fatalf("canonicalLeaf after main-chain tool_result = %q, want u-result", got)
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
	ingestTrackerLine(tracker, []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"start"}}`))
	ingestTrackerLine(tracker, []byte(`{"type":"assistant","uuid":"a-advisor","parentUuid":"u1","message":{"id":"msg-advisor","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}`))
	ingestTrackerLine(tracker, []byte(`{"type":"assistant","uuid":"a-final","parentUuid":"u1","message":{"id":"msg-final","role":"assistant","content":[{"type":"text","text":"final"}]}}`))

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
