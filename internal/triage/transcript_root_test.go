package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// The §E6 transcript root (transcript_root.go).
//
// A resume rebinds the CLI's task lifecycle onto the resuming tool's own
// call — the CARRIER — while every sidechain row the agent produces, in
// round one and in every resumed round, keeps naming the ORIGINAL launch
// as its `parent_tool_use_id`. These tests pin the three mechanisms that
// keep a carrier from being read as a transcript scope: the durable
// stamp, the live-event parent rewrite, and the resume prompt row.

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

// resumeAgent drives the wire sequence a §E6 resume produces: the
// resuming tool's own launch, the meta-only rebind `task_started`
// carrying `extra`, and the ack whose `is_background` is what flips the
// carrier (SendMessage's own ack carries no async marker).
func resumeAgent(t *testing.T, router *Router, threadID, carrierID string, extra map[string]any) {
	t.Helper()
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "SendMessage",
		"input":    map[string]any{"to": "a464e54e96a45cd0c", "summary": "resume"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: carrierID,
		ItemType: "SendMessage", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier launch start: %v", err)
	}
	rebind, _ := json.Marshal(extra)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: carrierID,
		Meta: rebind, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier rebind meta-update: %v", err)
	}
	ack, _ := json.Marshal(map[string]any{"is_background": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: carrierID,
		Content: "resumed from transcript in the background with your message.",
		Meta:    ack, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier ack: %v", err)
	}
}

// deliverResumePrompt fires the EventUserText the parser emits alongside
// the rebind `task_started` (parse_system.go#resumePromptEvent). An
// empty rootID is the reconnect shape, where the parser never saw the
// original binding and triage must resolve the placement itself.
func deliverResumePrompt(t *testing.T, router *Router, threadID, carrierID, rootID, content string) {
	t.Helper()
	fields := map[string]any{
		"wire_only":                               true,
		provider.MetaSubagentResumePromptKey:      true,
		provider.MetaSubagentPromptProvisionalKey: true,
		provider.MetaResumeCarrierIDKey:           carrierID,
	}
	if rootID != "" {
		fields[provider.MetaTranscriptRootIDKey] = rootID
	}
	meta, _ := json.Marshal(fields)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: threadID, Role: "user",
		ItemID:  provider.SubagentOpeningPromptItemID(carrierID),
		Content: content, ContentPresent: true, Meta: meta,
		ParentToolUseID: carrierID, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("resume prompt: %v", err)
	}
}

func itemMetaField(t *testing.T, item store.Item, key string) any {
	t.Helper()
	if strings.TrimSpace(item.Meta) == "" {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &fields); err != nil {
		t.Fatalf("unmarshal meta of %s: %v", item.ID, err)
	}
	return fields[key]
}

func allTurnItems(t *testing.T, st *store.Store, threadID string, turnIndex int) []store.Item {
	t.Helper()
	items, err := st.ListTurnItemsSansPayload(threadID, turnIndex)
	if err != nil {
		t.Fatalf("list turn %d of %s: %v", turnIndex, threadID, err)
	}
	return items
}

// assertNothingIsParentedToACarrier is the tripwire the whole design
// reduces to: a row carrying `transcript_root_id` is a LIFECYCLE row, so
// nothing in the thread may name it as a parent. It reads the carrier
// set off the rows themselves rather than from the test's own ids, so it
// catches a carrier no test knew about.
func assertNothingIsParentedToACarrier(t *testing.T, st *store.Store, threadID string, turnIndex int) {
	t.Helper()
	items := allTurnItems(t, st, threadID, turnIndex)
	carriers := map[string]bool{}
	for _, item := range items {
		if transcriptRootFromItemMeta(item.Meta) != "" {
			carriers[item.ID] = true
		}
	}
	if len(carriers) == 0 {
		t.Fatal("no carrier rows found — the tripwire would pass vacuously")
	}
	for _, item := range items {
		if item.ParentID != "" && carriers[item.ParentID] {
			t.Fatalf("row %s (%s) is parented to carrier %s; a carrier is never a transcript scope",
				item.ID, item.Kind, item.ParentID)
		}
	}
}

func transcriptRootFromItemMeta(meta string) string {
	if !strings.Contains(meta, provider.MetaTranscriptRootIDKey) {
		return ""
	}
	var decoded struct {
		TranscriptRootID string `json:"transcript_root_id"`
	}
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return ""
	}
	return strings.TrimSpace(decoded.TranscriptRootID)
}

