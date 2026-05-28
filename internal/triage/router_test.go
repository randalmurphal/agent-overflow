package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestHandleEveryEventKindCovered guards against silent drops of newly-added
// EventKinds: it loops every kind listed in provider.AllEventKinds and asserts
// the triage switch does not fall through to its default branch.
//
// Other errors (e.g. handler-specific payload validation) are ignored here —
// this test is a pure coverage check. If it fires with ErrUnhandledEventKind,
// the fix is to add a case in Router.Handle for the named kind.
func TestHandleEveryEventKindCovered(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	for _, kind := range provider.AllEventKinds {
		t.Run(string(kind), func(t *testing.T) {
			evt := provider.ProviderEvent{
				Kind:      kind,
				ThreadID:  "t1",
				TurnID:    "turn-1",
				ItemID:    "item-1",
				Content:   "content",
				Timestamp: time.Now(),
			}
			err := router.Handle(evt)
			if errors.Is(err, ErrUnhandledEventKind) {
				t.Fatalf("Handle fell through to default for kind %q — add a case in router.go", kind)
			}
		})
	}
}

// TestAllEventKindsListIsComplete fails when a new EventKind constant is
// introduced in types.go without a matching entry in AllEventKinds. The check
// is by name, computed by looking at every exported EventKind value we can
// enumerate — since Go lacks runtime reflection over package-level constants,
// the list is maintained manually in types.go; this test only verifies the
// invariant stated in that file's AllEventKinds comment: the slice contains
// every known kind.
//
// Concretely, we keep a reference list here and assert set equality. Any
// future kind therefore requires touching three places (const block, the
// exported slice, and this reference) — which is the friction we want so the
// drift surfaces at CI time.
func TestAllEventKindsListIsComplete(t *testing.T) {
	expected := map[provider.EventKind]bool{
		provider.EventInit:                       true,
		provider.EventTextDelta:                  true,
		provider.EventToolStart:                  true,
		provider.EventToolComplete:               true,
		provider.EventTurnStart:                  true,
		provider.EventTurnComplete:               true,
		provider.EventApprovalRequest:            true,
		provider.EventApprovalResolved:           true,
		provider.EventUserInputRequest:           true,
		provider.EventUserInputResolved:          true,
		provider.EventSessionStatus:              true,
		provider.EventTokenUsage:                 true,
		provider.EventError:                      true,
		provider.EventTodoUpdate:                 true,
		provider.EventTaskCreate:                 true,
		provider.EventTaskUpdate:                 true,
		provider.EventNotification:               true,
		provider.EventAPIRetry:                   true, // transient retry envelopes; rendered as inline timeline rows hiding attempts < 4
		provider.EventCompactBoundary:            true,
		provider.EventRateLimits:                 true,
		provider.EventModelRerouted:              true,
		provider.EventThreadRenamed:              true,
		provider.EventContentBlockStart:          true,
		provider.EventContentBlockStop:           true,
		provider.EventBackgroundTaskTerminal:     true, // Wave 1 — new, Wave 2 wires triage
		provider.EventBackgroundTaskNotification: true, // Claude task_notification is a notification, not a terminal lifecycle signal
		provider.EventSubagentNotification:       true, // reserved for Codex subagent UI
		provider.EventSubagentStatus:             true, // Codex child lifecycle marker; updates live state only
		provider.EventCodexExecResult:            true, // Codex raw exec_command result; live-state enrichment only
		provider.EventTerminalInteraction:        true, // Codex polling marker; triage persists empty-stdin variant
		provider.EventUserText:                   true, // Phase A: dispatch case wired; full handler lands in Phase E
		provider.EventDiff:                       true,
		provider.EventCommandOutput:              true,
		provider.EventThinking:                   true,
		provider.EventProposedPlan:               true,
	}

	got := make(map[provider.EventKind]bool, len(provider.AllEventKinds))
	for _, k := range provider.AllEventKinds {
		got[k] = true
	}

	for k := range expected {
		if !got[k] {
			t.Errorf("provider.AllEventKinds is missing %q", k)
		}
	}
	for k := range got {
		if !expected[k] {
			t.Errorf("provider.AllEventKinds has unknown %q — if this is a new kind, add it to both lists and add a case in router.go", k)
		}
	}
}

// TestHandleReturnsSentinelForUnknownKind is the direct test for the default
// branch: a synthetic kind not in AllEventKinds must return
// ErrUnhandledEventKind. This guards the sentinel contract the coverage test
// above relies on. The router no longer fans the unknown kind out on a
// wire channel — emission is zero so a fresh EventKind surfaces as a
// sentinel error instead of a silent passthrough.
func TestHandleReturnsSentinelForUnknownKind(t *testing.T) {
	router, _, emissions := newTestRouter(t)
	evt := provider.ProviderEvent{
		Kind:      provider.EventKind("synthetic_unknown_kind_for_test"),
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}
	err := router.Handle(evt)
	if !errors.Is(err, ErrUnhandledEventKind) {
		t.Fatalf("expected ErrUnhandledEventKind, got %v", err)
	}
	if len(*emissions) != 0 {
		t.Fatalf("expected 0 emissions for unhandled kind, got %d: %+v", len(*emissions), *emissions)
	}
}

// -- DiffMeta tests --

func TestExtractDiffMetaRealDiff(t *testing.T) {
	patch := `diff --git a/main.go b/main.go
index abc123..def456 100644
--- a/main.go
+++ b/main.go
@@ -10,6 +10,8 @@ func main() {
 	fmt.Println("hello")
+	fmt.Println("new line 1")
+	fmt.Println("new line 2")
 	fmt.Println("world")
-	fmt.Println("old line")
 }
`
	dm := ExtractDiffMeta(patch)

	if dm.FilePath != "main.go" {
		t.Errorf("filePath: got %q, want %q", dm.FilePath, "main.go")
	}
	if dm.Insertions != 2 {
		t.Errorf("insertions: got %d, want 2", dm.Insertions)
	}
	if dm.Deletions != 1 {
		t.Errorf("deletions: got %d, want 1", dm.Deletions)
	}
	if dm.ChangeKind != "modified" {
		t.Errorf("changeKind: got %q, want %q", dm.ChangeKind, "modified")
	}
}

func TestExtractDiffMetaNewFile(t *testing.T) {
	patch := `diff --git a/new.go b/new.go
new file mode 100644
index 0000000..abc1234
--- /dev/null
+++ b/new.go
@@ -0,0 +1,3 @@
+package main
+
+func init() {}
`
	dm := ExtractDiffMeta(patch)

	if dm.FilePath != "new.go" {
		t.Errorf("filePath: got %q, want %q", dm.FilePath, "new.go")
	}
	if dm.ChangeKind != "added" {
		t.Errorf("changeKind: got %q, want %q", dm.ChangeKind, "added")
	}
	if dm.Insertions != 3 {
		t.Errorf("insertions: got %d, want 3", dm.Insertions)
	}
	if dm.Deletions != 0 {
		t.Errorf("deletions: got %d, want 0", dm.Deletions)
	}
}

func TestExtractDiffMetaDeletedFile(t *testing.T) {
	patch := `diff --git a/old.go b/old.go
deleted file mode 100644
index abc1234..0000000
--- a/old.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package main
-
-func old() {}
`
	dm := ExtractDiffMeta(patch)

	if dm.ChangeKind != "deleted" {
		t.Errorf("changeKind: got %q, want %q", dm.ChangeKind, "deleted")
	}
}

func TestExtractDiffMetaRenamed(t *testing.T) {
	patch := `diff --git a/old.go b/new.go
rename from old.go
rename to new.go
similarity index 100%
`
	dm := ExtractDiffMeta(patch)

	if dm.ChangeKind != "renamed" {
		t.Errorf("changeKind: got %q, want %q", dm.ChangeKind, "renamed")
	}
}

func TestExtractDiffMetaPreviewTruncation(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, "+line "+string(rune('A'+i%26)))
	}
	patch := strings.Join(lines, "\n")

	dm := ExtractDiffMeta(patch)

	previewLines := strings.Split(dm.Preview, "\n")
	if len(previewLines) != 20 {
		t.Errorf("preview lines: got %d, want 20", len(previewLines))
	}
}

func TestExtractDiffMetaEmpty(t *testing.T) {
	dm := ExtractDiffMeta("")

	if dm.FilePath != "" {
		t.Errorf("filePath: got %q, want empty", dm.FilePath)
	}
	if dm.Insertions != 0 {
		t.Errorf("insertions: got %d, want 0", dm.Insertions)
	}
	if dm.Deletions != 0 {
		t.Errorf("deletions: got %d, want 0", dm.Deletions)
	}
	if dm.ChangeKind != "modified" {
		t.Errorf("changeKind: got %q, want %q", dm.ChangeKind, "modified")
	}
}

// -- CommandOutputMeta tests --

func TestExtractCommandOutputMeta50Lines(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "output line")
	}
	output := strings.Join(lines, "\n")

	cm := ExtractCommandOutputMeta(output, "go build", 0)

	if cm.LineCount != 50 {
		t.Errorf("lineCount: got %d, want 50", cm.LineCount)
	}

	if cm.Preview != "" {
		t.Errorf("preview: got %q, want empty", cm.Preview)
	}

	if cm.Command != "go build" {
		t.Errorf("command: got %q, want %q", cm.Command, "go build")
	}
	if cm.ExitCode != 0 {
		t.Errorf("exitCode: got %d, want 0", cm.ExitCode)
	}
}

func TestExtractCommandOutputMeta3Lines(t *testing.T) {
	output := "line1\nline2\nline3"
	cm := ExtractCommandOutputMeta(output, "echo hi", 1)

	if cm.LineCount != 3 {
		t.Errorf("lineCount: got %d, want 3", cm.LineCount)
	}

	if cm.Preview != "" {
		t.Errorf("preview: got %q, want empty", cm.Preview)
	}

	if cm.ExitCode != 1 {
		t.Errorf("exitCode: got %d, want 1", cm.ExitCode)
	}
}

func TestExtractCommandOutputMetaEmpty(t *testing.T) {
	cm := ExtractCommandOutputMeta("", "", 0)

	// Empty string splits to [""] which is 1 element.
	if cm.LineCount != 1 {
		t.Errorf("lineCount: got %d, want 1", cm.LineCount)
	}
}

// -- ThinkingMeta tests --

func TestExtractThinkingMeta500Chars(t *testing.T) {
	content := strings.Repeat("a", 500)
	tm := ExtractThinkingMeta(content)

	if len(tm.Preview) != 203 { // 200 + "..."
		t.Errorf("preview length: got %d, want 203", len(tm.Preview))
	}
	if !strings.HasSuffix(tm.Preview, "...") {
		t.Error("preview should end with ...")
	}
}

func TestExtractThinkingMeta100Chars(t *testing.T) {
	content := strings.Repeat("b", 100)
	tm := ExtractThinkingMeta(content)

	if tm.Preview != content {
		t.Errorf("preview: got %q, want full content", tm.Preview)
	}
}

func TestExtractThinkingMetaTokenEstimate(t *testing.T) {
	content := strings.Repeat("c", 400)
	tm := ExtractThinkingMeta(content)

	if tm.TokenCount != 100 {
		t.Errorf("tokenCount: got %d, want 100 (400/4)", tm.TokenCount)
	}
}

// -- Router tests --

