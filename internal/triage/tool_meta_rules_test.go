package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestApplyToolMetaRule_Edit pins the chokepoint that promotes
// Edit's `old_string` / `new_string` (and the optional
// `replace_all` flag) out of items.meta into a sibling
// tool_call_input payload while leaving `file_path` inline.
func TestApplyToolMetaRule_Edit(t *testing.T) {
	const filePath = "/tmp/example.go"
	oldString := strings.Repeat("a", 200_000)
	newString := strings.Repeat("b", 200_000)
	raw := mustMarshalMeta(t, map[string]any{
		"toolName": "Edit",
		"input": map[string]any{
			"file_path":   filePath,
			"old_string":  oldString,
			"new_string":  newString,
			"replace_all": false,
		},
	})

	now := time.Now().UnixMilli()
	trimmed, payload, err := applyToolMetaRule("Edit", raw, now)
	if err != nil {
		t.Fatalf("applyToolMetaRule: %v", err)
	}
	if payload == nil {
		t.Fatalf("expected promoted payload, got nil")
	}
	if payload.Kind != payloadKindToolCallInput {
		t.Fatalf("payload kind = %q, want %q", payload.Kind, payloadKindToolCallInput)
	}
	if payload.CreatedAt != now {
		t.Fatalf("payload createdAt = %d, want %d", payload.CreatedAt, now)
	}

	// Trimmed meta keeps file_path; drops the heavy fields.
	gotInput := readNestedInput(t, trimmed)
	if gotInput["file_path"] == nil {
		t.Errorf("trimmed input missing file_path: %s", string(trimmed))
	}
	for _, dropped := range []string{"old_string", "new_string", "replace_all"} {
		if _, present := gotInput[dropped]; present {
			t.Errorf("trimmed input still has %s; expected promotion", dropped)
		}
	}
	// items.meta should be a few KB at most.
	if len(trimmed) > 8*1024 {
		t.Errorf("trimmed meta is %d bytes; expected ≤ 8 KB", len(trimmed))
	}

	// Payload data should JSON-decode to a map of just the promoted
	// keys.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(payload.Data, &decoded); err != nil {
		t.Fatalf("decode payload data: %v", err)
	}
	for _, want := range []string{"old_string", "new_string", "replace_all"} {
		if _, ok := decoded[want]; !ok {
			t.Errorf("payload missing key %q", want)
		}
	}
	if _, ok := decoded["file_path"]; ok {
		t.Errorf("payload should not carry file_path; that's inline")
	}
}

func TestApplyToolMetaRule_Write(t *testing.T) {
	content := strings.Repeat("z", 1_500_000) // 1.5 MB
	raw := mustMarshalMeta(t, map[string]any{
		"toolName": "Write",
		"input":    map[string]any{"file_path": "/tmp/big.txt", "content": content},
	})

	trimmed, payload, err := applyToolMetaRule("Write", raw, 1)
	if err != nil {
		t.Fatalf("applyToolMetaRule: %v", err)
	}
	if payload == nil {
		t.Fatalf("expected payload for Write content")
	}
	if len(trimmed) > 4*1024 {
		t.Errorf("trimmed meta is %d bytes; expected ≤ 4 KB", len(trimmed))
	}
	if len(payload.Data) < len(content) {
		t.Errorf("payload data %d bytes; expected ≥ content length %d", len(payload.Data), len(content))
	}
}

func TestApplyToolMetaRule_MultiEdit(t *testing.T) {
	edits := make([]map[string]any, 0, 50)
	for i := 0; i < 50; i++ {
		edits = append(edits, map[string]any{
			"old_string": strings.Repeat("o", 1_000),
			"new_string": strings.Repeat("n", 1_000),
		})
	}
	raw := mustMarshalMeta(t, map[string]any{
		"toolName": "MultiEdit",
		"input":    map[string]any{"file_path": "/tmp/many.go", "edits": edits},
	})

	trimmed, payload, err := applyToolMetaRule("MultiEdit", raw, 1)
	if err != nil {
		t.Fatalf("applyToolMetaRule: %v", err)
	}
	if payload == nil {
		t.Fatalf("expected payload for MultiEdit edits")
	}
	if _, present := readNestedInput(t, trimmed)["edits"]; present {
		t.Errorf("trimmed meta should not carry edits; got: %s", string(trimmed))
	}
}

