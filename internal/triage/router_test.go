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
		provider.EventInit:              true,
		provider.EventTextDelta:         true,
		provider.EventToolStart:         true,
		provider.EventToolComplete:      true,
		provider.EventTurnStart:         true,
		provider.EventTurnComplete:      true,
		provider.EventApprovalRequest:   true,
		provider.EventApprovalResolved:  true,
		provider.EventSessionStatus:     true,
		provider.EventTokenUsage:        true,
		provider.EventError:             true,
		provider.EventCompactBoundary:   true,
		provider.EventRateLimits:        true,
		provider.EventModelRerouted:     true,
		provider.EventThreadRenamed:     true,
		provider.EventContentBlockStart: true,
		provider.EventContentBlockStop:  true,
		provider.EventDiff:              true,
		provider.EventCommandOutput:     true,
		provider.EventThinking:          true,
		provider.EventProposedPlan:      true,
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

	upserts := filterEmissions(*emissions, "provider:item_upsert")
	if len(upserts) != 1 {
		t.Fatalf("expected 1 provider:item_upsert emission, got %d", len(upserts))
	}
	item, ok := upserts[0].data.(store.Item)
	if !ok {
		t.Fatalf("item upsert type = %T, want store.Item", upserts[0].data)
	}
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
	// compact-boundary produces an item_upsert + usage reset, and
	// rate-limits now folds onto provider:usage per the chat-rewrite
	// spec (Channels section).
	if got := len(filterEmissions(*emissions, "provider:event")); got != 0 {
		t.Fatalf("expected zero provider:event emissions, got %d (%+v)", got, *emissions)
	}
	// Compact boundary emits a `reset` usage; rate-limits emits a
	// `rate_limits` usage. Assert both are present and ordered so the
	// discriminator is verified rather than the raw count alone.
	usageEmits := filterEmissions(*emissions, "provider:usage")
	if len(usageEmits) != 2 {
		t.Fatalf("expected 2 provider:usage emissions (compact reset + rate_limits), got %+v", *emissions)
	}
	if got := usageEmits[0].data.(provider.UsageEvent).Action; got != "reset" {
		t.Fatalf("compact-boundary usage action = %q, want %q", got, "reset")
	}
	if got := usageEmits[1].data.(provider.UsageEvent).Action; got != "rate_limits" {
		t.Fatalf("rate-limits usage action = %q, want %q", got, "rate_limits")
	}
	if len(filterEmissions(*emissions, "provider:item_upsert")) != 1 {
		t.Fatalf("expected compact boundary item upsert, got %+v", *emissions)
	}
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

// TestSessionStatusRoutesPersistentKinds covers the persistent → provider:status
// path for EventSessionStatus. A rate-limit retry maps to
// rate_limited_retrying and the banner carries the thread's provider so a
// multi-provider UI can scope the banner correctly.
func TestSessionStatusRoutesPersistentKinds(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	meta, _ := json.Marshal(map[string]any{
		"error":        "rate_limit",
		"error_status": 429,
	})
	evt := provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  "t1",
		Content:   "retrying",
		Meta:      meta,
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle session-status: %v", err)
	}

	statusEmits := filterEmissions(*emissions, "provider:status")
	if len(statusEmits) != 1 {
		t.Fatalf("expected 1 provider:status emission, got %+v", *emissions)
	}
	payload, ok := statusEmits[0].data.(provider.ProviderStatusEvent)
	if !ok {
		t.Fatalf("provider:status payload type = %T, want provider.ProviderStatusEvent", statusEmits[0].data)
	}
	if payload.Kind != provider.ProviderStatusRateLimitedRetrying {
		t.Fatalf("kind = %q, want %q", payload.Kind, provider.ProviderStatusRateLimitedRetrying)
	}
	if payload.Message != "rate_limit" {
		t.Fatalf("message = %q, want %q", payload.Message, "rate_limit")
	}
	if payload.Provider != "claude" {
		t.Fatalf("provider = %q, want claude (from seeded thread)", payload.Provider)
	}
	if payload.ThreadID != "t1" {
		t.Fatalf("threadID = %q, want t1", payload.ThreadID)
	}
	if len(filterEmissions(*emissions, "provider:event")) != 0 {
		t.Fatalf("expected no provider:event passthrough for persistent status, got %+v", *emissions)
	}
}