// TestRouterWaitReturnsImmediatelyWhenIdle is the happy-path contract
// for Router.Wait: if no Handle is running, Wait returns instantly.
// App shutdown calls this first, so a regression that introduced a
// blocking wait would stall every Quit.
func TestRouterWaitReturnsImmediatelyWhenIdle(t *testing.T) {
	router, _, _ := newTestRouter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := router.Wait(ctx); err != nil {
		t.Fatalf("Wait() on idle router error = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("idle Wait took %v; expected instant return", elapsed)
	}
}

// TestRouterWaitBlocksUntilInflightHandleReturns tests the drain
// contract under load: a Handle() in progress holds Wait open until
// the Handle returns. We drive this with a stub store-free event
// (ErrUnhandledEventKind returns fast) and then a blocking handler
// emulation via a goroutine that paces the Done.
//
// Rather than monkey-patching the router, we inject a handler that
// blocks by feeding an event kind whose handler does real work
// (payload persistence) but the store write is just a memory insert
// on the test store. That is NOT blocking on its own, so we instead
// call Wait while a slow synthetic "Handle" is active via a helper
// that mirrors the inflight add/done.
func TestRouterWaitBlocksUntilInflightHandleReturns(t *testing.T) {
	router, _, _ := newTestRouter(t)

	// Simulate an in-flight Handle call by bumping the inflight counter
	// directly. This mirrors exactly what Handle does at its entrypoint
	// — test stays honest because Wait reads the same waitgroup.
	router.inflight.Add(1)

	waitDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		waitDone <- router.Wait(ctx)
	}()

	// Wait must not resolve while the counter is held.
	select {
	case err := <-waitDone:
		t.Fatalf("Wait returned %v while inflight > 0; expected to block", err)
	case <-time.After(50 * time.Millisecond):
	}

	router.inflight.Done()

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait() after drain error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after inflight dropped to zero")
	}
}

// TestRouterWaitHonoursContextDeadline guards the timeout branch: if
// Handle is stuck longer than the caller's deadline, Wait returns
// context.DeadlineExceeded rather than blocking forever. App Shutdown
// relies on this to keep Quit latency bounded.
func TestRouterWaitHonoursContextDeadline(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.inflight.Add(1)
	defer router.inflight.Done() // release after test so the test's own cleanup doesn't hang

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := router.Wait(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Wait took %v; expected near the 100ms timeout", elapsed)
	}
}

type emitted struct {
	eventName string
	data      any
}

func newTestRouter(t *testing.T) (*Router, *store.Store, *[]emitted) {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var emissions []emitted
	emit := func(eventName string, data any) {
		emissions = append(emissions, emitted{eventName, data})
	}

	router := NewRouter(st, emit)
	return router, st, &emissions
}

func createTestThread(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	err := st.CreateThread(store.Thread{
		ID:            id,
		ProjectID:     triageTestProjectID,
		Title:         "Test",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
}

func insertToolCallItem(t *testing.T, st *store.Store, threadID, itemID, summary, toolName string, status string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:        strings.TrimSpace(itemID),
		ThreadID:  threadID,
		TurnIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    status,
		Summary:   summary,
		ToolName:  toolName,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append tool_call %s: %v", itemID, err)
	}
}

func TestInlineEventEmit(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "hello ",
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) != 1 {
		t.Fatalf("expected 1 provider:item_event upsert, got %d", len(upserts))
	}
	item := upserts[0]
	if item.Kind != "assistant_text" {
		t.Fatalf("item kind: got %q, want assistant_text", item.Kind)
	}
	if item.Status != "streaming" {
		t.Fatalf("status: got %q, want streaming", item.Status)
	}
}

func TestCompactBoundaryAndRateLimitsEmit(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	events := []provider.ProviderEvent{
		{Kind: provider.EventCompactBoundary, ThreadID: "t1", Timestamp: time.Now()},
		{Kind: provider.EventRateLimits, ThreadID: "t1", Timestamp: time.Now()},
	}

	for _, evt := range events {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle %s: %v", evt.Kind, err)
		}
	}

	// Neither event should land on the legacy passthrough channel —
	// compact-boundary produces an item_event upsert (and rate-limits now
	// folds onto provider:usage per the chat-rewrite spec). Compaction
	// without a window in meta does NOT emit a reset — that flash of 0%
	// was the bug; the next thread/tokenUsage/updated overwrites the
	// meter naturally.
	if got := len(filterEmissions(*emissions, "provider:event")); got != 0 {
		t.Fatalf("expected zero provider:event emissions, got %d (%+v)", got, *emissions)
	}
	usageEmits := filterEmissions(*emissions, "provider:usage")
	if len(usageEmits) != 1 {
		t.Fatalf("expected 1 provider:usage emission (rate_limits only — no compact reset), got %+v", *emissions)
	}
	if got := usageEmits[0].data.(provider.UsageEvent).Action; got != "rate_limits" {
		t.Fatalf("rate-limits usage action = %q, want %q", got, "rate_limits")
	}
	if len(filterItemEventUpserts(*emissions)) != 1 {
		t.Fatalf("expected compact boundary item upsert, got %+v", *emissions)
	}
}

func TestCompactBoundariesInSameTurnPersistDistinctRows(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	for _, evt := range []provider.ProviderEvent{
		{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  "t1",
			ItemID:    "compact-a",
			Content:   "Conversation compacted",
			Meta:      json.RawMessage(`{"trigger":"auto"}`),
			Timestamp: time.Now(),
		},
		{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  "t1",
			ItemID:    "compact-b",
			Content:   "Conversation compacted",
			Meta:      json.RawMessage(`{"trigger":"manual"}`),
			Timestamp: time.Now(),
		},
	} {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle compact %s: %v", evt.ItemID, err)
		}
	}

	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) != 2 {
		t.Fatalf("expected 2 compaction upserts, got %+v", upserts)
	}
	if upserts[0].ID == upserts[1].ID {
		t.Fatalf("compaction IDs collided: %q", upserts[0].ID)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	compactions := make([]store.Item, 0, 2)
	for _, item := range items {
		if item.Kind == "compaction" {
			compactions = append(compactions, item)
		}
	}
	if len(compactions) != 2 {
		t.Fatalf("stored compactions = %d, want 2 (%+v)", len(compactions), items)
	}
	if compactions[0].ID != "compact:0:provider:compact-a" || compactions[1].ID != "compact:0:provider:compact-b" {
		t.Fatalf("stored compaction IDs = %q, %q", compactions[0].ID, compactions[1].ID)
	}
	if !strings.Contains(compactions[0].Meta, `"trigger":"auto"`) {
		t.Fatalf("compaction meta not persisted: %q", compactions[0].Meta)
	}
}

func TestCompactBoundaryWithSameProviderIDUpdatesExistingRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	for _, content := range []string{"First compact", "Second compact"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  "t1",
			ItemID:    "compact-a",
			Content:   content,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle compact %q: %v", content, err)
		}
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var compactions []store.Item
	for _, item := range items {
		if item.Kind == "compaction" {
			compactions = append(compactions, item)
		}
	}
	if len(compactions) != 1 {
		t.Fatalf("stored compactions = %d, want 1 (%+v)", len(compactions), items)
	}
	if compactions[0].Summary != "Second compact" {
		t.Fatalf("summary = %q, want updated content", compactions[0].Summary)
	}
}

func TestCompactBoundaryProviderIDNormalization(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	longID := strings.Repeat("a", maxProviderCompactionIDLength+1)
	for _, evt := range []provider.ProviderEvent{
		{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  "t1",
			ItemID:    " compact-a ",
			Timestamp: time.Now(),
		},
		{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  "t1",
			ItemID:    "bad\nid",
			Timestamp: time.Now(),
		},
		{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  "t1",
			ItemID:    longID,
			Timestamp: time.Now(),
		},
	} {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle compact %q: %v", evt.ItemID, err)
		}
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	compactions := compactionItems(items)
	if len(compactions) != 3 {
		t.Fatalf("stored compactions = %d, want 3 (%+v)", len(compactions), items)
	}
	if compactions[0].ID != "compact:0:provider:compact-a" {
		t.Fatalf("trimmed provider ID = %q", compactions[0].ID)
	}
	if compactions[1].ID != "compact:0:seq:0" {
		t.Fatalf("control-character provider ID should fall back to seq, got %q", compactions[1].ID)
	}
	if !strings.HasPrefix(compactions[2].ID, "compact:0:provider:sha256:") {
		t.Fatalf("long provider ID should be hashed, got %q", compactions[2].ID)
	}
	if len(compactions[2].ID) > 512 {
		t.Fatalf("hashed provider ID too long: %d", len(compactions[2].ID))
	}
}

func TestCompactBoundariesWithoutProviderIDUseNextAvailableSequence(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	for i := 0; i < 2; i++ {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  "t1",
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle compact %d: %v", i, err)
		}
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	compactions := compactionItems(items)
	if len(compactions) != 2 {
		t.Fatalf("stored compactions = %d, want 2 (%+v)", len(compactions), items)
	}
	if compactions[0].ID != "compact:0:seq:0" || compactions[1].ID != "compact:0:seq:1" {
		t.Fatalf("stored compaction IDs = %q, %q", compactions[0].ID, compactions[1].ID)
	}
}

func TestCompactBoundarySequenceSkipsPersistedRowsAfterRouterRestart(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCompactBoundary,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle initial compact: %v", err)
	}

	router = NewRouter(st, func(string, any) {})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCompactBoundary,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle compact after restart: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	compactions := compactionItems(items)
	if len(compactions) != 2 {
		t.Fatalf("stored compactions = %d, want 2 (%+v)", len(compactions), items)
	}
	if compactions[0].ID != "compact:0:seq:0" || compactions[1].ID != "compact:0:seq:1" {
		t.Fatalf("stored compaction IDs = %q, %q", compactions[0].ID, compactions[1].ID)
	}
}