// TestApplyToolMetaRule_Bash pins that bounded-input tools (Bash,
// Read, Grep, ...) pass through with their kept fields intact and
// produce no overflow payload.
func TestApplyToolMetaRule_Bash(t *testing.T) {
	raw := mustMarshalMeta(t, map[string]any{
		"toolName": "Bash",
		"input": map[string]any{
			"command":     "echo hello",
			"description": "smoke test",
			"timeout":     5000,
			"cwd":         "/tmp",
			// Heavy unrelated key that's not in KeepInput should be
			// dropped to enforce the allowlist.
			"_internal_dump": strings.Repeat("x", 100_000),
		},
	})

	trimmed, payload, err := applyToolMetaRule("Bash", raw, 1)
	if err != nil {
		t.Fatalf("applyToolMetaRule: %v", err)
	}
	if payload != nil {
		t.Errorf("Bash should not promote any fields; got payload kind %q", payload.Kind)
	}
	gotInput := readNestedInput(t, trimmed)
	for _, kept := range []string{"command", "description", "timeout", "cwd"} {
		if _, ok := gotInput[kept]; !ok {
			t.Errorf("trimmed input missing kept field %q", kept)
		}
	}
	if _, leaked := gotInput["_internal_dump"]; leaked {
		t.Errorf("Bash KeepInput allowlist should drop _internal_dump")
	}
}

func TestApplyToolMetaRule_AskUserQuestion(t *testing.T) {
	raw := mustMarshalMeta(t, map[string]any{
		"toolName": "AskUserQuestion",
		"input": map[string]any{
			"questions": []map[string]any{
				{"text": "Which?", "options": []string{"a", "b"}},
			},
		},
	})

	trimmed, payload, err := applyToolMetaRule("AskUserQuestion", raw, 1)
	if err != nil {
		t.Fatalf("applyToolMetaRule: %v", err)
	}
	if payload != nil {
		t.Fatalf("AskUserQuestion should not promote; got payload")
	}
	if _, ok := readNestedInput(t, trimmed)["questions"]; !ok {
		t.Errorf("trimmed input missing questions")
	}
}

func TestApplyToolMetaRule_UnknownToolPassesThrough(t *testing.T) {
	raw := mustMarshalMeta(t, map[string]any{
		"toolName": "TotallyMadeUpTool_v3",
		"input":    map[string]any{"alpha": 1, "beta": "two", "gamma": []int{1, 2, 3}},
	})

	trimmed, payload, err := applyToolMetaRule("TotallyMadeUpTool_v3", raw, 1)
	if err != nil {
		t.Fatalf("applyToolMetaRule: %v", err)
	}
	if payload != nil {
		t.Errorf("unknown tool should not promote")
	}
	if string(trimmed) != string(raw) {
		t.Errorf("unknown tool meta should pass through unchanged")
	}
}

func TestApplyToolMetaRule_NoInputKeyPassesThrough(t *testing.T) {
	raw := mustMarshalMeta(t, map[string]any{
		"toolName":  "Edit",
		"task_id":   "t-1",
		"is_inline": true,
	})

	trimmed, payload, err := applyToolMetaRule("Edit", raw, 1)
	if err != nil {
		t.Fatalf("applyToolMetaRule: %v", err)
	}
	if payload != nil {
		t.Errorf("rule should not fabricate a payload when input is absent")
	}
	if string(trimmed) != string(raw) {
		t.Errorf("meta without input should pass through unchanged")
	}
}

func TestApplyToolMetaRule_PreservesTopLevelMetaKeys(t *testing.T) {
	raw := mustMarshalMeta(t, map[string]any{
		"toolName":             "Edit",
		"task_id":              "task-42",
		"is_background":        true,
		"assistant_message_id": "msg-1",
		"input": map[string]any{
			"file_path":  "/x",
			"old_string": "y",
			"new_string": "z",
		},
	})

	trimmed, _, err := applyToolMetaRule("Edit", raw, 1)
	if err != nil {
		t.Fatalf("applyToolMetaRule: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		t.Fatalf("decode trimmed: %v", err)
	}
	for _, key := range []string{"toolName", "task_id", "is_background", "assistant_message_id", "input"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("trim dropped top-level meta key %q", key)
		}
	}
}

