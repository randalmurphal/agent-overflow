package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestPersistAndEmitContextWindow_CodexBaselineFormula pins that Codex
// threads use the 12000-token baseline formula from
// codex-rs/protocol/src/protocol.rs:percent_of_context_window_remaining
// (mirrored in provider.ComputeContextPercent). This is what makes our
// meter agree with Codex's TUI for the same wire numbers.
func TestPersistAndEmitContextWindow_CodexBaselineFormula(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createCodexThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTokenUsage,
		ThreadID:  "t1",
		Meta:      mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: 125000, MaxTokens: 200000}),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	usageEmits := filterEmissions(emissions.snapshot(), "provider:usage")
	if len(usageEmits) != 1 {
		t.Fatalf("expected 1 usage emission, got %+v", emissions.snapshot())
	}
	got := usageEmits[0].data.(provider.UsageEvent)
	// (200000-12000) effective window = 188000.
	// (125000-12000) effective used = 113000.
	// percent used = 113000 / 188000 * 100 = 60.106...
	want := provider.ComputeContextPercent(provider.Codex, 125000, 200000)
	if got.ContextPercent != want {
		t.Fatalf("Codex baseline percent: got %v, want %v", got.ContextPercent, want)
	}
	plain := float64(125000) / float64(200000) * 100 // 62.5
	if got.ContextPercent == plain {
		t.Fatalf("Codex meter must NOT use the plain ratio (%v)", plain)
	}
}

// TestPersistAndEmitContextWindow_ClaudePlainRatio pins that Claude
// threads keep using the plain `used / max * 100` formula — the Codex
// 12000 baseline is provider-specific and must not leak.
func TestPersistAndEmitContextWindow_ClaudePlainRatio(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1") // default provider is "claude"

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTokenUsage,
		ThreadID:  "t1",
		Meta:      mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: 100000, MaxTokens: 200000}),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	usageEmits := filterEmissions(emissions.snapshot(), "provider:usage")
	if len(usageEmits) != 1 {
		t.Fatalf("expected 1 usage emission, got %+v", emissions.snapshot())
	}
	got := usageEmits[0].data.(provider.UsageEvent)
	want := float64(100000) / float64(200000) * 100 // 50
	if got.ContextPercent != want {
		t.Fatalf("Claude percent: got %v, want %v", got.ContextPercent, want)
	}
}

// TestPersistAndEmitContextWindow_ExceededPlumbsThrough verifies that the
// Codex `ContextWindowExceeded` sentinel propagates from the parsed
// ContextWindow through the persisted JSON and the emitted UsageEvent.
func TestPersistAndEmitContextWindow_ExceededPlumbsThrough(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createCodexThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTokenUsage,
		ThreadID:  "t1",
		Meta:      mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: 200000, MaxTokens: 200000, Exceeded: true}),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	usageEmits := filterEmissions(emissions.snapshot(), "provider:usage")
	if len(usageEmits) != 1 {
		t.Fatalf("expected 1 usage emission, got %+v", emissions.snapshot())
	}
	got := usageEmits[0].data.(provider.UsageEvent)
	if !got.Exceeded {
		t.Fatalf("UsageEvent.Exceeded must propagate, got %+v", got)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if !strings.Contains(thread.LastTokenUsage, "\"exceeded\":true") {
		t.Fatalf("persisted JSON must include exceeded=true, got %q", thread.LastTokenUsage)
	}
}

// TestHandleTokenUsage_DropsSubagentEvents is the triage-side
// defense-in-depth for Bug C1: even if a future classifier regression
// lets a subagent token-usage event through with ParentToolUseID set,
// the handler must drop it instead of overwriting the parent meter.
// Mirrors internal/provider/claude/parse_assistant.go:appendContextUsageEvent.
func TestHandleTokenUsage_DropsSubagentEvents(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTokenUsage,
		ThreadID:        "t1",
		ParentToolUseID: "spawn-call-1",
		Meta:            mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: 99999, MaxTokens: 200000}),
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if got := len(filterEmissions(emissions.snapshot(), "provider:usage")); got != 0 {
		t.Fatalf("expected 0 usage emissions for subagent token usage, got %d (%+v)", got, emissions.snapshot())
	}
	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.LastTokenUsage != "" {
		t.Fatalf("subagent token usage must NOT touch parent's last_token_usage, got %q", thread.LastTokenUsage)
	}
}