func TestCompactBoundaryWithContextWindowEmitsUsageSnapshot(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	meta, err := json.Marshal(provider.ContextWindow{
		UsedTokens:     50000,
		MaxTokens:      200000,
		UsedPercentage: 25,
	})
	if err != nil {
		t.Fatalf("marshal context window: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCompactBoundary,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	usageEmits := filterEmissions(*emissions, "provider:usage")
	if len(usageEmits) != 1 {
		t.Fatalf("expected 1 provider:usage emission, got %+v", *emissions)
	}
	usage := usageEmits[0].data.(provider.UsageEvent)
	if usage.Action != "usage" {
		t.Fatalf("usage action: got %q, want usage", usage.Action)
	}
	if usage.UsedTokens != 50000 {
		t.Fatalf("usedTokens: got %d, want 50000", usage.UsedTokens)
	}
	if usage.MaxTokens != 200000 {
		t.Fatalf("maxTokens: got %d, want 200000", usage.MaxTokens)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if !strings.Contains(thread.LastTokenUsage, "\"usedTokens\":50000") {
		t.Fatalf("last token usage not persisted: %q", thread.LastTokenUsage)
	}
}

// TestCompactBoundaryWithAggregateOnlyUsageMetaDoesNotReset locks in the
// behavior that compact events whose meta does NOT decode as a
// ContextWindow do NOT clear `last_token_usage` or emit a reset. The
// previous "reset" branch caused a brief 0%-meter flash between the
// compact event and the post-compact `thread/tokenUsage/updated` —
// Codex's `recompute_token_usage` always emits a fresh reading, so
// trusting that next signal keeps the meter consistent without the flash.
func TestCompactBoundaryWithAggregateOnlyUsageMetaDoesNotReset(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Seed a prior reading so we can prove the compact event does not
	// clear it.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTokenUsage,
		ThreadID:  "t1",
		Meta:      mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: 100000, MaxTokens: 200000}),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle prior token usage: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCompactBoundary,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"totalProcessed":120000}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Only the original token-usage emission. No compact-driven reset.
	usageEmits := filterEmissions(*emissions, "provider:usage")
	if len(usageEmits) != 1 {
		t.Fatalf("expected exactly the prior usage emission, got %+v", *emissions)
	}
	if got := usageEmits[0].data.(provider.UsageEvent).Action; got != "usage" {
		t.Fatalf("usage action: got %q, want usage", got)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if !strings.Contains(thread.LastTokenUsage, "\"usedTokens\":100000") {
		t.Fatalf("compact must NOT clear last_token_usage; got %q", thread.LastTokenUsage)
	}
}

// TestCompactBoundaryFollowedByTokenUsageOverwrites pins the wire-order
// path Codex takes after `recompute_token_usage`: the post-compact
// `thread/tokenUsage/updated` overwrites the meter with the fresh
// reading. Without Fix 3 in place, the intermediate compact-clear
// would have flashed 0% in between.
func TestCompactBoundaryFollowedByTokenUsageOverwrites(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTokenUsage,
		ThreadID:  "t1",
		Meta:      mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: 180000, MaxTokens: 200000}),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle pre-compact: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCompactBoundary,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"totalProcessed":120000}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle compact: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTokenUsage,
		ThreadID:  "t1",
		Meta:      mustMarshalContextWindow(t, provider.ContextWindow{UsedTokens: 30000, MaxTokens: 200000}),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle post-compact: %v", err)
	}

	usageEmits := filterEmissions(*emissions, "provider:usage")
	if len(usageEmits) != 2 {
		t.Fatalf("expected pre-compact + post-compact usage emissions only, got %+v", *emissions)
	}
	final := usageEmits[len(usageEmits)-1].data.(provider.UsageEvent)
	if final.UsedTokens != 30000 {
		t.Fatalf("final usage tokens: got %d, want 30000", final.UsedTokens)
	}
	if final.Action != "usage" {
		t.Fatalf("final action: got %q, want usage", final.Action)
	}
}

func mustMarshalContextWindow(t *testing.T, w provider.ContextWindow) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal context window: %v", err)
	}
	return raw
}

// TestRateLimitsRoutedToUsageChannel verifies EventRateLimits emits on the
// provider:usage channel with action=rate_limits and the snapshot carried
// through in the RateLimits field (chat-rewrite spec, Channels section).
// This replaces the legacy provider:event fanout.
func TestRateLimitsRoutedToUsageChannel(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	snapshot := provider.RateLimitsSnapshot{
		Provider: "claude",
		Limits: []provider.RateLimitEntry{
			{
				LimitID:     "five_hour",
				LimitName:   "5h",
				UsedPercent: 62.5,
				WindowMins:  300,
				ResetsAt:    1776283200,
			},
		},
		UpdatedAt: 1776283000,
	}
	meta, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	evt := provider.ProviderEvent{
		Kind:      provider.EventRateLimits,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle rate-limits: %v", err)
	}

	if got := len(filterEmissions(*emissions, "provider:event")); got != 0 {
		t.Fatalf("expected 0 provider:event emissions, got %d (%+v)", got, *emissions)
	}
	usage := filterEmissions(*emissions, "provider:usage")
	if len(usage) != 1 {
		t.Fatalf("expected 1 provider:usage emission, got %+v", *emissions)
	}
	payload, ok := usage[0].data.(provider.UsageEvent)
	if !ok {
		t.Fatalf("usage payload type = %T, want provider.UsageEvent", usage[0].data)
	}
	if payload.Action != "rate_limits" {
		t.Fatalf("usage action = %q, want rate_limits", payload.Action)
	}
	if payload.ThreadID != "t1" {
		t.Fatalf("usage threadID = %q, want t1", payload.ThreadID)
	}
	if payload.RateLimits == nil {
		t.Fatalf("rate-limits payload is nil; expected snapshot")
	}
	if payload.RateLimits.Provider != "claude" {
		t.Fatalf("rate-limits provider = %q, want claude", payload.RateLimits.Provider)
	}
	if got := len(payload.RateLimits.Limits); got != 1 {
		t.Fatalf("rate-limits entries = %d, want 1", got)
	}
}

// TestRateLimitsTolereatesMissingMeta covers the degenerate case where the
// rate-limits snapshot is missing or malformed — the router still emits the
// discriminator so the frontend listener fires, just without the payload
// body. This mirrors the "drop meta, keep the signal" pattern used for
// token usage.
func TestRateLimitsTolereatesMissingMeta(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:      provider.EventRateLimits,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle empty rate-limits: %v", err)
	}

	usage := filterEmissions(*emissions, "provider:usage")
	if len(usage) != 1 {
		t.Fatalf("expected 1 provider:usage emission, got %+v", *emissions)
	}
	payload := usage[0].data.(provider.UsageEvent)
	if payload.Action != "rate_limits" {
		t.Fatalf("usage action = %q, want rate_limits", payload.Action)
	}
	if payload.RateLimits != nil {
		t.Fatalf("rate-limits should be nil when meta is empty, got %+v", payload.RateLimits)
	}
}

// TestSessionStatusDropsTransientKinds verifies that transient lifecycle
// signals ("disconnected", "session_state_changed") don't reach any
// frontend channel. The "error" content is no longer transient — it
// promotes to a session_died notification + provider:session_died
// emission and a synthesized truncated turn-complete; that path is
// covered separately by the session_died tests.
func TestSessionStatusDropsTransientKinds(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	transient := []string{"disconnected", "session_state_changed"}
	for _, content := range transient {
		evt := provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  "t1",
			Content:   content,
			Timestamp: time.Now(),
		}
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle session-status %q: %v", content, err)
		}
	}

	if got := len(filterEmissions(*emissions, "provider:status")); got != 0 {
		t.Fatalf("expected 0 provider:status emissions for transients, got %d (%+v)", got, *emissions)
	}
	if got := len(filterEmissions(*emissions, "provider:session_died")); got != 0 {
		t.Fatalf("expected 0 provider:session_died emissions for transients, got %d (%+v)", got, *emissions)
	}
	if got := len(filterEmissions(*emissions, "provider:event")); got != 0 {
		t.Fatalf("expected 0 provider:event emissions for transients, got %d (%+v)", got, *emissions)
	}
}

// TestSessionStatusUnknownContentIsSilentDrop guards the "log once, drop
// silently" contract for unrecognized Content strings. The event shouldn't
// surface on any channel — the log is an observability breadcrumb, not a
// routing decision.
func TestSessionStatusUnknownContentIsSilentDrop(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  "t1",
		Content:   "wholly-new-subtype-from-sdk",
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle unknown session-status: %v", err)
	}

	if len(*emissions) != 0 {
		t.Fatalf("expected zero emissions for unknown session-status, got %+v", *emissions)
	}

	// Second emission of the same unknown content should also not emit
	// (the "log once" part is implicit — we just assert that no channel
	// fires so the silent-drop contract holds regardless of dedup state).
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle repeat unknown session-status: %v", err)
	}
	if len(*emissions) != 0 {
		t.Fatalf("repeat unknown session-status emitted %+v", *emissions)
	}
}

func TestModelReroutedUpdatesThread(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:      provider.EventModelRerouted,
		ThreadID:  "t1",
		Content:   "gpt-5.4",
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.Model != "gpt-5.4" {
		t.Fatalf("expected updated model, got %q", thread.Model)
	}
	if len(*emissions) != 1 || (*emissions)[0].eventName != "thread:updated" {
		t.Fatalf("expected thread:updated emission for model reroute, got %+v", *emissions)
	}
	tue := (*emissions)[0].data.(ThreadUpdateEvent)
	if tue.Action != "patch" {
		t.Fatalf("expected action=patch, got %q", tue.Action)
	}
	if tue.ID != "t1" {
		t.Fatalf("expected id=t1, got %q", tue.ID)
	}
	if tue.Model == nil || *tue.Model != "gpt-5.4" {
		t.Fatalf("expected model patch to be gpt-5.4, got %v", tue.Model)
	}
	if tue.Title != nil {
		t.Fatalf("title must be nil in model-only patch, got %v", tue.Title)
	}
	if tue.Thread != nil {
		t.Fatalf("thread must be nil in patch event, got non-nil")
	}
}

func TestThreadRenamedUpdatesThread(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:      provider.EventThreadRenamed,
		ThreadID:  "t1",
		Content:   "New Title",
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.Title != "New Title" {
		t.Fatalf("expected updated title, got %q", thread.Title)
	}
	if len(*emissions) != 1 || (*emissions)[0].eventName != "thread:updated" {
		t.Fatalf("expected thread:updated emission for thread rename, got %+v", *emissions)
	}
	tue := (*emissions)[0].data.(ThreadUpdateEvent)
	if tue.Action != "patch" {
		t.Fatalf("expected action=patch, got %q", tue.Action)
	}
	if tue.ID != "t1" {
		t.Fatalf("expected id=t1, got %q", tue.ID)
	}
	if tue.Title == nil || *tue.Title != "New Title" {
		t.Fatalf("expected title patch to be 'New Title', got %v", tue.Title)
	}
	if tue.Model != nil {
		t.Fatalf("model must be nil in title-only patch, got %v", tue.Model)
	}
	if tue.Thread != nil {
		t.Fatalf("thread must be nil in patch event, got non-nil")
	}
}

func TestContextWindowUsageEmitsContextMeterUpdate(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		ProjectID:     triageTestProjectID,
		Title:         "Cost Test",
		Provider:      "codex",
		WorkspacePath: "/tmp",
		Model:         "gpt-5.4",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	meta, err := json.Marshal(provider.ContextWindow{
		UsedTokens: 126,
		MaxTokens:  258_400,
	})
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTokenUsage,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	usageEvents := filterEmissions(*emissions, "provider:usage")
	if len(usageEvents) != 1 {
		t.Fatalf("expected 1 provider:usage emission, got %+v", *emissions)
	}
	usage, ok := usageEvents[0].data.(provider.UsageEvent)
	if !ok {
		t.Fatalf("usage emission type = %T, want provider.UsageEvent", usageEvents[0].data)
	}
	if usage.UsedTokens != 126 {
		t.Fatalf("usedTokens: got %d, want 126", usage.UsedTokens)
	}
	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if !strings.Contains(thread.LastTokenUsage, "\"usedTokens\":126") {
		t.Fatalf("last token usage not persisted: %q", thread.LastTokenUsage)
	}
}

func TestGenericTokenUsageDoesNotEmitContextMeterUpdate(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		ProjectID:     triageTestProjectID,
		Title:         "Unknown Model",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		Model:         "unknown-model",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	meta, err := json.Marshal(provider.TokenUsage{InputTokens: 123, OutputTokens: 456})
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTokenUsage,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	usageEvents := filterEmissions(*emissions, "provider:usage")
	if len(usageEvents) != 0 {
		t.Fatalf("expected no provider:usage emission, got %+v", usageEvents)
	}
}