// TestSessionStatusPrecisePerReason covers the kind-precision contract of
// handleSessionStatus: when a retry event carries an upstream reason in
// Meta, the banner Kind matches the reason (rate-limit, unauth, or
// transient_retry as a catch-all) and the Message carries the raw reason
// so the user can tell rate-limit from auth-failure. Empty Meta falls
// through to transient_retry with a generic "Retrying..." message.
func TestSessionStatusPrecisePerReason(t *testing.T) {
	cases := []struct {
		name        string
		meta        map[string]any
		wantKind    provider.ProviderStatusEventKind
		wantMessage string
	}{
		{
			name:        "claude_rate_limit",
			meta:        map[string]any{"error": "rate_limit", "error_status": 429},
			wantKind:    provider.ProviderStatusRateLimitedRetrying,
			wantMessage: "rate_limit",
		},
		{
			name:        "claude_auth_failed",
			meta:        map[string]any{"error": "authentication_failed", "error_status": 401},
			wantKind:    provider.ProviderStatusUnauthenticated,
			wantMessage: "authentication_failed",
		},
		{
			name:        "claude_server_error",
			meta:        map[string]any{"error": "server_error", "error_status": 502},
			wantKind:    provider.ProviderStatusTransientRetry,
			wantMessage: "server_error",
		},
		{
			name:        "claude_billing_error",
			meta:        map[string]any{"error": "billing_error"},
			wantKind:    provider.ProviderStatusTransientRetry,
			wantMessage: "billing_error",
		},
		{
			name:        "claude_max_output_tokens",
			meta:        map[string]any{"error": "max_output_tokens"},
			wantKind:    provider.ProviderStatusTransientRetry,
			wantMessage: "max_output_tokens",
		},
		{
			name: "codex_nested_rate_limit",
			meta: map[string]any{
				"willRetry": true,
				"error":     map[string]any{"message": "Rate limit exceeded, retry after 30s"},
			},
			wantKind:    provider.ProviderStatusRateLimitedRetrying,
			wantMessage: "Rate limit exceeded, retry after 30s",
		},
		{
			name: "codex_nested_unknown",
			meta: map[string]any{
				"willRetry": true,
				"error":     map[string]any{"message": "Reconnecting... 2/5"},
			},
			wantKind:    provider.ProviderStatusTransientRetry,
			wantMessage: "Reconnecting... 2/5",
		},
		{
			name:        "empty_meta",
			meta:        nil,
			wantKind:    provider.ProviderStatusTransientRetry,
			wantMessage: "Retrying...",
		},
		// Negative cases — substring matching used to misclassify these.
		// A Codex rate-limit message that quotes "401 requests/minute" must
		// still land on rate_limited_retrying, not unauthenticated. The
		// boundary-aware status-code matcher keeps "401" from winning here
		// because it's glued to a digit run; the phrase "rate limit" wins
		// via the rate-limit branch.
		{
			name: "codex_rate_limit_mentions_401_requests",
			meta: map[string]any{
				"willRetry": true,
				"error":     map[string]any{"message": "Rate limit exceeded: 401 requests/minute"},
			},
			wantKind:    provider.ProviderStatusRateLimitedRetrying,
			wantMessage: "Rate limit exceeded: 401 requests/minute",
		},
		{
			// Claude's closed enum takes precedence over stray substrings.
			// If the same "rate_limit" enum arrives with a confusing
			// narrative message, we still land on rate_limited_retrying.
			name: "claude_rate_limit_exact_wins_over_text",
			meta: map[string]any{
				"error":         "rate_limit",
				"error_message": "Your 401 auth token expired — wait and retry.",
			},
			wantKind:    provider.ProviderStatusRateLimitedRetrying,
			wantMessage: "rate_limit",
		},
		{
			// 1401 must NOT match 401 as a status code. Free-form message
			// with no phrase signals falls through to transient retry.
			name: "codex_digit_glued_to_status_code_falls_through",
			meta: map[string]any{
				"willRetry": true,
				"error":     map[string]any{"message": "Process 1401 restarted"},
			},
			wantKind:    provider.ProviderStatusTransientRetry,
			wantMessage: "Process 1401 restarted",
		},
		{
			// "authorization" alone (no "unauthorized", no 401/403) is NOT
			// an auth error. We used to match "auth" as a prefix and flip
			// to unauthenticated incorrectly — this guards the fix.
			name: "message_mentions_authorization_header_only",
			meta: map[string]any{
				"willRetry": true,
				"error":     map[string]any{"message": "Refreshing authorization header"},
			},
			wantKind:    provider.ProviderStatusTransientRetry,
			wantMessage: "Refreshing authorization header",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")

			var meta json.RawMessage
			if tc.meta != nil {
				m, err := json.Marshal(tc.meta)
				if err != nil {
					t.Fatalf("marshal meta: %v", err)
				}
				meta = m
			}

			evt := provider.ProviderEvent{
				Kind:      provider.EventSessionStatus,
				ThreadID:  "t1",
				Content:   "retrying",
				Meta:      meta,
				Timestamp: time.Now(),
			}
			if err := router.Handle(evt); err != nil {
				t.Fatalf("handle session-status: %v", err)
			}

			statusEmits := filterEmissions(*emissions, "provider:status")
			if len(statusEmits) != 1 {
				t.Fatalf("expected 1 provider:status emission, got %+v", *emissions)
			}
			payload, ok := statusEmits[0].data.(provider.ProviderStatusEvent)
			if !ok {
				t.Fatalf("payload type = %T, want ProviderStatusEvent", statusEmits[0].data)
			}
			if payload.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", payload.Kind, tc.wantKind)
			}
			if payload.Message != tc.wantMessage {
				t.Fatalf("message = %q, want %q", payload.Message, tc.wantMessage)
			}
		})
	}
}