// TestPersistedItemMetaSizeBound runs synthetic launch events for
// every Claude tool the registry covers and asserts the persisted
// items.meta column stays under 8 KiB after the chokepoint runs.
// A new heavy-input tool added to the parser without a registry entry
// (or a registry entry whose KeepInput accidentally permits a
// megabyte field) trips the bound and fails the test.
func TestPersistedItemMetaSizeBound(t *testing.T) {
	const limit = 8 * 1024
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	heavy := strings.Repeat("Q", 1_500_000) // 1.5 MB
	cases := []struct {
		toolName string
		input    map[string]any
	}{
		{"Edit", map[string]any{"file_path": "/x", "old_string": heavy, "new_string": heavy}},
		{"Write", map[string]any{"file_path": "/x", "content": heavy}},
		{"MultiEdit", map[string]any{"file_path": "/x", "edits": []map[string]any{
			{"old_string": heavy, "new_string": heavy},
		}}},
		{"NotebookEdit", map[string]any{"notebook_path": "/n", "cell_id": "c", "new_source": heavy}},
		{"Bash", map[string]any{"command": "ls", "description": strings.Repeat("d", 1_000)}},
		{"Read", map[string]any{"file_path": "/r", "limit": 100}},
		{"Grep", map[string]any{"pattern": "foo", "path": "/p", "output_mode": "content"}},
		{"Task", map[string]any{"description": "summary", "prompt": strings.Repeat("p", 4_000), "subagent_type": "Explore"}},
	}

	for i, c := range cases {
		itemID := "item-" + c.toolName
		meta, err := json.Marshal(map[string]any{
			"toolName": c.toolName,
			"input":    c.input,
		})
		if err != nil {
			t.Fatalf("marshal meta for %s: %v", c.toolName, err)
		}
		evt := provider.ProviderEvent{
			Kind:      provider.EventToolStart,
			ThreadID:  "t1",
			ItemID:    itemID,
			ItemType:  c.toolName,
			Meta:      meta,
			Timestamp: time.Now(),
		}
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle %s: %v", c.toolName, err)
		}
		got, found, err := st.GetThreadItem("t1", itemID)
		if err != nil || !found {
			t.Fatalf("get %s: found=%v err=%v", itemID, found, err)
		}
		if got.Status != statusRunning {
			t.Errorf("[%d] %s status = %q, want running", i, c.toolName, got.Status)
		}
		if size := len(got.Meta); size > limit {
			t.Errorf("[%d] %s items.meta = %d bytes; cap %d", i, c.toolName, size, limit)
		}
	}
}

// TestPersistedItemMetaPromotesInputPayload pins that promotion
// actually writes the sibling payload row and links it via
// items.input_payload_id, so a future debug surface can still
// retrieve the original bytes.
func TestPersistedItemMetaPromotesInputPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	oldString := strings.Repeat("a", 100_000)
	newString := strings.Repeat("b", 100_000)
	meta, _ := json.Marshal(map[string]any{
		"toolName": "Edit",
		"input": map[string]any{
			"file_path":  "/tmp/x.go",
			"old_string": oldString,
			"new_string": newString,
		},
	})
	evt := provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "edit-1",
		ItemType:  "Edit",
		Meta:      meta,
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	item, found, err := st.GetThreadItem("t1", "edit-1")
	if err != nil || !found {
		t.Fatalf("get item: found=%v err=%v", found, err)
	}
	if item.InputPayloadID == "" {
		t.Fatalf("expected items.input_payload_id to be set")
	}

	data, err := st.GetPayloadData(item.ThreadID, item.InputPayloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if !strings.Contains(string(data), oldString) {
		t.Errorf("payload data missing old_string bytes")
	}
	if !strings.Contains(string(data), newString) {
		t.Errorf("payload data missing new_string bytes")
	}
}

// TestApplyToolMetaRule_IdempotentOnAlreadyTrimmed pins that running
// the rule twice on the same meta does not regenerate the payload
// (so a re-discovered launch / merged completion does not duplicate
// the writes).
func TestApplyToolMetaRule_IdempotentOnAlreadyTrimmed(t *testing.T) {
	raw := mustMarshalMeta(t, map[string]any{
		"toolName": "Edit",
		"input": map[string]any{
			"file_path":  "/x",
			"old_string": "o",
			"new_string": "n",
		},
	})
	trimmed1, payload1, err := applyToolMetaRule("Edit", raw, 1)
	if err != nil || payload1 == nil {
		t.Fatalf("first run: payload=%v err=%v", payload1, err)
	}
	_, payload2, err := applyToolMetaRule("Edit", trimmed1, 1)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if payload2 != nil {
		t.Errorf("second run should not re-promote (input map already trimmed); got payload %q", payload2.ID)
	}
}

func mustMarshalMeta(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	return raw
}

func readNestedInput(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal top-level meta: %v", err)
	}
	inputRaw, ok := top["input"]
	if !ok || len(inputRaw) == 0 {
		return nil
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		t.Fatalf("unmarshal nested input: %v", err)
	}
	return input
}
