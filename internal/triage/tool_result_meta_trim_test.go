package triage

// Router-level coverage for the completion-echo trim
// (internal/itemmeta.TrimToolResultEcho, wired through
// shapeToolItemMeta). Pure trim semantics are unit-tested in
// internal/itemmeta/trim_test.go; these tests pin that every tool
// persist path actually applies the trim and that the data the
// frontend still needs survives end to end.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func decodeMetaObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode meta object: %v\nraw: %s", err, raw)
	}
	return obj
}

// TestPersistedCompletionMetaSizeBound is the completion-side sibling
// of TestPersistedItemMetaSizeBound: a Claude completion event whose
// meta carries a megabyte-class tool_result / tool_use_result echo must
// persist a bounded items.meta, while the full result body stays
// reachable through the lazy payload row.
func TestPersistedCompletionMetaSizeBound(t *testing.T) {
	const limit = 16 * 1024
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	heavy := strings.Repeat("subagent transcript line\n", 60_000) // ~1.5 MB
	cases := []struct {
		name     string
		toolName string
		isError  bool
	}{
		{"task success", "Task", false},
		{"bash success", "Bash", false},
		{"bash failure", "Bash", true},
		{"read success", "Read", false},
	}

	for i, c := range cases {
		itemID := "item-" + strings.ReplaceAll(c.name, " ", "-")
		startMeta, _ := json.Marshal(map[string]any{
			"toolName": c.toolName,
			"input":    map[string]any{"command": "make build", "description": "build"},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventToolStart,
			ThreadID:  "t1",
			ItemID:    itemID,
			ItemType:  c.toolName,
			Meta:      startMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("[%d] handle start: %v", i, err)
		}

		completeMeta, _ := json.Marshal(map[string]any{
			"is_error": c.isError,
			"tool_result": map[string]any{
				"tool_use_id": itemID,
				"type":        "tool_result",
				"content":     []map[string]any{{"type": "text", "text": heavy}},
				"is_error":    c.isError,
			},
			"tool_use_result": map[string]any{
				"stdout": heavy,
				"stderr": "",
			},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventToolComplete,
			ThreadID:  "t1",
			ItemID:    itemID,
			ItemType:  c.toolName,
			Content:   heavy,
			Meta:      completeMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("[%d] handle complete: %v", i, err)
		}

		got, found, err := st.GetThreadItem("t1", itemID)
		if err != nil || !found {
			t.Fatalf("[%d] get item: found=%v err=%v", i, found, err)
		}
		if size := len(got.Meta); size > limit {
			t.Errorf("[%d] %s items.meta = %d bytes; cap %d", i, c.name, size, limit)
		}
		top := decodeMetaObject(t, json.RawMessage(got.Meta))
		if !c.isError {
			if _, ok := top["tool_result"]; ok {
				t.Errorf("[%d] %s kept tool_result on success", i, c.name)
			}
			if _, ok := top["tool_use_result"]; ok {
				t.Errorf("[%d] %s kept tool_use_result on success", i, c.name)
			}
		}

		// The full body must stay reachable for expand-on-demand.
		if got.PayloadID == "" {
			t.Fatalf("[%d] %s has no completion payload", i, c.name)
		}
		data, err := st.GetPayloadData(got.ThreadID, got.PayloadID)
		if err != nil {
			t.Fatalf("[%d] get payload data: %v", i, err)
		}
		if !strings.Contains(string(data), "subagent transcript line") {
			t.Errorf("[%d] %s payload lost the full result body", i, c.name)
		}
	}
}

// TestCompletionMetaKeepsAskUserQuestionEcho pins the answers echo the
// AskUserQuestionCard renders from: meta.tool_result.content survives
// completion persistence verbatim for user-input tools.
func TestCompletionMetaKeepsAskUserQuestionEcho(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "AskUserQuestion",
		"input": map[string]any{
			"questions": []map[string]any{{
				"question": "Which approach?",
				"header":   "Approach",
				"options":  []map[string]any{{"label": "A"}, {"label": "B"}},
			}},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "ask-1",
		ItemType:  "AskUserQuestion",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start: %v", err)
	}

	answersEcho := `{"answers":{"Approach":"B"}}`
	completeMeta, _ := json.Marshal(map[string]any{
		"is_error": false,
		"tool_result": map[string]any{
			"tool_use_id": "ask-1",
			"type":        "tool_result",
			"content":     answersEcho,
			"is_error":    false,
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "ask-1",
		ItemType:  "AskUserQuestion",
		Meta:      completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle complete: %v", err)
	}

	got, found, err := st.GetThreadItem("t1", "ask-1")
	if err != nil || !found {
		t.Fatalf("get item: found=%v err=%v", found, err)
	}
	var decoded struct {
		ToolResult struct {
			Content string `json:"content"`
		} `json:"tool_result"`
	}
	if err := json.Unmarshal([]byte(got.Meta), &decoded); err != nil {
		t.Fatalf("decode persisted meta: %v", err)
	}
	if decoded.ToolResult.Content != answersEcho {
		t.Errorf("answers echo lost: got %q want %q", decoded.ToolResult.Content, answersEcho)
	}
}

// TestCompletionMetaKeepsFailureExcerptEndToEnd drives a failed Bash
// completion through the router and asserts the persisted meta still
// satisfies the commandErrorForItem fallback chain: bounded
// tool_use_result.stderr with the original trailing lines.
func TestCompletionMetaKeepsFailureExcerptEndToEnd(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "make test"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "bash-fail",
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start: %v", err)
	}

	bigStderr := strings.Repeat("compiling...\n", 10_000) + "FAIL: TestThing\nexit status 1"
	completeMeta, _ := json.Marshal(map[string]any{
		"is_error":  true,
		"exit_code": 1,
		"tool_use_result": map[string]any{
			"stderr": bigStderr,
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "bash-fail",
		ItemType:  "Bash",
		Meta:      completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle complete: %v", err)
	}

	got, found, err := st.GetThreadItem("t1", "bash-fail")
	if err != nil || !found {
		t.Fatalf("get item: found=%v err=%v", found, err)
	}
	if got.Status != statusErrored {
		t.Errorf("status = %q, want errored", got.Status)
	}
	var decoded struct {
		ExitCode      int `json:"exit_code"`
		ToolUseResult struct {
			Stderr string `json:"stderr"`
		} `json:"tool_use_result"`
	}
	if err := json.Unmarshal([]byte(got.Meta), &decoded); err != nil {
		t.Fatalf("decode persisted meta: %v", err)
	}
	if decoded.ExitCode != 1 {
		t.Errorf("exit_code = %d, want 1", decoded.ExitCode)
	}
	if !strings.HasSuffix(decoded.ToolUseResult.Stderr, "FAIL: TestThing\nexit status 1") {
		t.Errorf("stderr excerpt lost the error tail: %q", decoded.ToolUseResult.Stderr)
	}
	if len(decoded.ToolUseResult.Stderr) > 2048 {
		t.Errorf("stderr excerpt = %d bytes; want ≤ 2048", len(decoded.ToolUseResult.Stderr))
	}
}