func TestApprovalRequestEmitsProviderApproval(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	meta, err := json.Marshal(provider.ApprovalRequest{
		RequestID:   "req-1",
		ThreadID:    "t1",
		ToolUseID:   "tool-1",
		ToolName:    "Bash",
		Description: "Allow Bash?",
		Input:       json.RawMessage(`{"command":"rm -rf tmp"}`),
		Title:       "Bash",
	})
	if err != nil {
		t.Fatalf("marshal approval: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  "t1",
		ItemID:    "req-1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	approvalEvents := filterEmissions(*emissions, "provider:approval")
	if len(approvalEvents) != 1 {
		t.Fatalf("expected 1 provider:approval emission, got %+v", *emissions)
	}
	approval, ok := approvalEvents[0].data.(provider.ApprovalEvent)
	if !ok {
		t.Fatalf("approval emission type = %T, want provider.ApprovalEvent", approvalEvents[0].data)
	}
	if approval.Action != "request" {
		t.Fatalf("action = %q, want request", approval.Action)
	}
	if approval.Request == nil || approval.Request.ToolUseID != "tool-1" {
		t.Fatalf("request = %+v, want toolUseId=tool-1", approval.Request)
	}
}

func TestApprovalDeclineBeforeToolStartCreatesSyntheticToolCall(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	requestMeta, err := json.Marshal(provider.ApprovalRequest{
		RequestID:   "req-1",
		ThreadID:    "t1",
		ToolUseID:   "tool-1",
		ToolName:    "Bash",
		Description: "Allow Bash?",
		Input:       json.RawMessage(`{"command":"rm -rf tmp"}`),
		Title:       "Bash",
	})
	if err != nil {
		t.Fatalf("marshal approval: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  "t1",
		ItemID:    "req-1",
		Meta:      requestMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle request: %v", err)
	}

	resolveMeta, _ := json.Marshal(map[string]any{
		"requestId": "req-1",
		"decision":  "declined",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  "t1",
		ItemID:    "req-1",
		Meta:      resolveMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle resolve: %v", err)
	}

	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 {
		t.Fatalf("expected 1 synthetic tool_call, got %+v", items)
	}
	if items[0].ID != "tool-1" {
		t.Fatalf("item id = %q, want %q", items[0].ID, "tool-1")
	}
	if items[0].Status != "declined" || items[0].Decision != "declined" {
		t.Fatalf("unexpected declined item state: %+v", items[0])
	}
	if !strings.Contains(items[0].Summary, "rm -rf tmp") {
		t.Fatalf("summary = %q, want command preview", items[0].Summary)
	}

	approvalEvents := filterEmissions(*emissions, "provider:approval")
	if len(approvalEvents) != 2 {
		t.Fatalf("expected request + resolve approval events, got %+v", approvalEvents)
	}
}

// TestAmendedDecisionUpdatesToolCallSummary verifies that when the user
// amends an approval's input (the Claude SDK UpdatedInput path), the
// persisted tool_call summary reflects the MODIFIED input, not the
// original command the provider proposed. Regression would leave the
// row showing the pre-amendment command, which the spec explicitly
// rules out for the amended decision.
func TestAmendedDecisionUpdatesToolCallSummary(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Seed the launch row with the ORIGINAL command.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "rm -rf /tmp/old"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "tool-amended",
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start: %v", err)
	}

	// Register the pending approval with the ORIGINAL input so we
	// exercise the overlay path (not a fresh request build).
	requestMeta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID: "req-a",
		ThreadID:  "t1",
		ToolUseID: "tool-amended",
		ToolName:  "Bash",
		Input:     json.RawMessage(`{"command":"rm -rf /tmp/old"}`),
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  "t1",
		ItemID:    "req-a",
		Meta:      requestMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle request: %v", err)
	}

	// Resolve with decision=amended and a different updatedInput.
	resolveMeta, _ := json.Marshal(map[string]any{
		"requestId":    "req-a",
		"decision":     "amended",
		"updatedInput": map[string]any{"command": "rm -rf /tmp/new"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  "t1",
		ItemID:    "req-a",
		Meta:      resolveMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle resolve: %v", err)
	}

	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 {
		t.Fatalf("expected 1 tool_call, got %+v", items)
	}
	got := items[0]
	if got.Decision != "amended" {
		t.Errorf("decision = %q, want amended", got.Decision)
	}
	if !strings.Contains(got.Summary, "rm -rf /tmp/new") {
		t.Errorf("summary %q must contain amended command /tmp/new", got.Summary)
	}
	if strings.Contains(got.Summary, "/tmp/old") {
		t.Errorf("summary %q still contains original command; amendment lost", got.Summary)
	}
}

func TestApprovalLostMarksRunningToolCallErrored(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "tool-1",
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start: %v", err)
	}

	requestMeta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID:   "req-1",
		ThreadID:    "t1",
		ToolName:    "Bash",
		Description: "Allow Bash?",
		Input:       json.RawMessage(`{"command":"rm -rf tmp"}`),
		Title:       "Bash",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  "t1",
		ItemID:    "tool-1",
		Meta:      requestMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle request: %v", err)
	}

	resolveMeta, _ := json.Marshal(map[string]any{
		"requestId": "req-1",
		"decision":  "lost",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  "t1",
		ItemID:    "req-1",
		Meta:      resolveMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle resolve: %v", err)
	}

	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 {
		t.Fatalf("expected 1 tool_call, got %+v", items)
	}
	if items[0].Status != statusErrored || items[0].Decision != "lost" {
		t.Fatalf("unexpected lost item state: %+v", items[0])
	}
}

func TestProviderToolItemIDsAreThreadScoped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	createTestThread(t, st, "t2")

	meta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	for _, threadID := range []string{"t1", "t2"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			ItemID:    "tool-1",
			ItemType:  "Bash",
			Meta:      meta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle %s: %v", threadID, err)
		}
	}

	t1Items := findItemsByKind(t, st, "t1", itemKindToolCall)
	t2Items := findItemsByKind(t, st, "t2", itemKindToolCall)
	if len(t1Items) != 1 || len(t2Items) != 1 {
		t.Fatalf("expected one tool_call per thread, got t1=%+v t2=%+v", t1Items, t2Items)
	}
	if t1Items[0].ID != "tool-1" {
		t.Fatalf("t1 id = %q, want %q", t1Items[0].ID, "tool-1")
	}
	if t2Items[0].ID != "tool-1" {
		t.Fatalf("t2 id = %q, want %q", t2Items[0].ID, "tool-1")
	}
	if t1Items[0].ID != t2Items[0].ID {
		t.Fatalf("thread-local ids should be reusable across threads, got t1=%q t2=%q", t1Items[0].ID, t2Items[0].ID)
	}
}

func TestInlineEventDoesNotCallStore(t *testing.T) {
	router, st, _ := newTestRouter(t)

	evt := provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// No thread was created, so ListItems should return nothing (and not error).
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected no items persisted for inline event, got %d", len(items))
	}
}

func TestEventInitUpdatesSessionRef(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread fixture: %v", err)
	}
	before := thread.UpdatedAt

	info := provider.SessionInfo{SessionID: "session-abc", Model: "opus"}
	meta, _ := json.Marshal(info)

	evt := provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// EventInit persists the session ref but does not emit on a wire
	// channel — there is no frontend contract for "init arrived".
	if len(*emissions) != 0 {
		t.Fatalf("expected 0 emissions for EventInit, got %d: %+v", len(*emissions), *emissions)
	}

	// Should update session ref.
	thr, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thr.SessionRef != "session-abc" {
		t.Errorf("session ref: got %q, want %q", thr.SessionRef, "session-abc")
	}
	if thr.UpdatedAt != before {
		t.Errorf("updated_at: got %d, want %d", thr.UpdatedAt, before)
	}
}

func TestEventInitFiresEventHook(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	var observed provider.ProviderEvent
	router.SetEventHook(func(evt provider.ProviderEvent) {
		observed = evt
	})

	info := provider.SessionInfo{SessionID: "session-xyz", Model: "opus"}
	meta, _ := json.Marshal(info)
	evt := provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if observed.Kind != provider.EventInit {
		t.Fatalf("eventHook observed = %q, want EventInit", observed.Kind)
	}
	if observed.ThreadID != "t1" {
		t.Fatalf("eventHook threadID = %q, want t1", observed.ThreadID)
	}
}

func TestTextDeltaAccumulation(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	for _, text := range []string{"hello ", "world", "!"} {
		evt := provider.ProviderEvent{
			Kind:      provider.EventTextDelta,
			ThreadID:  "t1",
			Content:   text,
			Timestamp: time.Now(),
		}
		router.Handle(evt)
	}
	if err := router.Wait(context.Background()); err != nil {
		t.Fatalf("wait flush: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Summary != "hello world!" {
		t.Fatalf("summary: got %q, want %q", items[0].Summary, "hello world!")
	}
	if items[0].Status != "streaming" {
		t.Fatalf("status: got %q, want streaming", items[0].Status)
	}
}

func TestTextDeltaEmitsSemanticDeltasWithoutSnapshotSpam(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	for _, text := range []string{"first ", "second", " third"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTextDelta,
			ThreadID:  "t1",
			Content:   text,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle text delta: %v", err)
		}
	}

	events := filterItemStreamEvents(*emissions)
	// One creation upsert + one delta per chunk — no full-snapshot re-upserts
	// per chunk (the "without snapshot spam" guarantee). The creation upsert
	// carries NO content; the first chunk streams as a delta so the frontend
	// smoother animates it instead of seeding it as already-revealed.
	if len(events) != 4 {
		t.Fatalf("provider:item_event count = %d, want 4 (1 creation upsert + 3 deltas): %+v", len(events), events)
	}
	if events[0].Action != itemStreamActionUpsert || events[0].Item == nil {
		t.Fatalf("event[0] = %+v, want initial upsert with item", events[0])
	}
	if events[0].Item.Summary != "" {
		t.Fatalf("initial upsert summary = %q, want empty (content streams as deltas)", events[0].Item.Summary)
	}
	wantDeltas := []string{"first ", "second", " third"}
	for i, want := range wantDeltas {
		event := events[i+1]
		if event.Action != itemStreamActionDelta {
			t.Fatalf("event[%d].action = %q, want delta", i+1, event.Action)
		}
		if event.Delta != want {
			t.Fatalf("event[%d].delta = %q, want %q", i+1, event.Delta, want)
		}
		if event.ThreadID != "t1" || event.ItemID == "" || event.Kind != string(provider.ItemAssistantText) {
			t.Fatalf("event[%d] malformed delta metadata: %+v", i+1, event)
		}
	}
}

// filterEmissions returns the subset of emissions on the given channel
// name. Tests that target a specific Wails channel use this so the
// presence of unrelated channels (provider:item_event, provider:meta,
// etc.) doesn't perturb count assertions.
func filterEmissions(emissions []emitted, channel string) []emitted {
	out := make([]emitted, 0, len(emissions))
	for _, e := range emissions {
		if e.eventName == channel {
			out = append(out, e)
		}
	}
	return out
}

func filterItemEventUpserts(emissions []emitted) []store.Item {
	out := make([]store.Item, 0)
	for _, e := range emissions {
		if e.eventName != "provider:item_event" {
			continue
		}
		event, ok := e.data.(ItemStreamEvent)
		if !ok || event.Action != itemStreamActionUpsert || event.Item == nil {
			continue
		}
		out = append(out, *event.Item)
	}
	return out
}