// A compact boundary scoped to a subagent persists under that launch, on
// the launch's turn, and leaves the thread alone: no context-window
// write, no usage frame — even when the meta carries a window. The meter
// is the main agent's; a subagent's compaction is private to it.
func TestHandleCompaction_ScopedBoundaryStaysUnderItsLaunchAndOffTheMeter(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	startAgentLaunch(t, router, "t1", "agent-scoped", "", "task-scoped")
	seedOpenTurn(t, router, st, "t1", 1)
	emissions.reset()

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventCompactBoundary,
		ThreadID:        "t1",
		ItemID:          "boundary-1",
		Content:         "Context compacted",
		ParentToolUseID: "agent-scoped",
		Meta:            mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: 99999, MaxTokens: 200000}),
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	children := childrenOfLaunch(t, st, "t1", "agent-scoped", 0)
	if len(children) != 1 || children[0].Kind != "compaction" {
		t.Fatalf("scoped compaction must land under the launch on its turn, got %+v", children)
	}
	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.LastTokenUsage != "" {
		t.Fatalf("a subagent's compaction must NOT touch the thread's last_token_usage, got %q", thread.LastTokenUsage)
	}
	if got := len(filterEmissions(emissions.snapshot(), "provider:usage")); got != 0 {
		t.Fatalf("expected 0 usage emissions for a scoped compaction, got %d (%+v)", got, emissions.snapshot())
	}
}

// createCodexThread mirrors createTestThread but sets Provider=codex so
// the formula branch under test is exercised.
func createCodexThread(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            id,
		ProjectID:     triageTestProjectID,
		Title:         "Codex Test",
		Provider:      "codex",
		Model:         "gpt-5.4",
		WorkspacePath: "/tmp",
		ContextWindow: 200000,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create codex thread: %v", err)
	}
}

func TestThrottledEmitUsageSuppressesRapidEvents(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	fire := func(used int) {
		t.Helper()
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTokenUsage,
			ThreadID:  "t1",
			Meta:      mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: used, MaxTokens: 200000}),
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	fire(50000)
	fire(51000)
	fire(52000)

	usageEmits := filterEmissions(emissions.snapshot(), "provider:usage")
	if len(usageEmits) != 1 {
		t.Fatalf("expected 1 emission (first passes, rest throttled), got %d", len(usageEmits))
	}
	first := usageEmits[0].data.(provider.UsageEvent)
	if first.UsedTokens != 50000 {
		t.Errorf("first emission tokens: got %d, want 50000", first.UsedTokens)
	}
}

func TestFlushUsageEmitThrottleDrainsPending(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	fire := func(used int) {
		t.Helper()
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTokenUsage,
			ThreadID:  "t1",
			Meta:      mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: used, MaxTokens: 200000}),
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	fire(50000)
	fire(60000)

	before := len(filterEmissions(emissions.snapshot(), "provider:usage"))
	if before != 1 {
		t.Fatalf("pre-flush: expected 1 emission, got %d", before)
	}

	router.FlushUsageEmitThrottle("t1")

	after := len(filterEmissions(emissions.snapshot(), "provider:usage"))
	if after != 2 {
		t.Fatalf("post-flush: expected 2 emissions, got %d", after)
	}
	last := filterEmissions(emissions.snapshot(), "provider:usage")[1].data.(provider.UsageEvent)
	if last.UsedTokens != 60000 {
		t.Errorf("flushed emission tokens: got %d, want 60000", last.UsedTokens)
	}
}

func TestResetUsageEmitThrottleAllowsImmediateNextEmit(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	fire := func(used int) {
		t.Helper()
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTokenUsage,
			ThreadID:  "t1",
			Meta:      mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: used, MaxTokens: 200000}),
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	fire(50000)
	fire(60000)

	usageEmits := filterEmissions(emissions.snapshot(), "provider:usage")
	if len(usageEmits) != 1 {
		t.Fatalf("pre-reset: expected 1 emission, got %d", len(usageEmits))
	}

	router.resetUsageEmitThrottle("t1")
	fire(30000)

	usageEmits = filterEmissions(emissions.snapshot(), "provider:usage")
	if len(usageEmits) != 2 {
		t.Fatalf("post-reset: expected 2 emissions, got %d", len(usageEmits))
	}
	last := usageEmits[1].data.(provider.UsageEvent)
	if last.UsedTokens != 30000 {
		t.Errorf("post-reset emission tokens: got %d, want 30000", last.UsedTokens)
	}
}

// Compile-time guard so the imports stay flagged as used even if a test
// is removed.
var _ = json.RawMessage{}