// ---------------------------------------------------------------------
// Root resolution: the three evidence sources, strongest first.
// ---------------------------------------------------------------------

// The parser's own stamp is the strongest evidence and survives the flip
// unchanged.
func TestResumeCarrierKeepsTheParserTranscriptRootStamp(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	resumeAgent(t, router, "t1", "carrier-1", map[string]any{
		"task_id":                        "task-1",
		"task_type":                      "local_agent",
		"description":                    "keep going",
		"resumes_tool_use_id":            "agent-1",
		provider.MetaTranscriptRootIDKey: "agent-1",
	})

	carrier := mustGetItem(t, st, "t1", "carrier-1")
	if got := itemMetaField(t, carrier, provider.MetaTranscriptRootIDKey); got != "agent-1" {
		t.Fatalf("carrier meta.transcript_root_id = %v, want agent-1", got)
	}
}

// With no stamp, the `resumes_tool_use_id` chain is walked to its END: a
// round-3 carrier names the round-2 CARRIER, never the launch, so a
// one-hop read would stamp a carrier as another carrier's root.
func TestResumeCarrierWalksTheResumesChainToTheOriginalLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	resumeAgent(t, router, "t1", "carrier-2", map[string]any{
		"task_id": "task-1", "task_type": "local_agent",
		"description": "round 2", "resumes_tool_use_id": "agent-1",
	})
	resumeAgent(t, router, "t1", "carrier-3", map[string]any{
		"task_id": "task-1", "task_type": "local_agent",
		"description": "round 3", "resumes_tool_use_id": "carrier-2",
	})

	for _, carrierID := range []string{"carrier-2", "carrier-3"} {
		carrier := mustGetItem(t, st, "t1", carrierID)
		if got := itemMetaField(t, carrier, provider.MetaTranscriptRootIDKey); got != "agent-1" {
			t.Fatalf("%s meta.transcript_root_id = %v, want agent-1 (the chain's end)", carrierID, got)
		}
	}
}

// With neither stamp nor chain — the reconnect edge, where the parser
// never saw the original binding — the persisted task_id is exact.
func TestResumeCarrierResolvesTheRootByTaskIDWhenNothingNamesIt(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	resumeAgent(t, router, "t1", "carrier-1", map[string]any{
		"task_id": "task-1", "task_type": "local_agent", "description": "keep going",
	})

	carrier := mustGetItem(t, st, "t1", "carrier-1")
	if got := itemMetaField(t, carrier, provider.MetaTranscriptRootIDKey); got != "agent-1" {
		t.Fatalf("carrier meta.transcript_root_id = %v, want agent-1 (task_id fallback)", got)
	}
}

// ---------------------------------------------------------------------
// The Handle chokepoint.
// ---------------------------------------------------------------------