func filterItemEventPatches(emissions []emitted) []ItemStreamEvent {
	out := make([]ItemStreamEvent, 0)
	for _, e := range emissions {
		if e.eventName != "provider:item_event" {
			continue
		}
		event, ok := e.data.(ItemStreamEvent)
		if !ok || event.Action != itemStreamActionPatch || event.Patch == nil {
			continue
		}
		out = append(out, event)
	}
	return out
}

func compactionItems(items []store.Item) []store.Item {
	out := make([]store.Item, 0)
	for _, item := range items {
		if item.Kind == "compaction" {
			out = append(out, item)
		}
	}
	return out
}

func filterItemStreamEvents(emissions []emitted) []ItemStreamEvent {
	out := make([]ItemStreamEvent, 0)
	for _, e := range emissions {
		if e.eventName != "provider:item_event" {
			continue
		}
		event, ok := e.data.(ItemStreamEvent)
		if !ok {
			continue
		}
		out = append(out, event)
	}
	return out
}

// filterItemEventMetas returns the subset of provider:item_event
// emissions with action=meta. Used by the live path-refs tests to
// assert that mid-stream linkification fires and dedupes without
// scanning unrelated channels. Mirrors filterItemEventUpserts /
// filterItemEventDeltas for the third action variant.
func filterItemEventMetas(emissions []emitted) []ItemStreamEvent {
	out := make([]ItemStreamEvent, 0)
	for _, e := range emissions {
		if e.eventName != "provider:item_event" {
			continue
		}
		event, ok := e.data.(ItemStreamEvent)
		if !ok || event.Action != itemStreamActionMeta {
			continue
		}
		out = append(out, event)
	}
	return out
}

func filterItemEventDeltas(emissions []emitted) []ItemDeltaEvent {
	out := make([]ItemDeltaEvent, 0)
	for _, e := range emissions {
		if e.eventName != "provider:item_event" {
			continue
		}
		event, ok := e.data.(ItemStreamEvent)
		if !ok || event.Action != itemStreamActionDelta {
			continue
		}
		out = append(out, ItemDeltaEvent{
			ThreadID:  event.ThreadID,
			ItemID:    event.ItemID,
			Kind:      event.Kind,
			Delta:     event.Delta,
			UpdatedAt: event.UpdatedAt,
		})
	}
	return out
}

func TestDiffPersistsHeavy(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "tool-1", "Edit: main.go", "file_change", "running")

	patch := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n foo\n+bar\n"
	evt := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		ItemID:    "tool-1",
		Content:   patch,
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) == 0 {
		t.Fatal("expected at least one provider:item_event upsert")
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "tool_call" {
		t.Errorf("item kind: got %q, want %q", items[0].Kind, "tool_call")
	}
	if items[0].PayloadKind != "diff" {
		t.Fatalf("payload kind: got %q, want diff", items[0].PayloadKind)
	}
	data, err := st.GetPayloadData(items[0].PayloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if string(data) != patch {
		t.Fatalf("payload data: got %q, want %q", string(data), patch)
	}
}

func TestDiffReplaceUpsertsExistingPayload(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "tool-1", "Edit: main.go", "file_change", "running")

	first := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		ItemID:    "tool-1",
		Content:   "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n",
		Timestamp: time.Now(),
	}
	if err := router.Handle(first); err != nil {
		t.Fatalf("handle first diff: %v", err)
	}

	second := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		ItemID:    "tool-1",
		Content:   "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n-old\n+new\n+newer\n",
		Replace:   true,
		Timestamp: time.Now(),
	}
	if err := router.Handle(second); err != nil {
		t.Fatalf("handle replacement diff: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item after replacement, got %d", len(items))
	}
	if items[0].PayloadKind != "diff" {
		t.Fatalf("payload kind: got %q, want diff", items[0].PayloadKind)
	}
	data, err := st.GetPayloadData(items[0].PayloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if !strings.Contains(string(data), "+newer") {
		t.Fatalf("expected replacement diff content, got %q", string(data))
	}
	if len(filterItemEventUpserts(*emissions)) < 2 {
		t.Fatalf("expected at least 2 item upserts, got %d", len(filterItemEventUpserts(*emissions)))
	}
}

func TestDiffWithoutReplaceAppendsPayloads(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "tool-1", "Edit: main.go", "file_change", "running")

	first := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		ItemID:    "tool-1",
		Content:   "first diff",
		Timestamp: time.Now(),
	}
	second := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		ItemID:    "tool-1",
		Content:   "second diff",
		Timestamp: time.Now(),
	}

	if err := router.Handle(first); err != nil {
		t.Fatalf("handle first diff: %v", err)
	}
	if err := router.Handle(second); err != nil {
		t.Fatalf("handle second diff: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 tool_call item without replace, got %d", len(items))
	}
	data, err := st.GetPayloadData(items[0].PayloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if string(data) != "first diffsecond diff" {
		t.Fatalf("payload data: got %q, want appended diff chunks", string(data))
	}
}

func TestUpgradeOnlyDiffDoesNotCreateFallbackRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	meta, _ := json.Marshal(map[string]any{
		"upgrade_only": true,
		"source":       "turn/diff/updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n",
		Meta:      meta,
		Replace:   true,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("upgrade-only diff created fallback rows: %+v", items)
	}
}

func TestCommandOutputPersistsHeavy(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "cmd-1", "Bash: go build", "command_execution", "running")

	cmdMeta, _ := json.Marshal(map[string]any{"command": "go build", "exitCode": 0})
	evt := provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  "t1",
		ItemID:    "cmd-1",
		Content:   "building...\nok",
		Meta:      cmdMeta,
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) == 0 {
		t.Fatal("expected at least one provider:item_event upsert")
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "tool_call" {
		t.Errorf("item kind: got %q, want %q", items[0].Kind, "tool_call")
	}
	if items[0].PayloadKind != "command_output" {
		t.Fatalf("payload kind: got %q, want command_output", items[0].PayloadKind)
	}
}

// TestCommandOutputMultipleDeltasAppend pins the append-in-SQLite
// behaviour that replaced the O(N^2) read-append-write path. Two
// separate outputDelta events for the same item_id must cumulate in
// the payload blob, not overwrite each other.
func TestCommandOutputMultipleDeltasAppend(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "cmd-1", "Bash: streaming", "command_execution", "running")

	// First chunk: creates the payload with data "chunk1\n".
	meta1, _ := json.Marshal(map[string]any{"command": "streaming", "exitCode": 0})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  "t1",
		ItemID:    "cmd-1",
		Content:   "chunk1\n",
		Meta:      meta1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle first: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items after first delta: got %d, want 1", len(items))
	}
	payloadID := items[0].PayloadID
	if payloadID == "" {
		t.Fatal("no payload id after first delta")
	}

	// Second chunk: must append, not replace.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  "t1",
		ItemID:    "cmd-1",
		Content:   "chunk2\n",
		Meta:      meta1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle second: %v", err)
	}

	items, err = st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items after second delta: got %d, want 1 (reuse existing)", len(items))
	}
	if items[0].PayloadID != payloadID {
		t.Errorf("payload id changed across deltas: %q vs %q", items[0].PayloadID, payloadID)
	}
	data, err := st.GetPayloadData(payloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	want := "chunk1\nchunk2\n"
	if string(data) != want {
		t.Errorf("payload data after two deltas = %q, want %q", data, want)
	}
}

func TestThinkingPersistsHeavy(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  "t1",
		ItemID:    "claude-thinking-1",
		Content:   "Let me think about this carefully...",
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "thinking" {
		t.Errorf("item kind: got %q, want %q", items[0].Kind, "thinking")
	}
}

func TestProposedPlanPersistsHeavy(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "plan-1",
		Content:   "# Ship it\n\n- one\n- two",
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "tool_call" {
		t.Fatalf("item kind: got %q, want tool_call", items[0].Kind)
	}
	if items[0].ToolName != "plan" {
		t.Fatalf("tool name: got %q, want plan", items[0].ToolName)
	}
	if items[0].PayloadKind != "proposed_plan" {
		t.Fatalf("payload kind: got %q, want proposed_plan", items[0].PayloadKind)
	}
	if items[0].Summary != "Ship it" {
		t.Fatalf("item summary: got %q, want %q", items[0].Summary, "Ship it")
	}

	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) == 0 {
		t.Fatalf("expected provider:item_event upsert, got %+v", *emissions)
	}
	if !strings.Contains(upserts[len(upserts)-1].Meta, `"planVersion":1`) {
		t.Fatalf("final plan upsert meta = %s, want decorated plan state", upserts[len(upserts)-1].Meta)
	}
}

func TestProposedPlanDedupesMatchingContentWithinTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	first := provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "permission-request-1",
		Content:   "# Ship it\n\n- one",
		Timestamp: time.Now(),
	}
	if err := router.Handle(first); err != nil {
		t.Fatalf("handle first: %v", err)
	}
	second := provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "assistant-tool-use-1",
		Content:   "# Ship it\n\n- one",
		Timestamp: time.Now(),
	}
	if err := router.Handle(second); err != nil {
		t.Fatalf("handle duplicate: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].ID != "permission-request-1" {
		t.Fatalf("deduped item id = %q, want first observed id", items[0].ID)
	}
}

func TestProposedPlanDuplicateContentInLaterTurnCreatesNewVersion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	content := "# Ship it\n\n- one"
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "plan-turn-1",
		Content:   content,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle first plan: %v", err)
	}
	if err := st.InsertItem(store.Item{
		ID:        "user:1",
		ThreadID:  "t1",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "same plan again",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert next turn user item: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "plan-turn-2",
		Content:   content,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle second plan: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var planCount int
	for _, item := range items {
		if item.PayloadKind == "proposed_plan" {
			planCount++
		}
	}
	if planCount != 2 {
		t.Fatalf("plan items = %d, want 2", planCount)
	}
}

func TestTurnCompleteExtractsProposedPlanFromAssistantText(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "<proposed_plan>\n# Ship it\n\n- one\n- two\n</proposed_plan>",
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("handle text delta: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("handle turn complete: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "assistant_text" {
		t.Fatalf("item kind: got %q, want assistant_text", items[0].Kind)
	}
	if items[0].Status != "completed" {
		t.Fatalf("status: got %q, want completed", items[0].Status)
	}
}

func TestReasoningDeltasPersistOnTurnComplete(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	for _, chunk := range []string{"Need ", "more ", "analysis"} {
		err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventThinking,
			ThreadID:  "t1",
			Content:   chunk,
			Timestamp: time.Now(),
		})
		if err != nil {
			t.Fatalf("handle reasoning delta: %v", err)
		}
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items before turn complete: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 persisted thinking item before turn complete, got %d", len(items))
	}
	if items[0].Status != "streaming" {
		t.Fatalf("status before turn complete: got %q, want streaming", items[0].Status)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("handle turn complete: %v", err)
	}

	items, err = st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items after turn complete: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 persisted thinking item, got %d", len(items))
	}
	if items[0].Kind != "thinking" {
		t.Fatalf("expected thinking item, got %q", items[0].Kind)
	}
	if items[0].Summary != "Need more analysis" {
		t.Fatalf("expected accumulated reasoning, got %q", items[0].Summary)
	}
	if items[0].Status != "completed" {
		t.Fatalf("status after turn complete: got %q, want completed", items[0].Status)
	}

	if len(filterItemEventUpserts(*emissions)) < 1 {
		t.Fatalf("expected initial streaming upsert, got %+v", *emissions)
	}
	if len(filterItemEventPatches(*emissions)) < 1 {
		t.Fatalf("expected settle patch, got %+v", *emissions)
	}
}

