package triage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
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

// The parser's stamp places the row without the carrier's own row, which
// is what makes the placement independent of whether the carrier has been
// written yet.
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

// The session mirror later delivers the same text WITH a provider
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
// The terminal.
// ---------------------------------------------------------------------

// Two rounds, both streamed live under the ORIGINAL launch, then a
// terminal `task_notification` on the CARRIER naming the agent's
// sidechain. The terminal settles the carrier's own lifecycle and leaves
// every row under the root exactly as the live stream wrote it.
func TestResumeTerminalOnTheCarrierLeavesTheRootsRowsAlone(t *testing.T) {
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

	underRoot := func() []string {
		var ids []string
		for _, item := range allTurnItems(t, st, "t1", 0) {
			if item.ParentID == "agent-1" {
				ids = append(ids, fmt.Sprintf("%s@%d", item.ID, item.UpdatedAt))
			}
		}
		return ids
	}
	before := underRoot()

	// --- Terminal: the notification lands on the CARRIER and names the
	// agent's sidechain, which completion never reads.
	stashAgentTerminal(t, router, "t1", "carrier-1", "task-1")
	notifyAgent(t, router, "t1", "carrier-1", "task-1", filepath.Join(t.TempDir(), "agent-1.jsonl"), nil)
	router.WaitForPendingSettles()

	assertNothingIsParentedToACarrier(t, st, "t1", 0)
	if after := underRoot(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("the terminal rewrote rows under the root:\nbefore %v\nafter  %v", before, after)
	}
	if after := mustGetItem(t, st, "t1", "toolu_sub_read"); after.ParentID != "agent-1" || after.CreatedAt != round1Tool.CreatedAt {
		t.Fatalf("round-1 tool moved: parent %q created_at %d, want agent-1 at %d", after.ParentID, after.CreatedAt, round1Tool.CreatedAt)
	}
	// The resume message stays under the root, written from the rebind.
	resumeRow := mustGetItem(t, st, "t1", provider.SubagentOpeningPromptItemID("carrier-1"))
	if resumeRow.ParentID != "agent-1" {
		t.Fatalf("resume prompt parented to %q, want the root agent-1", resumeRow.ParentID)
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

// A parser stamp can name a CARRIER: a parser that read a same-binding
// re-announce as a first binding recorded the round-2 carrier as the
// task's root, and stamped it on every later round (thread fc78be87,
// 2026-09-24). The keep-running flip corrects the carrier's stamp to the
// root the stamp resolves to, and the resume and wake prompts resolve the
// stamped row the same way instead of parenting to it.
func TestAStampThatNamesACarrierResolvesToTheRoot(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	resumeAgent(t, router, "t1", "carrier-2", map[string]any{
		"task_id": "task-1", "task_type": "local_agent", "description": "review the file",
		"resumes_tool_use_id": "agent-1", provider.MetaTranscriptRootIDKey: "agent-1",
	})
	// Round 3 as the defective parser stamped it: the root is carrier-2.
	resumeAgent(t, router, "t1", "carrier-3", map[string]any{
		"task_id": "task-1", "task_type": "local_agent", "description": "review the file",
		"resumes_tool_use_id": "carrier-2", provider.MetaTranscriptRootIDKey: "carrier-2",
	})
	deliverResumePrompt(t, router, "t1", "carrier-3", "carrier-2", "round three")
	parkWake(t, router, "t1", "carrier-3", "task-1", "shell", "task-shell", map[string]any{provider.MetaTranscriptRootIDKey: "carrier-2"})

	if got := itemMetaField(t, mustGetItem(t, st, "t1", "carrier-3"), provider.MetaTranscriptRootIDKey); got != "agent-1" {
		t.Errorf("round-3 carrier transcript_root_id = %v, want agent-1 (corrected by the flip)", got)
	}
	if prompt := mustGetItem(t, st, "t1", provider.SubagentOpeningPromptItemID("carrier-3")); prompt.ParentID != "agent-1" {
		t.Errorf("resume prompt parent = %q, want agent-1", prompt.ParentID)
	}
	if wake := mustGetItem(t, st, "t1", provider.SubagentWakePromptItemID("shell")); wake.ParentID != "agent-1" {
		t.Errorf("wake prompt parent = %q, want agent-1", wake.ParentID)
	}
	assertNothingIsParentedToACarrier(t, st, "t1", 0)
}

// The real sequence of thread fc78be87 (task aeb391c5ca059d78f), replayed
// through the parser into the router. The original launch was settled by
// a session death; a fresh process resumed it through a carrier the
// parser could not tie to a launch (HSTnk), resumed it again (011HU),
// re-announced 011HU's binding with a wake prompt, and resumed it a third
// time (01Gv4d9h). Every carrier names the original launch as its root,
// and every resume prompt lands under it.
func TestReplayedReannounceSequenceKeepsEveryRoundUnderTheOriginalLaunch(t *testing.T) {
	const (
		task     = "aeb391c5ca059d78f"
		launch   = "toolu_0189mHNQFmMCHVisk9vZ7rxp"
		recovery = "toolu_01HSTnkfWfX3M54n5e4Bje3K"
		round2   = "toolu_011HUFrpGE5W7yVa9L6XoFzE"
		round3   = "toolu_01Gv4d9hMYaVLopAXfd26Vvm"
	)
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	startAgentLaunch(t, router, "t1", launch, "", task)
	if _, err := router.SettleBackgroundLaunchesForSessionEnd("t1"); err != nil {
		t.Fatalf("session-end settle: %v", err)
	}
	seedOpenTurn(t, router, st, "t1", 1)

	parser := claude.NewParser()
	defer parser.Close()
	feed := func(line string) {
		t.Helper()
		events, err := parser.ParseLine("t1", []byte(line))
		if err != nil {
			t.Fatalf("ParseLine: %v\n%s", err, line)
		}
		for _, evt := range events {
			if evt.Timestamp.IsZero() {
				evt.Timestamp = time.Now()
			}
			if err := router.Handle(evt); err != nil {
				t.Fatalf("handle %s %s: %v", evt.Kind, evt.ItemID, err)
			}
		}
	}
	resume := func(carrier, prompt string) {
		t.Helper()
		feed(`{"type":"assistant","message":{"id":"msg-` + carrier + `","role":"assistant","content":[{"type":"tool_use","id":"` + carrier + `","name":"SendMessage","input":{"to":"` + task + `","message":"` + prompt + `"}}]}}`)
		feed(`{"type":"system","subtype":"task_started","task_id":"` + task + `","tool_use_id":"` + carrier + `","description":"L3 fail-closed tenant session routing","subagent_type":"general-purpose","task_type":"local_agent","prompt":"` + prompt + `"}`)
		feed(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"` + carrier + `","type":"tool_result","content":[{"type":"text","text":"{\"success\":true,\"message\":\"resumed from transcript in the background with your message.\",\"resumedAgentId\":\"` + task + `\"}"}]}]},"parent_tool_use_id":null}`)
	}
	stop := func(bound, summary string) {
		t.Helper()
		feed(`{"type":"system","subtype":"task_updated","task_id":"` + task + `","patch":{"status":"completed","end_time":1790114536078}}`)
		feed(`{"type":"system","subtype":"task_notification","task_id":"` + task + `","tool_use_id":"` + bound + `","status":"completed","summary":"` + summary + `"}`)
	}

	resume(recovery, "Recovery: the app crashed and killed you mid-task.")
	stop(recovery, "Round 1 report")
	resume(round2, "L3 lead review of C5.")
	feed(`{"type":"system","subtype":"task_started","task_id":"` + task + `","tool_use_id":"` + round2 + `","description":"L3 fail-closed tenant session routing","subagent_type":"general-purpose","task_type":"local_agent","is_backgrounded":true,"prompt":"<task-notification>\n<task-id>bflfd5w3i</task-id>\n<tool-use-id>toolu_01PCRUekAgYhLs7cCSLKdHga</tool-use-id>\n<status>completed</status>\n<summary>Background command \"gate\" completed (exit code 0)</summary>\n</task-notification>"}`)
	stop(round2, "Round 2 report")
	resume(round3, "L3 round 3.")

	for _, carrier := range []string{recovery, round2, round3} {
		if got := itemMetaField(t, mustGetItem(t, st, "t1", carrier), provider.MetaTranscriptRootIDKey); got != launch {
			t.Errorf("%s transcript_root_id = %v, want the original launch", carrier, got)
		}
		if prompt := mustGetItem(t, st, "t1", provider.SubagentOpeningPromptItemID(carrier)); prompt.ParentID != launch {
			t.Errorf("%s resume prompt parent = %q, want the original launch", carrier, prompt.ParentID)
		}
	}
	assertNothingIsParentedToACarrier(t, st, "t1", 1)
}
