package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

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

	previewLines := strings.Split(cm.Preview, "\n")
	if len(previewLines) != 10 {
		t.Errorf("preview lines: got %d, want 10", len(previewLines))
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

	previewLines := strings.Split(cm.Preview, "\n")
	if len(previewLines) != 3 {
		t.Errorf("preview lines: got %d, want 3", len(previewLines))
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
	now := time.Now().UnixMilli()
	err := st.CreateThread(store.Thread{
		ID:            id,
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

func TestInlineEventEmit(t *testing.T) {
	router, _, emissions := newTestRouter(t)

	evt := provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "hello ",
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(*emissions) != 1 {
		t.Fatalf("expected 1 emission, got %d", len(*emissions))
	}
	if (*emissions)[0].eventName != "provider:event" {
		t.Errorf("eventName: got %q, want %q", (*emissions)[0].eventName, "provider:event")
	}
}

func TestNewInlineEventsEmit(t *testing.T) {
	router, _, emissions := newTestRouter(t)

	events := []provider.ProviderEvent{
		{Kind: provider.EventToolProgress, ThreadID: "t1", Content: "progress", Timestamp: time.Now()},
		{Kind: provider.EventCompactBoundary, ThreadID: "t1", Timestamp: time.Now()},
		{Kind: provider.EventRateLimits, ThreadID: "t1", Timestamp: time.Now()},
	}

	for _, evt := range events {
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle %s: %v", evt.Kind, err)
		}
	}

	if len(*emissions) != len(events) {
		t.Fatalf("expected %d emissions, got %d", len(events), len(*emissions))
	}
	for i, em := range *emissions {
		if em.eventName != "provider:event" {
			t.Fatalf("emission %d eventName: got %q, want provider:event", i, em.eventName)
		}
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
	if len(*emissions) != 1 || (*emissions)[0].eventName != "provider:event" {
		t.Fatalf("expected inline emission for model reroute, got %+v", *emissions)
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
	if len(*emissions) != 1 || (*emissions)[0].eventName != "provider:event" {
		t.Fatalf("expected inline emission for thread rename, got %+v", *emissions)
	}
}

func TestTokenUsageAddsCalculatedCost(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		Title:         "Cost Test",
		Provider:      "codex",
		WorkspacePath: "/tmp",
		Model:         "gpt-5.4",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	meta, err := json.Marshal(provider.TokenUsage{
		InputTokens:  2_000_000,
		OutputTokens: 1_000_000,
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

	evt := emittedProviderEvent(t, emissions)
	var usage provider.TokenUsage
	if err := json.Unmarshal(evt.Meta, &usage); err != nil {
		t.Fatalf("unmarshal emitted usage: %v", err)
	}
	if usage.TotalCostUSD != 17.5 {
		t.Fatalf("totalCostUsd: got %f, want 17.5", usage.TotalCostUSD)
	}
}

func TestTokenUsageLeavesUnknownModelUnchanged(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		Title:         "Unknown Model",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		Model:         "unknown-model",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	original := provider.TokenUsage{InputTokens: 123, OutputTokens: 456}
	meta, err := json.Marshal(original)
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

	evt := emittedProviderEvent(t, emissions)
	var usage provider.TokenUsage
	if err := json.Unmarshal(evt.Meta, &usage); err != nil {
		t.Fatalf("unmarshal emitted usage: %v", err)
	}
	if usage.TotalCostUSD != 0 {
		t.Fatalf("totalCostUsd: got %f, want 0", usage.TotalCostUSD)
	}
	if usage.InputTokens != original.InputTokens || usage.OutputTokens != original.OutputTokens {
		t.Fatalf("usage changed unexpectedly: got %+v, want %+v", usage, original)
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

func TestEventInitEmitsAndUpdatesSessionRef(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

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

	// Should emit.
	if len(*emissions) != 1 {
		t.Fatalf("expected 1 emission, got %d", len(*emissions))
	}

	// Should update session ref.
	thr, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thr.SessionRef != "session-abc" {
		t.Errorf("session ref: got %q, want %q", thr.SessionRef, "session-abc")
	}
}

func TestTextDeltaAccumulation(t *testing.T) {
	router, _, _ := newTestRouter(t)

	for _, text := range []string{"hello ", "world", "!"} {
		evt := provider.ProviderEvent{
			Kind:      provider.EventTextDelta,
			ThreadID:  "t1",
			Content:   text,
			Timestamp: time.Now(),
		}
		router.Handle(evt)
	}

	// Check accumulator state.
	acc, ok := router.textAccumulators["t1"]
	if !ok {
		t.Fatal("expected text accumulator for t1")
	}
	if acc.String() != "hello world!" {
		t.Errorf("accumulated text: got %q, want %q", acc.String(), "hello world!")
	}
}

func emittedProviderEvent(t *testing.T, emissions *[]emitted) provider.ProviderEvent {
	t.Helper()
	if len(*emissions) != 1 {
		t.Fatalf("expected 1 emission, got %d", len(*emissions))
	}
	if (*emissions)[0].eventName != "provider:event" {
		t.Fatalf("eventName: got %q, want provider:event", (*emissions)[0].eventName)
	}

	evt, ok := (*emissions)[0].data.(provider.ProviderEvent)
	if !ok {
		t.Fatalf("emitted data type = %T, want provider.ProviderEvent", (*emissions)[0].data)
	}
	return evt
}

func TestBackgroundDeltaNotEmitted(t *testing.T) {
	router, _, emissions := newTestRouter(t)

	evt := provider.ProviderEvent{
		Kind:      provider.EventBackgroundDelta,
		ThreadID:  "t1",
		Content:   "background output",
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(*emissions) != 0 {
		t.Errorf("expected 0 emissions for background delta, got %d", len(*emissions))
	}
}

func TestDiffPersistsHeavy(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	patch := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n foo\n+bar\n"
	evt := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   patch,
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Should emit provider:meta (not provider:event).
	if len(*emissions) != 1 {
		t.Fatalf("expected 1 emission, got %d", len(*emissions))
	}
	if (*emissions)[0].eventName != "provider:meta" {
		t.Errorf("eventName: got %q, want %q", (*emissions)[0].eventName, "provider:meta")
	}

	// Should persist payload.
	metas, err := st.ListPayloadMetas("t1")
	if err != nil {
		t.Fatalf("list payload metas: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 payload meta, got %d", len(metas))
	}
	if metas[0].Kind != "diff" {
		t.Errorf("payload kind: got %q, want %q", metas[0].Kind, "diff")
	}

	// Should persist item.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "diff" {
		t.Errorf("item kind: got %q, want %q", items[0].Kind, "diff")
	}
}

func TestDiffReplaceUpsertsExistingPayload(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	first := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n",
		Timestamp: time.Now(),
	}
	if err := router.Handle(first); err != nil {
		t.Fatalf("handle first diff: %v", err)
	}

	second := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n-old\n+new\n+newer\n",
		Replace:   true,
		Timestamp: time.Now(),
	}
	if err := router.Handle(second); err != nil {
		t.Fatalf("handle replacement diff: %v", err)
	}

	metas, err := st.ListPayloadMetas("t1")
	if err != nil {
		t.Fatalf("list payload metas: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 payload meta after replacement, got %d", len(metas))
	}

	data, err := st.GetPayloadData(metas[0].ID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if !strings.Contains(string(data), "+newer") {
		t.Fatalf("expected replacement diff content, got %q", string(data))
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 diff item after replacement, got %d", len(items))
	}
	if len(*emissions) != 2 {
		t.Fatalf("expected 2 meta emissions, got %d", len(*emissions))
	}
}

func TestDiffWithoutReplaceAppendsPayloads(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	first := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   "first diff",
		Timestamp: time.Now(),
	}
	second := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   "second diff",
		Timestamp: time.Now(),
	}

	if err := router.Handle(first); err != nil {
		t.Fatalf("handle first diff: %v", err)
	}
	if err := router.Handle(second); err != nil {
		t.Fatalf("handle second diff: %v", err)
	}

	metas, err := st.ListPayloadMetas("t1")
	if err != nil {
		t.Fatalf("list payload metas: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected 2 payload metas without replace, got %d", len(metas))
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 diff items without replace, got %d", len(items))
	}
}

func TestCommandOutputPersistsHeavy(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	cmdMeta, _ := json.Marshal(map[string]any{"command": "go build", "exitCode": 0})
	evt := provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  "t1",
		Content:   "building...\nok",
		Meta:      cmdMeta,
		Timestamp: time.Now(),
	}

	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(*emissions) != 1 {
		t.Fatalf("expected 1 emission, got %d", len(*emissions))
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "command_execution" {
		t.Errorf("item kind: got %q, want %q", items[0].Kind, "command_execution")
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
	if len(items) != 0 {
		t.Fatalf("expected no persisted items before turn complete, got %d", len(items))
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
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

	metas, err := st.ListPayloadMetas("t1")
	if err != nil {
		t.Fatalf("list payload metas: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 payload meta after turn complete, got %d", len(metas))
	}

	data, err := st.GetPayloadData(metas[0].ID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if string(data) != "Need more analysis" {
		t.Fatalf("expected accumulated reasoning, got %q", string(data))
	}

	if acc := router.reasoningAccumulators["t1"]; acc.Len() != 0 {
		t.Fatalf("expected reasoning accumulator to be cleared, got %q", acc.String())
	}

	if len(*emissions) != 2 {
		t.Fatalf("expected meta + turn complete emissions, got %d", len(*emissions))
	}
	if (*emissions)[0].eventName != "provider:meta" || (*emissions)[1].eventName != "provider:event" {
		t.Fatalf("unexpected emissions order: %+v", *emissions)
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
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	})

	// Should have persisted a text item.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "text" {
		t.Errorf("item kind: got %q, want %q", items[0].Kind, "text")
	}
	if items[0].Role != "assistant" {
		t.Errorf("item role: got %q, want %q", items[0].Role, "assistant")
	}
	if items[0].Summary != "Hello world!" {
		t.Errorf("item summary: got %q, want %q", items[0].Summary, "Hello world!")
	}

	// Accumulator should be cleared.
	if acc := router.textAccumulators["t1"]; acc.Len() != 0 {
		t.Errorf("accumulator not cleared, has %q", acc.String())
	}

	// Should have emitted 3 events: 2 text deltas + 1 turn complete.
	if len(*emissions) != 3 {
		t.Errorf("expected 3 emissions, got %d", len(*emissions))
	}
}

func TestTurnCompleteWithoutAccumulatedText(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	})

	// No text accumulated, so no item should be persisted.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}

	// Should still emit turn complete.
	if len(*emissions) != 1 {
		t.Errorf("expected 1 emission, got %d", len(*emissions))
	}
}

func TestTurnCompleteGeneratesClaudeThreadTitleFromFirstUserMessage(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
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
		Kind:      "text",
		Role:      "user",
		Summary:   "Fix flaky reconnect logic after sleep resumes. It breaks after laptop wake.\nExtra detail.",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert user item: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle turn complete: %v", err)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.Title != "Fix flaky reconnect logic after sleep resumes" {
		t.Fatalf("thread title: got %q", thread.Title)
	}

	if len(*emissions) != 2 {
		t.Fatalf("expected rename + turn complete emissions, got %d", len(*emissions))
	}
	rename, ok := (*emissions)[0].data.(provider.ProviderEvent)
	if !ok {
		t.Fatalf("rename emission type = %T", (*emissions)[0].data)
	}
	if rename.Kind != provider.EventThreadRenamed {
		t.Fatalf("rename kind: got %q, want %q", rename.Kind, provider.EventThreadRenamed)
	}
	if rename.Content != thread.Title {
		t.Fatalf("rename content: got %q, want %q", rename.Content, thread.Title)
	}
}

func TestTurnCompleteDoesNotOverwriteCustomTitle(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            "t1",
		Title:         "Custom Title",
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
		Kind:      "text",
		Role:      "user",
		Summary:   "This should not replace the custom title.",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert user item: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle turn complete: %v", err)
	}

	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.Title != "Custom Title" {
		t.Fatalf("thread title: got %q, want %q", thread.Title, "Custom Title")
	}
	if len(*emissions) != 1 {
		t.Fatalf("expected only turn complete emission, got %d", len(*emissions))
	}
}

func TestBackgroundCompletePersists(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	evt := provider.ProviderEvent{
		Kind:      provider.EventBackgroundComplete,
		ThreadID:  "t1",
		Content:   "background task completed output",
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
	if items[0].Kind != "background_done" {
		t.Errorf("item kind: got %q, want %q", items[0].Kind, "background_done")
	}
}

// -- Error propagation tests --

func TestPersistHeavyReturnsErrorOnClosedStore(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Close the store to force insertion failures.
	st.Close()

	evt := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   "+added line",
		Timestamp: time.Now(),
	}

	err := router.Handle(evt)
	if err == nil {
		t.Fatal("expected error from Handle when store is closed")
	}

	// Meta should still be emitted even when persistence fails.
	if len(*emissions) != 1 {
		t.Fatalf("expected 1 emission (meta), got %d", len(*emissions))
	}
	if (*emissions)[0].eventName != "provider:meta" {
		t.Errorf("eventName: got %q, want %q", (*emissions)[0].eventName, "provider:meta")
	}
}

func TestTurnCompleteReturnsErrorOnClosedStore(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

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
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	})

	if err == nil {
		t.Fatal("expected error from Handle when store is closed and text accumulated")
	}

	// Turn complete should still be emitted even when persistence fails.
	found := false
	for _, em := range *emissions {
		if em.eventName == "provider:event" {
			if evt, ok := em.data.(provider.ProviderEvent); ok && evt.Kind == provider.EventTurnComplete {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected turn_complete event to be emitted even on persistence failure")
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