func TestReasoningSummaryIsBoundedButPayloadKeepsFullContent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Two deltas summing to 500 runes: more than `thinkingPreviewRunes`
	// (400), so the persisted summary must tail-cap to exactly 400
	// characters drawn from the END of the accumulated reasoning.
	chunks := []string{strings.Repeat("a", 300), strings.Repeat("b", 200)}
	for _, chunk := range chunks {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventThinking,
			ThreadID:  "t1",
			Content:   chunk,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle reasoning delta: %v", err)
		}
	}
	if err := router.Wait(context.Background()); err != nil {
		t.Fatalf("wait flush: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if got := len([]rune(items[0].Summary)); got != thinkingPreviewRunes {
		t.Fatalf("summary runes = %d, want %d (tail-cap)", got, thinkingPreviewRunes)
	}
	// Tail-cap: the last 400 chars come from chunks[0]+chunks[1] =
	// 300 'a' + 200 'b'. The tail is 200 'a' + 200 'b'.
	wantSummary := strings.Repeat("a", 200) + strings.Repeat("b", 200)
	if items[0].Summary != wantSummary {
		t.Fatalf("summary = %q, want %q (tail of accumulated reasoning)", items[0].Summary, wantSummary)
	}
	data, err := st.GetPayloadData(items[0].PayloadID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if string(data) != chunks[0]+chunks[1] {
		t.Fatalf("payload = %d bytes, want full reasoning", len(data))
	}
}

func TestThinkingDeltaEmitsSemanticDeltasWithoutSnapshotSpam(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	for _, text := range []string{"plan ", "more", " done"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventThinking,
			ThreadID:  "t1",
			Content:   text,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle thinking delta: %v", err)
		}
	}

	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) != 1 {
		t.Fatalf("provider:item_event upsert count = %d, want 1 initial row: %+v", len(upserts), upserts)
	}
	deltas := filterItemEventDeltas(*emissions)
	if len(deltas) != 2 {
		t.Fatalf("provider:item_event delta count = %d, want 2 follow-up deltas: %+v", len(deltas), deltas)
	}

	if err := router.Wait(context.Background()); err != nil {
		t.Fatalf("wait flush: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	data, err := st.GetPayloadData(items[0].PayloadID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if string(data) != "plan more done" {
		t.Fatalf("payload = %q, want full thinking content", string(data))
	}
}

func TestLateTextDeltaDoesNotResurrectSettledRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "hello",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle initial text: %v", err)
	}
	textRow, found, err := st.GetThreadItem("t1", "text:0:1")
	if err != nil || !found {
		t.Fatalf("get text row: found=%v err=%v", found, err)
	}
	textRow.Status = statusCompleted
	textRow.Summary = "hello"
	textRow.PayloadID = ""
	textRow.UpdatedAt = time.Now().UnixMilli()
	if _, err := st.UpsertItem(textRow, nil); err != nil {
		t.Fatalf("settle text row: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   " late",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle late text: %v", err)
	}

	item, found, err := st.GetThreadItem("t1", "text:0:1")
	if err != nil || !found {
		t.Fatalf("get text row: found=%v err=%v", found, err)
	}
	if item.Status != statusCompleted {
		t.Fatalf("status = %q, want completed", item.Status)
	}
	if item.Summary != "hello" {
		t.Fatalf("late delta mutated summary: %q", item.Summary)
	}
}

func TestLateThinkingDeltaDoesNotResurrectSettledRowOrPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  "t1",
		Content:   "first",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle initial thinking: %v", err)
	}
	item, found, err := st.GetThreadItem("t1", "think:0:1")
	if err != nil || !found {
		t.Fatalf("get thinking row: found=%v err=%v", found, err)
	}
	item.Status = statusCompleted
	item.UpdatedAt = time.Now().UnixMilli()
	if _, err := st.UpsertItem(item, nil); err != nil {
		t.Fatalf("settle thinking row: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  "t1",
		Content:   " late",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle late thinking: %v", err)
	}
	if err := router.Wait(context.Background()); err != nil {
		t.Fatalf("wait flush late thinking: %v", err)
	}

	settled, found, err := st.GetThreadItem("t1", item.ID)
	if err != nil || !found {
		t.Fatalf("get settled thinking row: found=%v err=%v", found, err)
	}
	if settled.Status != statusCompleted {
		t.Fatalf("status = %q, want completed", settled.Status)
	}
	if settled.Summary != "first" {
		t.Fatalf("late delta mutated summary: %q", settled.Summary)
	}
	data, err := st.GetPayloadData(item.PayloadID)
	if err != nil {
		t.Fatalf("get thinking payload: %v", err)
	}
	if string(data) != "first" {
		t.Fatalf("late delta mutated payload: %q", string(data))
	}
}

func TestTurnCompleteWithAccumulatedText(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Accumulate text.
	for _, text := range []string{"Hello ", "world!"} {
		router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTextDelta,
			ThreadID:  "t1",
			Content:   text,
			Timestamp: time.Now(),
		})
	}

	// Turn complete.
	router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	})

	// Should have persisted a text item.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "assistant_text" {
		t.Errorf("item kind: got %q, want %q", items[0].Kind, "assistant_text")
	}
	if items[0].Role != "assistant" {
		t.Errorf("item role: got %q, want %q", items[0].Role, "assistant")
	}
	if items[0].Summary != "Hello world!" {
		t.Errorf("item summary: got %q, want %q", items[0].Summary, "Hello world!")
	}
	if items[0].Status != "completed" {
		t.Errorf("status: got %q, want completed", items[0].Status)
	}

	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) != 1 {
		t.Errorf("expected 1 initial streaming upsert (settle is a patch), got %d", len(upserts))
	}
	patches := filterItemEventPatches(*emissions)
	if len(patches) != 1 {
		t.Errorf("expected 1 settle patch, got %d", len(patches))
	}
	if len(patches) > 0 {
		patch := patches[0]
		if patch.Patch == nil || patch.Patch.Status == nil || *patch.Patch.Status != "completed" {
			t.Errorf("settle patch should set status=completed, got %+v", patch.Patch)
		}
	}
}

func TestTurnCompleteWithoutAccumulatedText(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	})

	// No text accumulated, so no item should be persisted.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}

	// Empty turn produces no item upserts.
	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) != 0 {
		t.Errorf("expected 0 item upserts for empty turn, got %d: %+v", len(upserts), upserts)
	}
	// A bare EventTurnComplete arriving without a preceding
	// EventTurnStart has no open wire round (currentRoundByThread is empty
	// because setOpenRound was never called for this thread). Under
	// the per-round emission cadence (see internal/triage/AGENTS.md
	// "Wire-round vs logical-turn") the frontend therefore sees
	// nothing — there was no turn_started to pair with, so there's
	// nothing to clear. Persistence still ran (no items to settle, no
	// turns row to update because turn_start never fired). The
	// frontend's indicator stays in whatever state it was in,
	// untouched by this orphan complete.
	completed := filterEmissions(*emissions, "provider:turn_completed")
	if len(completed) != 0 {
		t.Errorf("expected 0 provider:turn_completed emissions for orphan complete, got %d: %+v", len(completed), completed)
	}
}