// TestSessionStatusDropsTransientKinds verifies that transient lifecycle
// signals ("disconnected", "session_state_changed", "error") don't reach
// the banner channel — they're handled by working-indicator + EventError
// flows elsewhere.
func TestSessionStatusDropsTransientKinds(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	transient := []string{"disconnected", "session_state_changed", "error"}
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
}

func TestTokenUsageEmitsContextMeterUpdate(t *testing.T) {
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

	// Provider-side cost pricing is the adapter's job. Triage trusts the
	// incoming meta verbatim and emits a provider:usage update.
	meta, err := json.Marshal(provider.TokenUsage{
		InputTokens:  2_000_000,
		OutputTokens: 1_000_000,
		TotalCostUSD: 5.25, // pre-priced by provider adapter
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
	if usage.UsedTokens != 3_000_000 {
		t.Fatalf("usedTokens: got %d, want 3000000", usage.UsedTokens)
	}
	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if !strings.Contains(thread.LastTokenUsage, "\"usedTokens\":3000000") {
		t.Fatalf("last token usage not persisted: %q", thread.LastTokenUsage)
	}
}

func TestTokenUsageEmitsWithoutProviderCost(t *testing.T) {
	// When the provider adapter didn't attach TotalCostUSD (empty or
	// unknown model), triage should still emit the context-meter update.
	// Cost is nice-to-have; the meter must fire regardless.
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

	usageEvents := filterEmissions(*emissions, "provider:usage")
	if len(usageEvents) != 1 {
		t.Fatalf("expected 1 provider:usage emission, got %+v", *emissions)
	}
	usage, ok := usageEvents[0].data.(provider.UsageEvent)
	if !ok {
		t.Fatalf("usage emission type = %T, want provider.UsageEvent", usageEvents[0].data)
	}
	if usage.UsedTokens != 579 {
		t.Fatalf("usedTokens: got %d, want 579", usage.UsedTokens)
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

// filterEmissions returns the subset of emissions on the given channel
// name. Tests that target a specific Wails channel use this so the
// presence of unrelated channels (provider:item_upsert, provider:meta,
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

	upserts := filterEmissions(*emissions, "provider:item_upsert")
	if len(upserts) == 0 {
		t.Fatal("expected at least one provider:item_upsert emission")
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
	if len(filterEmissions(*emissions, "provider:item_upsert")) < 2 {
		t.Fatalf("expected at least 2 item upserts, got %d", len(filterEmissions(*emissions, "provider:item_upsert")))
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

	upserts := filterEmissions(*emissions, "provider:item_upsert")
	if len(upserts) == 0 {
		t.Fatal("expected at least one provider:item_upsert emission")
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

	if len(filterEmissions(*emissions, "provider:item_upsert")) == 0 {
		t.Fatalf("expected provider:item_upsert emission, got %+v", *emissions)
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
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
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
	if items[0].Summary != "Need more analysis" {
		t.Fatalf("expected accumulated reasoning, got %q", items[0].Summary)
	}
	if items[0].Status != "completed" {
		t.Fatalf("status after turn complete: got %q, want completed", items[0].Status)
	}

	if len(filterEmissions(*emissions, "provider:item_upsert")) < 4 {
		t.Fatalf("expected streaming upserts plus settle upsert, got %+v", *emissions)
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

	upserts := filterEmissions(*emissions, "provider:item_upsert")
	if len(upserts) != 3 {
		t.Errorf("expected 3 item upserts, got %d", len(upserts))
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

	// Router no longer fans every event out on provider:event. An empty
	// turn produces no item upserts and no typed emissions either.
	if len(*emissions) != 0 {
		t.Errorf("expected 0 emissions for empty turn, got %d: %+v", len(*emissions), *emissions)
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

	upserts := filterEmissions(*emissions, "provider:item_upsert")
	if len(upserts) != 1 {
		t.Fatalf("expected 1 provider:item_upsert emission, got %+v", *emissions)
	}
}

// TestFatalErrorOrderingMatchesSpec pins the ordering contract on a
// fatal EventError, per chat-rewrite §"Live provider-crash flip":
//
//   1. flip every streaming/running item in the turn to errored
//   2. persist the error row
//   3. drain any queued completions as errored
//   4. synthesize TurnComplete{truncated:true} when no wire
//      TurnComplete is expected
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
	completeMeta, _ := json.Marshal(map[string]any{
		"is_background": true,
		"exit_code":     0,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bg-1",
		Meta: completeMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg complete: %v", err)
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
	upserts := filterEmissions(*emissions, "provider:item_upsert")
	var sequence []string
	for _, e := range upserts {
		item, ok := e.data.(store.Item)
		if !ok {
			continue
		}
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
// EventTurnComplete{truncated:true} so the frontend working
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
	// TurnComplete. The synthetic event has meta.synthetic=true so we
	// can distinguish it from a real wire TurnComplete.
	close(observed)
	var gotSynthesized bool
	for evt := range observed {
		if evt.Kind != provider.EventTurnComplete {
			continue
		}
		var meta map[string]any
		if len(evt.Meta) > 0 {
			_ = json.Unmarshal(evt.Meta, &meta)
		}
		if synth, _ := meta["synthetic"].(bool); synth {
			gotSynthesized = true
			// And meta.truncated must be true so handleTurnComplete
			// takes the truncated branch (flip items, drain as errored).
			if truncated, _ := meta["truncated"].(bool); !truncated {
				t.Fatalf("synthesized TurnComplete lacks truncated:true, meta=%+v", meta)
			}
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
	if !strings.HasSuffix(items[0].Summary, " — interrupted") {
		t.Fatalf("text summary missing interrupted suffix: %q", items[0].Summary)
	}
	if items[1].Kind != "error" {
		t.Fatalf("second item kind: got %q, want error", items[1].Kind)
	}

	upserts := filterEmissions(*emissions, "provider:item_upsert")
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
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
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
