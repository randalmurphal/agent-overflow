package sessionimport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/importir"
)

// Fixtures are hand-written minimal rows, never copies of a real
// transcript and never a read of a real ~/.claude. Every test writes its
// own tree under t.TempDir().

// writeJSONL writes lines (each a map or a raw string) as a JSONL file,
// creating parent directories.
func writeJSONL(t *testing.T, path string, lines ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	var body []byte
	for _, line := range lines {
		switch typed := line.(type) {
		case string:
			body = append(body, typed...)
		default:
			encoded, err := json.Marshal(typed)
			if err != nil {
				t.Fatalf("marshal fixture line: %v", err)
			}
			body = append(body, encoded...)
		}
		body = append(body, '\n')
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type rowOpt func(map[string]any)

func with(key string, value any) rowOpt {
	return func(m map[string]any) { m[key] = value }
}

// userRow builds a `user` transcript row with plain-string content.
func userRow(uuid, parent, text, ts string, opts ...rowOpt) map[string]any {
	row := map[string]any{
		"type":        "user",
		"uuid":        uuid,
		"parentUuid":  nilIfEmpty(parent),
		"isSidechain": false,
		"timestamp":   ts,
		"cwd":         "/repo",
		"gitBranch":   "main",
		"message":     map[string]any{"role": "user", "content": text},
	}
	for _, opt := range opts {
		opt(row)
	}
	return row
}

// userBlocksRow builds a `user` row whose content is a block array.
func userBlocksRow(uuid, parent string, blocks []any, ts string, opts ...rowOpt) map[string]any {
	row := userRow(uuid, parent, "", ts, opts...)
	row["message"] = map[string]any{"role": "user", "content": blocks}
	return row
}

// toolResultRow builds the `user` row that closes a tool call.
func toolResultRow(uuid, parent, toolUseID, content, ts string, opts ...rowOpt) map[string]any {
	return userBlocksRow(uuid, parent, []any{
		map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": content},
	}, ts, opts...)
}

// assistantRow builds an `assistant` row carrying the given content blocks.
func assistantRow(uuid, parent, messageID string, blocks []any, ts string, opts ...rowOpt) map[string]any {
	row := map[string]any{
		"type":        "assistant",
		"uuid":        uuid,
		"parentUuid":  nilIfEmpty(parent),
		"isSidechain": false,
		"timestamp":   ts,
		"cwd":         "/repo",
		"message": map[string]any{
			"role":    "assistant",
			"id":      messageID,
			"model":   "claude-test-1",
			"content": blocks,
		},
	}
	for _, opt := range opts {
		opt(row)
	}
	return row
}

func textBlock(text string) any {
	return map[string]any{"type": "text", "text": text}
}

func toolUseBlock(id, name string, input map[string]any) any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// decodeRows round-trips fixture rows through JSON so tests see exactly
// what a real read produces (numbers as float64, absent keys absent) —
// handing Go literals straight to BuildBranches would test a shape the
// code never actually receives.
func decodeRows(t *testing.T, lines ...any) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("marshal fixture row: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("decode fixture row: %v", err)
		}
		if t, _ := decoded["type"].(string); t == "" {
			continue
		}
		out = append(out, decoded)
	}
	return out
}

// decodeBranchRows is decodeRows projected into the Row shape
// BuildBranches walks. These rows keep their Raw bodies, which is what a
// caller that already has the whole file decoded hands over; the transcript
// reader hands over skeletons instead.
func decodeBranchRows(t *testing.T, lines ...any) []Row {
	t.Helper()
	return newRows(decodeRows(t, lines...))
}

// loadBranch is the "one written transcript, one branch" shortcut: open
// the file, convert the branch, and merge the file-level warnings into the
// branch's own so a test can assert on one list. The session stays open for
// the test's lifetime because ConvertBranch reads back through it.
func loadBranch(t *testing.T, path string, index, wantBranches int) LoadedBranch {
	t.Helper()
	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	t.Cleanup(func() { _ = loaded.Close() })
	if len(loaded.Branches) != wantBranches {
		t.Fatalf("got %d branches, want %d", len(loaded.Branches), wantBranches)
	}
	branch, err := loaded.ConvertBranch(index)
	if err != nil {
		t.Fatalf("ConvertBranch(%d): %v", index, err)
	}
	branch.Warnings = append(append([]importir.Warning(nil), loaded.Warnings...), branch.Warnings...)
	return branch
}

// buildChain is the common "one fixture, one branch" shortcut.
func buildChain(t *testing.T, lines ...any) Branch {
	t.Helper()
	branches, _ := BuildBranches(decodeBranchRows(t, lines...), nil)
	if len(branches) != 1 {
		t.Fatalf("BuildBranches: got %d branches, want 1", len(branches))
	}
	return branches[0]
}