func TestTurnCompleteDoesNotAutoRenameClaudeThread(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		ProjectID:     triageTestProjectID,
		Title:         "New Thread",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if err := st.InsertItem(store.Item{
		ID:        "user-1",
		ThreadID:  "t1",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "Fix flaky reconnect logic after sleep resumes. It breaks after laptop wake.\nExtra detail.",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert user item: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("handle turn complete: %v", err)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.Title != "New Thread" {
		t.Fatalf("thread title: got %q", thread.Title)
	}

	// Auto-rename heuristics live on the app layer, not in triage. Turn
	// completion here should NOT flip the title nor emit a thread row
	// update for the old Claude-thread fallback.
	updates := filterEmissions(*emissions, "thread:updated")
	if len(updates) != 0 {
		t.Fatalf("expected 0 thread:updated emissions, got %d (%+v)", len(updates), updates)
	}
}

// -- Error propagation tests --

// TestPersistDropsInvalidParentID verifies the spec invariant that
// parent_id must point to a tool_call row. The router enforces this
// defensively via persistItem:
//   - Self-reference drops the link with a log.
//   - An existing non-tool_call parent drops the link.
//   - Cycles along the parent chain drop the link.
//
// A parent that doesn't exist yet is intentionally NOT dropped —
// subagent streaming text/thinking can reference a Task tool_call that
// materializes later in the same turn.
func TestPersistDropsInvalidParentID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Seed a non-tool_call row (assistant_text) we can point at to
	// trigger the "parent exists but is wrong kind" branch.
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:        "text-parent",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "not a tool call",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed text parent: %v", err)
	}
	// Seed a tool_call with a ParentID that points at itself. Used by
	// the cycle test below.
	if _, err := st.AppendItem(store.Item{
		ID:        "cycle-a",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		Summary:   "cycle a",
		ToolName:  "Bash",
		ParentID:  "cycle-a",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed cycle parent: %v", err)
	}

	// 1. Self-parent is dropped.
	selfItem := store.Item{
		ID:        "self-parent",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		Summary:   "self-parented",
		ToolName:  "Bash",
		ParentID:  "self-parent",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := router.PersistItem(selfItem, nil); err != nil {
		t.Fatalf("persist self-parent: %v", err)
	}
	got, found, err := st.GetThreadItem("t1", "self-parent")
	if err != nil || !found {
		t.Fatalf("get self-parent: err=%v found=%v", err, found)
	}
	if got.ParentID != "" {
		t.Errorf("self-parent kept ParentID %q, want dropped", got.ParentID)
	}

	// 2. Parent exists but is NOT a tool_call — drop the link.
	wrongKindItem := store.Item{
		ID:        "wrong-kind-child",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		Summary:   "wrong-kind parent",
		ToolName:  "Bash",
		ParentID:  "text-parent",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := router.PersistItem(wrongKindItem, nil); err != nil {
		t.Fatalf("persist wrong-kind: %v", err)
	}
	got, _, _ = st.GetThreadItem("t1", "wrong-kind-child")
	if got.ParentID != "" {
		t.Errorf("wrong-kind child kept ParentID %q, want dropped", got.ParentID)
	}

	// 3. Parent chain cycles back → drop the link.
	cycleChild := store.Item{
		ID:        "cycle-child",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		Summary:   "cycle child",
		ToolName:  "Bash",
		ParentID:  "cycle-a", // cycle-a points back at itself
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := router.PersistItem(cycleChild, nil); err != nil {
		t.Fatalf("persist cycle child: %v", err)
	}
	got, _, _ = st.GetThreadItem("t1", "cycle-child")
	if got.ParentID != "" {
		t.Errorf("cycle child kept ParentID %q, want dropped", got.ParentID)
	}

	// 4. Unknown parent_id — PRESERVED so a late-arriving parent row
	//    still links up correctly (subagent streaming text ordering).
	yetToArriveItem := store.Item{
		ID:        "early-child",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "streaming",
		Summary:   "early subagent text",
		ParentID:  "task_tool_future",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := router.PersistItem(yetToArriveItem, nil); err != nil {
		t.Fatalf("persist early child: %v", err)
	}
	got, _, _ = st.GetThreadItem("t1", "early-child")
	if got.ParentID != "task_tool_future" {
		t.Errorf("unknown parent was dropped (%q) — should be preserved for late arrival", got.ParentID)
	}
}

func TestErrorPersistsTimelineItem(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  "t1",
		Content:   "provider blew up",
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "error" {
		t.Fatalf("kind: got %q, want error", items[0].Kind)
	}
	if items[0].Summary != "provider blew up" {
		t.Fatalf("summary: got %q, want provider blew up", items[0].Summary)
	}

	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) != 1 {
		t.Fatalf("expected 1 provider:item_event upsert, got %+v", *emissions)
	}
}

func TestErrorSplitsAssistantTextAroundVisibleErrorRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "before error",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first text delta: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  "t1",
		Content:   "recoverable provider warning",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("error: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "after error",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second text delta: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected text, error, text; got %d items: %+v", len(items), items)
	}
	if items[0].Kind != "assistant_text" || items[0].Summary != "before error" {
		t.Fatalf("first item = (%q, %q), want pre-error assistant text", items[0].Kind, items[0].Summary)
	}
	if items[1].Kind != "error" || items[1].Summary != "recoverable provider warning" {
		t.Fatalf("second item = (%q, %q), want error row", items[1].Kind, items[1].Summary)
	}
	if items[2].Kind != "assistant_text" || items[2].Summary != "after error" {
		t.Fatalf("third item = (%q, %q), want post-error assistant text", items[2].Kind, items[2].Summary)
	}
}

func TestUnscopedErrorSplitsScopedAssistantTextAroundVisibleErrorRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	const parentToolUseID = "task-tool-1"

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        "t1",
		Content:         "scoped before error",
		ParentToolUseID: parentToolUseID,
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("first scoped text delta: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  "t1",
		Content:   "root provider warning",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("unscoped error: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        "t1",
		Content:         "scoped after error",
		ParentToolUseID: parentToolUseID,
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("second scoped text delta: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected scoped text, unscoped error, scoped text; got %d items: %+v", len(items), items)
	}
	if items[0].Kind != "assistant_text" || items[0].Summary != "scoped before error" || items[0].ParentID != parentToolUseID {
		t.Fatalf("first item = (%q, %q, parent %q), want pre-error scoped assistant text", items[0].Kind, items[0].Summary, items[0].ParentID)
	}
	if items[1].Kind != "error" || items[1].Summary != "root provider warning" || items[1].ParentID != "" {
		t.Fatalf("second item = (%q, %q, parent %q), want unscoped error row", items[1].Kind, items[1].Summary, items[1].ParentID)
	}
	if items[2].Kind != "assistant_text" || items[2].Summary != "scoped after error" || items[2].ParentID != parentToolUseID {
		t.Fatalf("third item = (%q, %q, parent %q), want post-error scoped assistant text", items[2].Kind, items[2].Summary, items[2].ParentID)
	}
}

// TestFatalErrorOrderingMatchesSpec pins the ordering contract on a
// fatal EventError, per chat-rewrite §"Live provider-crash flip":
//
//  1. flip every streaming/running item in the turn to errored
//  2. persist the error row
//  3. drain any queued completions as errored
//  4. synthesize TurnComplete with TruncatedTurnCompleteMeta when no wire
//     TurnComplete is expected
//
// Earlier code drained before creating the error row; the emission
// ordering below locks the spec sequence so a regression surfaces as
// a loud assertion failure rather than a subtle UX glitch.
func TestFatalErrorOrderingMatchesSpec(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// 1. Start a turn with a streaming text item AND a queued background
	//    tool completion. The background starts before the text block
	//    opens so handleToolStart's settleStreamingScope doesn't close
	//    the (still-to-be-opened) text block.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "bg"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-1",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "hello",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	// Post-refactor: EventToolComplete on the backgrounded placeholder
	// is a no-op for sibling creation. The EventBackgroundTaskTerminal
	// fires the sibling-row upsert, which queues behind the streaming
	// text item.
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-1",
		"tool_use_id": "bg-1",
		"status":      "completed",
		"exit_code":   0,
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-1",
		Meta: terminalMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal: %v", err)
	}

	// Baseline check: queued completion has NOT persisted yet (sits
	// behind the streaming text block).
	done := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(done) != 0 {
		t.Fatalf("background_done should still be queued, got %d", len(done))
	}

	// Clear emissions up to this point — we only want the fatal-error
	// fan-out in the sequence check.
	*emissions = (*emissions)[:0]

	// 2. Fire a fatal EventError. This exercises the spec ordering.
	fatalMeta, _ := json.Marshal(map[string]any{"fatal": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: "provider crashed", Meta: fatalMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("fatal error: %v", err)
	}

	// Walk the upsert emissions and extract the fatal-phase items in
	// order. We care specifically about:
	//   - the flipped streaming text item (Kind=assistant_text, Status=errored)
	//   - the new error row (Kind=error)
	//   - the drained background_done row (Kind=background_done, Status=errored)
	upserts := filterItemEventUpserts(*emissions)
	var sequence []string
	for _, item := range upserts {
		switch {
		case item.Kind == "assistant_text" && item.Status == "errored":
			sequence = append(sequence, "flip_text")
		case item.Kind == "error":
			sequence = append(sequence, "create_error")
		case item.Kind == itemKindBackgroundDone && item.Status == "errored":
			sequence = append(sequence, "drain_bg_done")
		}
	}

	if len(sequence) < 3 {
		t.Fatalf("expected flip_text, create_error, drain_bg_done in order; got %+v (upserts=%+v)", sequence, upserts)
	}

	// Spec ordering: flip_text MUST come before create_error, and
	// create_error MUST come before drain_bg_done. Anything else is a
	// regression.
	flipIdx := indexOf(sequence, "flip_text")
	errorIdx := indexOf(sequence, "create_error")
	drainIdx := indexOf(sequence, "drain_bg_done")
	if !(flipIdx < errorIdx && errorIdx < drainIdx) {
		t.Fatalf("fatal-error emission ordering violated: flip=%d error=%d drain=%d sequence=%+v",
			flipIdx, errorIdx, drainIdx, sequence)
	}

}

// TestFatalErrorSynthesizesTurnCompleteWhenNoWireExpected verifies the
// final step of the spec ordering: absent an `expect_turn_complete`
// opt-in on the error meta, the router must synthesize an
// EventTurnComplete with TruncatedTurnCompleteMeta so the frontend working
// indicator flips off without needing the wire event. The synthesis
// is observable via the test-only event hook, which fires for
// EVERY Handle invocation — including the recursive one.
func TestFatalErrorSynthesizesTurnCompleteWhenNoWireExpected(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	observed := make(chan provider.ProviderEvent, 8)
	router.SetEventHook(func(evt provider.ProviderEvent) {
		observed <- evt
	})

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "hi",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	fatalMeta, _ := json.Marshal(map[string]any{"fatal": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: "subprocess exit", Meta: fatalMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("fatal error: %v", err)
	}

	// Drain all observed events and look for the synthesized
	// TurnComplete. The typed payload has Synthetic=true so we
	// can distinguish it from a real wire TurnComplete.
	close(observed)
	var gotSynthesized bool
	for evt := range observed {
		if evt.Kind != provider.EventTurnComplete {
			continue
		}
		meta, ok := evt.TurnComplete.(*provider.TruncatedTurnCompleteMeta)
		if ok && meta != nil && meta.Synthetic {
			gotSynthesized = true
		}
	}

	if !gotSynthesized {
		t.Fatal("expected a synthesized TurnComplete after fatal without expect_turn_complete opt-in")
	}
}

// TestFatalErrorSkipsSynthesisWhenExpectTurnComplete guards the
// opt-in case: a fatal error on a still-alive session carrying
// `expect_turn_complete:true` must NOT synthesize, because a real
// wire TurnComplete is still coming. Double-emitting would leave
// the working indicator in a confusing "just-flipped" state.
func TestFatalErrorSkipsSynthesisWhenExpectTurnComplete(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	observed := make(chan provider.ProviderEvent, 8)
	router.SetEventHook(func(evt provider.ProviderEvent) {
		observed <- evt
	})

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	fatalMeta, _ := json.Marshal(map[string]any{"fatal": true, "expect_turn_complete": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: "t1",
		Content: "mid-turn refusal", Meta: fatalMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("fatal error: %v", err)
	}

	close(observed)
	for evt := range observed {
		if evt.Kind != provider.EventTurnComplete {
			continue
		}
		var meta map[string]any
		if len(evt.Meta) > 0 {
			_ = json.Unmarshal(evt.Meta, &meta)
		}
		if synth, _ := meta["synthetic"].(bool); synth {
			t.Fatal("synthesized TurnComplete must NOT fire when expect_turn_complete is true")
		}
	}
}

// indexOf returns the index of value in slice, or -1 if absent. Used
// only by TestFatalErrorOrderingMatchesSpec for readable assertions.
func indexOf(haystack []string, needle string) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}

func TestFatalErrorFlipsStreamingItemsToErrored(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "hello",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   " buffered",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("buffered text delta: %v", err)
	}

	meta, _ := json.Marshal(map[string]any{"fatal": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  "t1",
		Content:   "fatal session failure",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("fatal error: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected text item plus error item, got %d", len(items))
	}
	if items[0].Kind != "assistant_text" {
		t.Fatalf("first item kind: got %q, want assistant_text", items[0].Kind)
	}
	if items[0].Status != "errored" {
		t.Fatalf("text status: got %q, want errored", items[0].Status)
	}
	if items[0].Summary != "hello buffered — interrupted" {
		t.Fatalf("text summary after fatal flip = %q, want buffered text plus interrupted suffix", items[0].Summary)
	}
	if items[1].Kind != "error" {
		t.Fatalf("second item kind: got %q, want error", items[1].Kind)
	}

	upserts := filterItemEventUpserts(*emissions)
	if len(upserts) < 3 {
		t.Fatalf("expected streaming upsert, flip upsert, and error upsert; got %+v", *emissions)
	}
}

func TestPersistHeavyReturnsErrorOnClosedStore(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "cmd-1", "Bash: go build", "command_execution", "running")

	// Close the store to force insertion failures.
	st.Close()

	evt := provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  "t1",
		ItemID:    "cmd-1",
		Content:   "line 1",
		Timestamp: time.Now(),
	}

	err := router.Handle(evt)
	if err == nil {
		t.Fatal("expected error from Handle when store is closed")
	}

	if len(*emissions) != 0 {
		t.Fatalf("expected 0 emissions on failed persist, got %d", len(*emissions))
	}
}

func TestTurnCompleteReturnsErrorOnClosedStore(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Install the test-only router hook so we can observe Handle calls
	// even after persistence fails. Every Handle defer-fires the hook
	// regardless of return status.
	observed := make(chan provider.ProviderEvent, 4)
	router.SetEventHook(func(evt provider.ProviderEvent) {
		observed <- evt
	})

	// Accumulate text before closing the store.
	router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "hello",
		Timestamp: time.Now(),
	})

	// Close the store to force insertion failure.
	st.Close()

	err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	})

	if err == nil {
		t.Fatal("expected error from Handle when store is closed and text accumulated")
	}

	// Drain the hook channel and verify the turn-complete observation fired.
	foundTurnComplete := false
	for len(observed) > 0 {
		evt := <-observed
		if evt.Kind == provider.EventTurnComplete {
			foundTurnComplete = true
		}
	}
	if !foundTurnComplete {
		t.Fatal("expected turn_complete eventHook observation even on persistence failure")
	}
}

