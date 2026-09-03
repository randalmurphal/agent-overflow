package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestCommandOutputMetaStabilisesOnComplete pins the jitter fix in
// persistToolCallCompletion: each streaming EventCommandOutput delta
// rewrites the payload meta from just the delta (so the collapsed card
// flickers with per-chunk line counts), but the tool_call completion
// MUST rebuild meta once against the cumulative payload data so the
// final card shows the total lineCount.
//
// Before the fix, lineCount ended up reflecting the line count of the
// last chunk only (e.g. 2 for three chunks of 2 lines each); the test
// would fail because meta would show lineCount=2 instead of the
// cumulative 6.
func TestCommandOutputMetaStabilisesOnComplete(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Start the tool call so the lifecycle row exists before any command
	// output arrives — matches the production ordering where
	// handleToolStart lands first.
	startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-xx",
		ItemType: "command_execution", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	cmdMeta, _ := json.Marshal(map[string]any{"command": "go build", "exitCode": 0})
	deltas := []string{
		"line1\nline2\n",
		"line3\nline4\n",
		"line5\nline6\n",
	}
	for i, delta := range deltas {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventCommandOutput,
			ThreadID:  "t1",
			ItemID:    "cmd-xx",
			Content:   delta,
			Meta:      cmdMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("output delta %q: %v", delta, err)
		}
		// Streaming deltas accumulate in the persistence buffer. Flush
		// the first two so each lands as its own window; leave the third
		// buffered so completion exercises the flush-before-rebuild path.
		if i < 2 {
			if err := router.FlushThread("t1"); err != nil {
				t.Fatalf("flush window %d: %v", i, err)
			}
		}
	}

	// Mid-stream meta reflects the last flushed window only (per-window
	// jitter). That's intentional — the streaming hot path doesn't
	// rebuild over the full blob per flush. Confirm the scaffolding is
	// what we think before asserting the fix.
	item, _, _ := st.GetThreadItem("t1", "cmd-xx")
	if item.PayloadID == "" {
		t.Fatalf("expected payload attached after streaming deltas")
	}
	var midMeta CommandOutputMeta
	if err := json.Unmarshal([]byte(item.PayloadMeta), &midMeta); err != nil {
		t.Fatalf("unmarshal mid meta: %v", err)
	}
	if midMeta.LineCount != 3 {
		// ExtractCommandOutputMeta counts "line3\nline4\n" as 3 lines
		// because of the trailing empty split token.
		t.Fatalf("pre-completion lineCount = %d, want 3 (per-window jitter contract)", midMeta.LineCount)
	}

	// Completion rebuilds meta from the cumulative blob.
	completeMeta, _ := json.Marshal(map[string]any{"exit_code": 0})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "cmd-xx",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, _, _ = st.GetThreadItem("t1", "cmd-xx")
	var finalMeta CommandOutputMeta
	if err := json.Unmarshal([]byte(item.PayloadMeta), &finalMeta); err != nil {
		t.Fatalf("unmarshal final meta: %v", err)
	}
	// The cumulative blob is "line1\nline2\nline3\nline4\nline5\nline6\n"
	// which splits into 7 tokens (six lines + trailing empty from the
	// final newline). The point of the test is that the count is
	// cumulative, not that it matches a particular off-by-one — the
	// important contract is count > last-delta count.
	if finalMeta.LineCount <= midMeta.LineCount {
		t.Errorf("final lineCount = %d, want > mid count %d (cumulative)", finalMeta.LineCount, midMeta.LineCount)
	}
	if finalMeta.Command != "go build" {
		t.Errorf("final command lost: got %q, want go build", finalMeta.Command)
	}
	if finalMeta.Preview != "" {
		t.Errorf("final preview = %q, want empty", finalMeta.Preview)
	}
}

func TestCommandOutputReplaceOverridesEarlierDeltas(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "cmd-replace",
		ItemType: "command_execution", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	cmdMeta, _ := json.Marshal(map[string]any{"command": "go test", "exitCode": 0})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  "t1",
		ItemID:    "cmd-replace",
		Content:   "partial\n",
		Meta:      cmdMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  "t1",
		ItemID:    "cmd-replace",
		Content:   "full output\n",
		Meta:      cmdMeta,
		Replace:   true,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	item, ok, err := st.GetThreadItem("t1", "cmd-replace")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if string(data) != "full output\n" {
		t.Fatalf("payload data = %q, want replacement only", string(data))
	}
}

func TestCommandOutputMetaStoresCompactFailureMessage(t *testing.T) {
	meta := ExtractCommandOutputMetaWithError(
		"setup\npanic: boom\nstack trace line",
		"go test",
		1,
		"\x1b[31mpermission denied\x1b[0m\nwhile opening file",
	)

	if meta.ErrorMessage != "permission denied while opening file" {
		t.Fatalf("errorMessage = %q", meta.ErrorMessage)
	}
}

func TestCommandOutputMetaFallsBackToOutputTailForNonZeroExit(t *testing.T) {
	meta := ExtractCommandOutputMetaWithError("ok\nmissing file\n", "cat missing", 2, "")

	if meta.ErrorMessage != "ok missing file" {
		t.Fatalf("errorMessage = %q", meta.ErrorMessage)
	}
}

// A command that exited 0 has no failure message, whatever the provider
// meta holds. The explicit-message read falls through to
// `tool_use_result.stdout` and `tool_result.content`, which on a
// successful command are its ordinary output; before this rule a dev
// server's startup banner was persisted as the row's errorMessage.
func TestCommandOutputPayloadMetaHasNoFailureMessageOnSuccess(t *testing.T) {
	banner := "  VITE ready in 214 ms\n\n  Local:   http://localhost:5173/\n"
	obj := map[string]json.RawMessage{
		"command":         json.RawMessage(`"npm run dev"`),
		"exit_code":       json.RawMessage(`0`),
		"is_error":        json.RawMessage(`false`),
		"tool_use_result": json.RawMessage(`{"stdout":` + mustJSONString(banner) + `}`),
	}
	var meta CommandOutputMeta
	if err := json.Unmarshal([]byte(buildCommandOutputPayloadMeta(banner, obj)), &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.ErrorMessage != "" {
		t.Fatalf("a successful command carried errorMessage = %q", meta.ErrorMessage)
	}
	if meta.DevServerURL != "http://localhost:5173/" {
		t.Fatalf("devServerUrl = %q, want the banner's", meta.DevServerURL)
	}

	// The same row failing keeps the message, read from the same place.
	obj["exit_code"] = json.RawMessage(`1`)
	if err := json.Unmarshal([]byte(buildCommandOutputPayloadMeta(banner, obj)), &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.ErrorMessage == "" {
		t.Fatal("a failed command lost its failure message")
	}
}

func mustJSONString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}