// Every live event naming a carrier as its parent is rewritten onto the
// root before dispatch, so "a row parented to a carrier" is
// unrepresentable regardless of which parser path emitted it.
func TestHandleRewritesACarrierParentOntoTheTranscriptRoot(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	resumeAgent(t, router, "t1", "carrier-1", map[string]any{
		"task_id": "task-1", "task_type": "local_agent",
		"description": "round 2", "resumes_tool_use_id": "agent-1",
	})

	// A round-2 assistant block and a round-2 tool call, both addressed
	// to the carrier (which a naive parser or a future wire change could
	// produce) rather than to the root.
	deliverSubagentBlock(t, router, "t1", "carrier-1", "msg_round2#0", "text", "round two answer")
	toolMeta, _ := json.Marshal(map[string]any{"toolName": "Read"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_round2",
		ItemType: "Read", Meta: toolMeta, ParentToolUseID: "carrier-1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("round-2 tool start: %v", err)
	}

	tool := mustGetItem(t, st, "t1", "toolu_round2")
	if tool.ParentID != "agent-1" {
		t.Fatalf("round-2 tool parent = %q, want agent-1 (FAILS pre-fix: the carrier)", tool.ParentID)
	}
	assertNothingIsParentedToACarrier(t, st, "t1", 0)
}

// The rewrite is per-thread state. A carrier id from one thread must not
// rewrite an identically-named parent on another.
func TestCarrierRootRewriteIsPerThread(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	createTestThread(t, st, "t2")
	seedOpenTurn(t, router, st, "t1", 0)
	seedOpenTurn(t, router, st, "t2", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	resumeAgent(t, router, "t1", "carrier-1", map[string]any{
		"task_id": "task-1", "task_type": "local_agent",
		"description": "round 2", "resumes_tool_use_id": "agent-1",
	})

	// The same id is an ordinary launch on t2.
	startAgentLaunch(t, router, "t2", "carrier-1", "", "task-2")
	toolMeta, _ := json.Marshal(map[string]any{"toolName": "Read"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t2", ItemID: "toolu_other",
		ItemType: "Read", Meta: toolMeta, ParentToolUseID: "carrier-1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("t2 tool start: %v", err)
	}
	if got := mustGetItem(t, st, "t2", "toolu_other").ParentID; got != "carrier-1" {
		t.Fatalf("t2 tool parent = %q, want carrier-1 (t1's rewrite must not cross threads)", got)
	}
}

// A persisted tool_call's parent NEVER moves. A late meta-only
// EventToolStart naming a different scope must not reparent the row —
// the reparenting half of the 2026-09-03 incident.
func TestToolStartDoesNotReparentAPersistedRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	toolMeta, _ := json.Marshal(map[string]any{"toolName": "Read"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_child",
		ItemType: "Read", Meta: toolMeta, ParentToolUseID: "agent-1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("child tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_child",
		Meta: toolMeta, ParentToolUseID: "somewhere-else", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("child tool re-start: %v", err)
	}
	if got := mustGetItem(t, st, "t1", "toolu_child").ParentID; got != "agent-1" {
		t.Fatalf("re-discovered tool parent = %q, want agent-1 (a persisted row's parent never moves)", got)
	}
}

// ---------------------------------------------------------------------
// The resume prompt row.
// ---------------------------------------------------------------------

// The rebind `task_started` is the only envelope carrying the resume
// message. Its row's IDENTITY is the carrier's scope (so it cannot
// collide with round one's opening prompt) while its PLACEMENT is the
// root's — and it is resolved in the live order, where the carrier→root
// map is still empty because the flip has not happened yet.
func TestResumePromptRowLandsUnderTheTranscriptRoot(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	root := mustGetItem(t, st, "t1", "agent-1")

	// Live order: launch, rebind, resume prompt, THEN the ack.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "SendMessage",
		"input":    map[string]any{"to": "a464e54e96a45cd0c"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "carrier-1",
		ItemType: "SendMessage", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier launch start: %v", err)
	}
	rebind, _ := json.Marshal(map[string]any{
		"task_id": "task-1", "task_type": "local_agent",
		"description": "round 2", "resumes_tool_use_id": "agent-1",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "carrier-1",
		Meta: rebind, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier rebind: %v", err)
	}
	deliverResumePrompt(t, router, "t1", "carrier-1", "", "apply the rework and report back")

	prompt := mustGetItem(t, st, "t1", provider.SubagentOpeningPromptItemID("carrier-1"))
	if prompt.ParentID != "agent-1" {
		t.Fatalf("resume prompt parent = %q, want agent-1 (FAILS pre-fix: the carrier)", prompt.ParentID)
	}
	if prompt.TurnIndex != root.TurnIndex {
		t.Fatalf("resume prompt turn = %d, want %d (the root's turn, invariant 10)", prompt.TurnIndex, root.TurnIndex)
	}
	if prompt.Summary != "apply the rework and report back" {
		t.Fatalf("resume prompt summary = %q", prompt.Summary)
	}
	if got := itemMetaField(t, prompt, provider.MetaResumeCarrierIDKey); got != "carrier-1" {
		t.Fatalf("resume prompt meta.resume_carrier_id = %v, want carrier-1", got)
	}
	if got := itemMetaField(t, prompt, provider.MetaSubagentPromptProvisionalKey); got != true {
		t.Fatalf("resume prompt must be provisional until the transcript names it, got %v", got)
	}

	// Idempotent: the parser re-emits nothing, but a reconnect replay
	// must not double the row.
	deliverResumePrompt(t, router, "t1", "carrier-1", "", "apply the rework and report back")
	prompts := 0
	for _, item := range allTurnItems(t, st, "t1", 0) {
		if item.Kind == itemKindUserText && item.ParentID == "agent-1" {
			prompts++
		}
	}
	if prompts != 1 {
		t.Fatalf("expected exactly 1 scoped user row under the root, got %d", prompts)
	}
}

// The parser's stamp places the row without any store read, which is
// what makes the placement independent of whether the carrier's own row
// has been written yet.
func TestResumePromptUsesTheParserRootStampWithoutReadingTheCarrier(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	// No carrier row at all: nothing to resolve through.
	deliverResumePrompt(t, router, "t1", "carrier-1", "agent-1", "now check the tests")

	prompt := mustGetItem(t, st, "t1", provider.SubagentOpeningPromptItemID("carrier-1"))
	if prompt.ParentID != "agent-1" {
		t.Fatalf("resume prompt parent = %q, want agent-1 (the stamped root)", prompt.ParentID)
	}
}

// A blank resume message writes no row rather than an empty bubble.
func TestBlankResumePromptWritesNoRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	deliverResumePrompt(t, router, "t1", "carrier-1", "", "   ")

	if _, found, err := st.GetThreadItem("t1", provider.SubagentOpeningPromptItemID("carrier-1")); err != nil || found {
		t.Fatalf("blank resume prompt wrote a row: found=%v err=%v", found, err)
	}
}

// The terminal transcript later delivers the same text WITH a provider
// uuid. That binds the standing row in place; it must not mint a second
// `user:wire:<uuid>` copy below the answer it asked for.
func TestResumePromptBindsItsProviderUUIDWithoutDuplicating(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	resumeAgent(t, router, "t1", "carrier-1", map[string]any{
		"task_id": "task-1", "task_type": "local_agent",
		"description": "round 2", "resumes_tool_use_id": "agent-1",
	})
	deliverResumePrompt(t, router, "t1", "carrier-1", "", "apply the rework and report back")

	// The transcript's copy: same text, under the ROOT scope, carrying
	// the provider uuid and NO opening-prompt marker (the converter
	// marks only the agent's first scoped user row).
	deliverSubagentPrompt(t, router, "t1", "agent-1", "sidechain-uuid-2", "apply the rework and report back")

	promptID := provider.SubagentOpeningPromptItemID("carrier-1")
	bound := mustGetItem(t, st, "t1", promptID)
	if got := itemMetaField(t, bound, "provider_item_id"); got != "sidechain-uuid-2" {
		t.Fatalf("resume prompt meta.provider_item_id = %v, want sidechain-uuid-2 (bound in place)", got)
	}
	if got := itemMetaField(t, bound, provider.MetaSubagentPromptProvisionalKey); got == true {
		t.Fatal("a bound resume prompt is no longer provisional")
	}
	if got := itemMetaField(t, bound, provider.MetaResumeCarrierIDKey); got != "carrier-1" {
		t.Fatalf("bound row lost its resume_carrier_id: %v", got)
	}
	if got := itemMetaField(t, bound, provider.MetaSubagentOpeningPromptKey); got == true {
		t.Fatal("a resume prompt must never become the agent's OPENING prompt")
	}
	if _, found, err := st.GetThreadItem("t1", "user:wire:sidechain-uuid-2"); err != nil || found {
		t.Fatalf("transcript copy duplicated as user:wire:sidechain-uuid-2: found=%v err=%v", found, err)
	}
	scoped := 0
	for _, item := range allTurnItems(t, st, "t1", 0) {
		if item.Kind == itemKindUserText && item.ParentID == "agent-1" {
			scoped++
		}
	}
	if scoped != 1 {
		t.Fatalf("expected exactly 1 scoped user row under the root, got %d", scoped)
	}
}

// ---------------------------------------------------------------------
// The terminal replay.
// ---------------------------------------------------------------------

// The end-to-end shape of the incident: two rounds, both streamed live
// under the ORIGINAL launch, then a terminal `task_notification` on the
// CARRIER naming the agent's full sidechain. Pre-fix the carrier was the
// replay scope, so the whole transcript read as undelivered — round-1
// tool rows were reparented onto the carrier and its text duplicated.
func TestResumeTerminalReplayReconcilesAgainstTheTranscriptRoot(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// --- Round 1: launched async, streams its whole sidechain live.
	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	deliverSubagentPrompt(t, router, "t1", "agent-1", "s1", "the task prompt")
	deliverSubagentBlock(t, router, "t1", "agent-1", "msg_open#0", "text", "reading the file first")
	readMeta, _ := json.Marshal(map[string]any{"toolName": "Read"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_sub_read",
		ItemType: "Read", Meta: readMeta, ParentToolUseID: "agent-1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("round-1 tool start: %v", err)
	}
	resultMeta, _ := json.Marshal(map[string]any{"toolName": "Read"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "toolu_sub_read",
		Content: "package main", Meta: resultMeta, ParentToolUseID: "agent-1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("round-1 tool complete: %v", err)
	}
	deliverSubagentBlock(t, router, "t1", "agent-1", "msg_close#0", "text", "done: it is a main package")

	round1Tool := mustGetItem(t, st, "t1", "toolu_sub_read")

	// --- Round 2: resumed through SendMessage. Its rows keep naming the
	// ORIGINAL launch on the wire, which is the fact the design rests on.
	deliverResumePrompt(t, router, "t1", "carrier-1", "agent-1", "now check the tests")
	resumeAgent(t, router, "t1", "carrier-1", map[string]any{
		"task_id": "task-1", "task_type": "local_agent",
		"description": "round 2", "resumes_tool_use_id": "agent-1",
	})
	deliverSubagentBlock(t, router, "t1", "agent-1", "msg_round2#0", "text", "tests pass")

	// --- Terminal: the notification lands on the CARRIER and names the
	// agent's whole sidechain, round 1 included.
	transcript := writeSubagentTranscript(t, "agent-1.jsonl",
		sidechainPromptRow("s1", "the task prompt", 1),
		sidechainTextRow("s2", "s1", "msg_open", "reading the file first", 2),
		sidechainToolUseRow("s3", "s2", "msg_tool", "toolu_sub_read", "Read", 3),
		sidechainToolResultRow("s4", "s3", "toolu_sub_read", "package main", 4),
		sidechainTextRow("s5", "s4", "msg_close", "done: it is a main package", 5),
		sidechainPromptRow("s6", "now check the tests", 6),
		sidechainTextRow("s7", "s6", "msg_round2", "tests pass", 7),
	)
	stashAgentTerminal(t, router, "t1", "carrier-1", "task-1")
	notifyAgent(t, router, "t1", "carrier-1", "task-1", transcript, nil)
	router.WaitForPendingSettles()

	assertNothingIsParentedToACarrier(t, st, "t1", 0)

	// Round-1 rows are untouched: same parent, same creation instant.
	afterReplay := mustGetItem(t, st, "t1", "toolu_sub_read")
	if afterReplay.ParentID != "agent-1" {
		t.Fatalf("round-1 tool reparented to %q (FAILS pre-fix: the carrier)", afterReplay.ParentID)
	}
	if afterReplay.CreatedAt != round1Tool.CreatedAt {
		t.Fatalf("round-1 tool re-minted: created_at %d -> %d", round1Tool.CreatedAt, afterReplay.CreatedAt)
	}

	// No `<kind>|provider_item_id` is written twice under the root.
	seen := map[string]string{}
	scopedPrompts := 0
	for _, item := range allTurnItems(t, st, "t1", 0) {
		if item.ParentID != "agent-1" {
			continue
		}
		if item.Kind == itemKindUserText {
			scopedPrompts++
		}
		providerItemID := decodeProviderItemID(item.Meta)
		if providerItemID == "" {
			continue
		}
		key := item.Kind + "|" + providerItemID
		if prior, dup := seen[key]; dup {
			t.Fatalf("duplicate %s under the root: %s and %s", key, prior, item.ID)
		}
		seen[key] = item.ID
	}
	// One per round: the agent's opening prompt and the resume message.
	if scopedPrompts != 2 {
		t.Fatalf("expected 2 scoped user rows under the root (opening + resume), got %d", scopedPrompts)
	}
	resumeRow := mustGetItem(t, st, "t1", provider.SubagentOpeningPromptItemID("carrier-1"))
	if got := itemMetaField(t, resumeRow, "provider_item_id"); got != "s6" {
		t.Fatalf("resume prompt bound to %v, want s6 (the transcript's uuid)", got)
	}

	// The carrier keeps its own lifecycle row and exactly one sibling.
	siblings := 0
	for _, item := range findItemsByKind(t, st, "t1", itemKindBackgroundDone) {
		if item.CompletionOf == "carrier-1" {
			siblings++
		}
	}
	if siblings != 1 {
		t.Fatalf("expected exactly 1 completion sibling under the carrier, got %d", siblings)
	}
}