// -- buildSummary tests --

func TestBuildSummaryDiff(t *testing.T) {
	meta := `{"filePath":"src/main.go","changeKind":"modified","insertions":5,"deletions":3,"preview":"..."}`
	summary := buildSummary("diff", meta)

	if summary != "modified: +5/-3 src/main.go" {
		t.Errorf("got %q, want %q", summary, "modified: +5/-3 src/main.go")
	}
}

func TestBuildSummaryCommandOutput(t *testing.T) {
	meta := `{"command":"go build","exitCode":0,"lineCount":15,"preview":"..."}`
	summary := buildSummary("command_output", meta)

	if summary != "$ go build (exit 0, 15 lines)" {
		t.Errorf("got %q, want %q", summary, "$ go build (exit 0, 15 lines)")
	}
}

func TestBuildSummaryThinking(t *testing.T) {
	meta := `{"tokenCount":100,"preview":"Let me think about this"}`
	summary := buildSummary("thinking", meta)

	if summary != "Let me think about this" {
		t.Errorf("got %q, want %q", summary, "Let me think about this")
	}
}

// TestCleanupThreadDropsLateEvents exercises Bug B5: events that arrive
// AFTER CleanupThread has been called must not be persisted under the
// stopped thread. The pre-fix router happily wrote rows into a thread
// whose session had been stopped because CleanupThread only cleared
// accumulator state.
func TestCleanupThreadDropsLateEvents(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// First event arrives and is persisted normally.
	before := provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "plan-1",
		Content:   "# Before\n\n- one",
		Timestamp: time.Now(),
	}
	if err := router.Handle(before); err != nil {
		t.Fatalf("handle before: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items before: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("pre-stop items = %d, want 1 (setup broken)", len(items))
	}

	// Stop the thread: simulates StopSession cleanup.
	router.CleanupThread("t1")

	// Late event — would arrive from a readLoop draining in-flight
	// stdout lines after StopSession returned.
	after := provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "plan-2",
		Content:   "# After\n\n- late",
		Timestamp: time.Now(),
	}
	if err := router.Handle(after); err != nil {
		t.Fatalf("handle after stop: %v", err)
	}

	// Persistence must have been suppressed.
	items, err = st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items after: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("post-stop items = %d, want 1 (late event persisted under stopped thread)", len(items))
	}
}

func TestProposedPlanUsesRevisionSourceParentFromTurnMetadata(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "plan-1",
		Content:   "# Plan 1\n\n- one",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle first plan: %v", err)
	}
	if err := st.InsertItem(store.Item{
		ID:        "user:1",
		ThreadID:  "t1",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "Please revise the plan.",
		Meta:      `{"revisionSourceProposedPlan":{"threadId":"t1","itemId":"plan-1"}}`,
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert revision user item: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "plan-2",
		Content:   "# Plan 2\n\n- two",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle revised plan: %v", err)
	}

	state, found, err := st.GetProposedPlanState("t1", "plan-2")
	if err != nil {
		t.Fatalf("GetProposedPlanState: %v", err)
	}
	if !found || state.RevisionParentItemID != "plan-1" {
		t.Fatalf("revision parent = %+v, want plan-1", state)
	}
}

func TestDuplicateTurnStartDoesNotReacceptRevisionComments(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	now := time.Now().UnixMilli()

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "t1",
		ItemID:    "plan-1",
		Content:   "# Plan\n\n- one",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle plan: %v", err)
	}
	if _, err := st.CreateProposedPlanComment(store.ProposedPlanComment{
		ID: "comment-1", ThreadID: "t1", PlanItemID: "plan-1", StartLine: 1, EndLine: 1,
		Body: "revise", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	if err := st.InsertItem(store.Item{
		ID:        "user:1",
		ThreadID:  "t1",
		TurnIndex: 1,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "Please revise the plan.",
		Meta:      `{"revisionSourceProposedPlan":{"threadId":"t1","itemId":"plan-1"},"revisionSourceCommentIds":["comment-1"]}`,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert revision user item: %v", err)
	}
	turnStart := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 1,
		Timestamp: time.Now(),
	}
	if err := router.Handle(turnStart); err != nil {
		t.Fatalf("handle first turn start: %v", err)
	}
	if _, err := st.UpdateProposedPlanComment("t1", "comment-1", store.ProposedPlanCommentUpdate{Body: "new draft"}, now+1); err != nil {
		t.Fatalf("update comment back to draft: %v", err)
	}
	if err := router.Handle(turnStart); err != nil {
		t.Fatalf("handle duplicate turn start: %v", err)
	}
	comment, err := st.GetProposedPlanComment("t1", "comment-1")
	if err != nil {
		t.Fatalf("get comment: %v", err)
	}
	if comment.Status != "draft" {
		t.Fatalf("comment status = %q, want draft after duplicate turn start", comment.Status)
	}
}

// TestCleanupThreadDropsRapidInFlight exercises the tight race: many
// events interleave with CleanupThread; no partial state remains.
func TestCleanupThreadDropsRapidInFlight(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "tight")

	// Fire 50 plan items sequentially, then stop halfway through.
	total := 50
	stopAt := 25
	for i := 0; i < total; i++ {
		if i == stopAt {
			router.CleanupThread("tight")
		}
		evt := provider.ProviderEvent{
			Kind:      provider.EventProposedPlan,
			ThreadID:  "tight",
			ItemID:    fmt.Sprintf("plan-%d", i),
			Content:   fmt.Sprintf("# Plan %d\n\n- line", i),
			Timestamp: time.Now(),
		}
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}

	items, err := st.ListItems("tight")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != stopAt {
		t.Fatalf("items = %d, want %d (stop dropped either too few or too many)", len(items), stopAt)
	}
}

// TestCleanupThreadCanBeUndoneByNewEvents verifies CleanupThread is NOT
// sticky: after cleanup, if the thread is restarted (a new StartSession
// reintroduces events), those new events should persist. The bug-fix
// flag must reset implicitly when the thread sees activity again OR
// a restart routine. We model the "restart" as a re-emission with a
// fresh init-like event; the router clears the stopped marker on
// EventInit for the same thread.
func TestCleanupThreadDoesNotPoisonFutureSessions(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "restart")

	router.CleanupThread("restart")

	// Simulate a restart: EventInit arrives for this thread (StartSession
	// fires one as part of the claude handshake).
	info := provider.SessionInfo{SessionID: "new-sid", Model: "opus"}
	meta, _ := json.Marshal(info)
	initEvt := provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "restart",
		Meta:      meta,
		Timestamp: time.Now(),
	}
	if err := router.Handle(initEvt); err != nil {
		t.Fatalf("handle init: %v", err)
	}

	// A subsequent diff must persist — the stopped marker from the
	// earlier CleanupThread cannot continue to suppress the restart.
	after := provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		ThreadID:  "restart",
		ItemID:    "plan-after-restart",
		Content:   "# Restarted\n\n- fresh",
		Timestamp: time.Now(),
	}
	if err := router.Handle(after); err != nil {
		t.Fatalf("handle after restart: %v", err)
	}
	items, err := st.ListItems("restart")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (stopped marker leaked into new session)", len(items))
	}
}

// TestHandleEventSubagentNotification_EmitsPassthrough confirms the
// Wave 2 emission-only contract for the Codex `<subagent_notification>`
// tag: the handler fans out a provider:subagent_notification event
// carrying the raw Meta payload but does NOT persist an item (the
// tray / subagent UI will decide what to surface later). The handler
// must also not error — it's a pure pass-through.
func TestHandleEventSubagentNotification_EmitsPassthrough(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	meta := json.RawMessage(`{"agent_path":"child-123","status":"completed"}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventSubagentNotification,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent notification: %v", err)
	}

	// No persistence — subagent notifications are not timeline rows.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("subagent notification must not persist items; got %+v", items)
	}

	notif := filterEmissions(*emissions, "provider:subagent_notification")
	if len(notif) != 1 {
		t.Fatalf("expected 1 provider:subagent_notification emission, got %d", len(notif))
	}
	payload, ok := notif[0].data.(SubagentNotificationEvent)
	if !ok {
		t.Fatalf("emission payload type = %T, want SubagentNotificationEvent", notif[0].data)
	}
	if payload.ThreadID != "t1" {
		t.Errorf("ThreadID = %q, want t1", payload.ThreadID)
	}
	if string(payload.Meta) != string(meta) {
		t.Errorf("payload.Meta = %q, want %q (raw passthrough)", string(payload.Meta), string(meta))
	}
}

func TestPersistItemFieldsAndPatch_NoEmitOnStoreError(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	status := "completed"
	err := router.persistItemFieldsAndPatch("t1", "nonexistent-item", "assistant_text", store.ItemPartialUpdate{
		Status: &status,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent item, got nil")
	}
	patches := filterItemEventPatches(*emissions)
	if len(patches) != 0 {
		t.Fatalf("expected 0 patch emissions on store error, got %d", len(patches))
	}
}

func TestPersistItemFieldsAndPatch_EmitsPatchOnSuccess(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:        "text:0:0",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "streaming",
		Summary:   "hello world",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append item: %v", err)
	}

	newStatus := "completed"
	newMeta := `{"pathRefs":[]}`
	newUpdatedAt := now + 1000
	if err := router.persistItemFieldsAndPatch("t1", "text:0:0", "assistant_text", store.ItemPartialUpdate{
		Status:    &newStatus,
		Meta:      &newMeta,
		UpdatedAt: &newUpdatedAt,
	}); err != nil {
		t.Fatalf("persist and patch: %v", err)
	}

	patches := filterItemEventPatches(*emissions)
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch emission, got %d", len(patches))
	}
	p := patches[0]
	if p.ItemID != "text:0:0" {
		t.Errorf("patch itemId = %q, want text:0:0", p.ItemID)
	}
	if p.Kind != "assistant_text" {
		t.Errorf("patch kind = %q, want assistant_text", p.Kind)
	}
	if p.Patch.Status == nil || *p.Patch.Status != "completed" {
		t.Errorf("patch status = %v, want completed", p.Patch.Status)
	}
	if p.Patch.Meta == nil || *p.Patch.Meta != `{"pathRefs":[]}` {
		t.Errorf("patch meta = %v, want {\"pathRefs\":[]}", p.Patch.Meta)
	}
	if p.Patch.UpdatedAt == nil || *p.Patch.UpdatedAt != newUpdatedAt {
		t.Errorf("patch updatedAt = %v, want %d", p.Patch.UpdatedAt, newUpdatedAt)
	}
	if p.Patch.Summary != nil {
		t.Errorf("patch summary must be nil when not updated, got %v", p.Patch.Summary)
	}

	item, found, err := st.GetThreadItem("t1", "text:0:0")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !found {
		t.Fatal("item not found after update")
	}
	if item.Status != "completed" {
		t.Errorf("stored status = %q, want completed", item.Status)
	}
	if item.Summary != "hello world" {
		t.Errorf("stored summary must be unchanged, got %q", item.Summary)
	}
}
