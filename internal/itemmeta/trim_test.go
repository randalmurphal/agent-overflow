package itemmeta

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func decodeObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode meta object: %v\nraw: %s", err, raw)
	}
	return obj
}

func TestTrimToolResultEcho_SuccessDropsEcho(t *testing.T) {
	heavy := strings.Repeat("output line\n", 50_000) // ~600 KB
	raw := mustMarshal(t, map[string]any{
		"toolName":  "Task",
		"task_id":   "task-7",
		"is_error":  false,
		"exit_code": 0,
		"input":     map[string]any{"description": "explore", "prompt": "go"},
		"tool_result": map[string]any{
			"tool_use_id": "toolu_1",
			"type":        "tool_result",
			"content":     []map[string]any{{"type": "text", "text": heavy}},
			"is_error":    false,
		},
		"tool_use_result": map[string]any{
			"content":     []map[string]any{{"type": "text", "text": heavy}},
			"totalTokens": 123456,
		},
	})

	trimmed, changed := TrimToolResultEcho("Task", raw)
	if !changed {
		t.Fatalf("expected trim to report a change")
	}
	if len(trimmed) > 1024 {
		t.Errorf("trimmed meta = %d bytes; expected well under 1 KB", len(trimmed))
	}
	top := decodeObject(t, trimmed)
	if _, ok := top["tool_result"]; ok {
		t.Errorf("success row kept tool_result")
	}
	if _, ok := top["tool_use_result"]; ok {
		t.Errorf("success row kept tool_use_result")
	}
	for _, key := range []string{"toolName", "task_id", "is_error", "exit_code", "input"} {
		if _, ok := top[key]; !ok {
			t.Errorf("trim dropped unrelated top-level key %q", key)
		}
	}
}

func TestTrimToolResultEcho_FailureKeepsBoundedTailExcerpts(t *testing.T) {
	var stderrBuilder strings.Builder
	for i := 0; i < 2_000; i++ {
		stderrBuilder.WriteString(strings.Repeat("e", 40))
		stderrBuilder.WriteString("\n")
	}
	stderrBuilder.WriteString("final: assertion failed at line 42")
	bigStderr := stderrBuilder.String() // ~82 KB, known last line

	raw := mustMarshal(t, map[string]any{
		"toolName":  "Bash",
		"is_error":  true,
		"exit_code": 1,
		"tool_result": map[string]any{
			"tool_use_id": "toolu_2",
			"type":        "tool_result",
			"content":     []map[string]any{{"type": "text", "text": bigStderr}},
			"is_error":    true,
		},
		"tool_use_result": map[string]any{
			"stderr":      bigStderr,
			"stdout":      strings.Repeat("s", 10_000),
			"interrupted": false,
		},
	})

	trimmed, changed := TrimToolResultEcho("Bash", raw)
	if !changed {
		t.Fatalf("expected trim to report a change")
	}
	if len(trimmed) > 4*ToolResultExcerptCap {
		t.Errorf("trimmed failure meta = %d bytes; cap ~%d", len(trimmed), 4*ToolResultExcerptCap)
	}

	var decoded struct {
		ToolResult struct {
			Content string `json:"content"`
		} `json:"tool_result"`
		ToolUseResult struct {
			Stderr      string `json:"stderr"`
			Stdout      string `json:"stdout"`
			Interrupted *bool  `json:"interrupted"`
		} `json:"tool_use_result"`
		IsError  bool `json:"is_error"`
		ExitCode int  `json:"exit_code"`
	}
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		t.Fatalf("decode trimmed: %v", err)
	}
	if !decoded.IsError || decoded.ExitCode != 1 {
		t.Errorf("failure flags lost: is_error=%v exit_code=%d", decoded.IsError, decoded.ExitCode)
	}
	if decoded.ToolUseResult.Interrupted != nil {
		t.Errorf("tool_use_result kept a non-excerpt field")
	}
	for name, got := range map[string]string{
		"tool_result.content":    decoded.ToolResult.Content,
		"tool_use_result.stderr": decoded.ToolUseResult.Stderr,
	} {
		if got == "" {
			t.Errorf("%s excerpt missing", name)
			continue
		}
		if len(got) > ToolResultExcerptCap {
			t.Errorf("%s excerpt = %d bytes; cap %d", name, len(got), ToolResultExcerptCap)
		}
		if !strings.HasSuffix(got, "final: assertion failed at line 42") {
			t.Errorf("%s excerpt lost the tail; got suffix %q", name, got[max(0, len(got)-60):])
		}
		// Excerpt starts at a line boundary so "render the last N
		// lines" consumers (commandErrorForItem → compactErrorMessage)
		// see exactly the lines the full string produced.
		if idx := strings.IndexByte(got, '\n'); idx >= 0 {
			firstLine := got[:idx]
			if len(firstLine) != 40 {
				t.Errorf("%s excerpt does not start on a line boundary; first line %q", name, firstLine)
			}
		}
	}
	if decoded.ToolUseResult.Stdout == "" || len(decoded.ToolUseResult.Stdout) > ToolResultExcerptCap {
		t.Errorf("stdout excerpt = %d bytes; want non-empty ≤ %d", len(decoded.ToolUseResult.Stdout), ToolResultExcerptCap)
	}
}

// TestTrimToolResultEcho_LastTwoLinesParity pins the UI invariance
// contract: the frontend's commandErrorForItem renders the last two
// non-empty lines of tool_use_result.stderr. Those lines must be
// byte-identical before and after the trim.
func TestTrimToolResultEcho_LastTwoLinesParity(t *testing.T) {
	lastTwoLines := func(s string) []string {
		lines := []string{}
		for _, line := range strings.Split(s, "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, strings.TrimSpace(line))
			}
		}
		if len(lines) <= 2 {
			return lines
		}
		return lines[len(lines)-2:]
	}

	bigStderr := strings.Repeat("noise line that fills the buffer\n", 5_000) +
		"Error: connection refused\nat main.go:99"
	raw := mustMarshal(t, map[string]any{
		"toolName": "Bash",
		"is_error": true,
		"tool_use_result": map[string]any{
			"stderr": bigStderr,
		},
	})

	trimmed, changed := TrimToolResultEcho("Bash", raw)
	if !changed {
		t.Fatalf("expected trim to report a change")
	}
	var decoded struct {
		ToolUseResult struct {
			Stderr string `json:"stderr"`
		} `json:"tool_use_result"`
	}
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		t.Fatalf("decode trimmed: %v", err)
	}
	want := lastTwoLines(bigStderr)
	got := lastTwoLines(decoded.ToolUseResult.Stderr)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("last-two-lines parity broken:\n got %q\nwant %q", got, want)
	}
}

func TestTrimToolResultEcho_ExitCodeAloneSignalsFailure(t *testing.T) {
	raw := mustMarshal(t, map[string]any{
		"toolName":  "Bash",
		"is_error":  false,
		"exit_code": 2,
		"tool_use_result": map[string]any{
			"stderr":      "boom",
			"interrupted": false,
		},
	})
	trimmed, changed := TrimToolResultEcho("Bash", raw)
	if !changed {
		t.Fatalf("expected trim to report a change")
	}
	var decoded struct {
		ToolUseResult struct {
			Stderr string `json:"stderr"`
		} `json:"tool_use_result"`
	}
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		t.Fatalf("decode trimmed: %v", err)
	}
	if decoded.ToolUseResult.Stderr != "boom" {
		t.Errorf("non-zero exit_code should keep the stderr excerpt; got %q", decoded.ToolUseResult.Stderr)
	}
}

func TestTrimToolResultEcho_NestedIsErrorFallback(t *testing.T) {
	raw := mustMarshal(t, map[string]any{
		"toolName": "Read",
		"tool_result": map[string]any{
			"tool_use_id": "toolu_3",
			"content":     "file not found: /tmp/missing",
			"is_error":    true,
		},
	})
	trimmed, changed := TrimToolResultEcho("Read", raw)
	if !changed {
		t.Fatalf("expected trim to report a change")
	}
	var decoded struct {
		ToolResult struct {
			Content string `json:"content"`
		} `json:"tool_result"`
	}
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		t.Fatalf("decode trimmed: %v", err)
	}
	if decoded.ToolResult.Content != "file not found: /tmp/missing" {
		t.Errorf("nested is_error row lost its content excerpt; got %q", decoded.ToolResult.Content)
	}
}

func TestTrimToolResultEcho_UserInputToolsExempt(t *testing.T) {
	for _, toolName := range []string{"AskUserQuestion", "request_user_input"} {
		raw := mustMarshal(t, map[string]any{
			"toolName": toolName,
			"is_error": false,
			"tool_result": map[string]any{
				"content": `{"answers":{"Approach":"Option B"}}`,
			},
		})
		trimmed, changed := TrimToolResultEcho(toolName, raw)
		if changed {
			t.Errorf("%s: user-input echo must stay untrimmed", toolName)
		}
		if string(trimmed) != string(raw) {
			t.Errorf("%s: meta bytes changed", toolName)
		}
	}
}

func TestTrimToolResultEchoObjectMatchesByteAPI(t *testing.T) {
	raw := mustMarshal(t, map[string]any{
		"toolName":  "Bash",
		"is_error":  true,
		"exit_code": 1,
		"input":     map[string]any{"command": "make test"},
		"tool_result": map[string]any{
			"content":  strings.Repeat("result\n", 2_000) + "final result",
			"is_error": true,
		},
		"tool_use_result": map[string]any{
			"stderr": strings.Repeat("stderr\n", 2_000) + "final stderr",
			"stdout": "partial output",
		},
	})
	want, changed := TrimToolResultEcho("Bash", raw)
	if !changed {
		t.Fatal("byte API did not trim fixture")
	}
	obj := decodeObject(t, raw)
	if !TrimToolResultEchoObject("Bash", obj) {
		t.Fatal("object API did not trim fixture")
	}
	got, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal object API result: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("object API drifted from byte API\n got: %s\nwant: %s", got, want)
	}
}

func TestTrimToolResultEcho_FixedPoint(t *testing.T) {
	cases := map[string][]byte{
		"success": mustMarshal(t, map[string]any{
			"toolName":        "Bash",
			"is_error":        false,
			"tool_result":     map[string]any{"content": strings.Repeat("x", 10_000)},
			"tool_use_result": map[string]any{"stdout": strings.Repeat("y", 10_000)},
		}),
		"failure": mustMarshal(t, map[string]any{
			"toolName":        "Bash",
			"is_error":        true,
			"tool_result":     map[string]any{"content": strings.Repeat("line\n", 3_000)},
			"tool_use_result": map[string]any{"stderr": strings.Repeat("err\n", 3_000)},
		}),
	}
	for name, raw := range cases {
		first, changed := TrimToolResultEcho("Bash", raw)
		if !changed {
			t.Fatalf("%s: first pass should change", name)
		}
		second, changedAgain := TrimToolResultEcho("Bash", first)
		if changedAgain {
			t.Errorf("%s: second pass changed again:\nfirst:  %s\nsecond: %s", name, first, second)
		}
	}
}

func TestTrimToolResultEcho_NoEchoFieldsUntouched(t *testing.T) {
	raw := mustMarshal(t, map[string]any{
		"toolName": "Bash",
		"is_error": true,
		"input":    map[string]any{"command": "ls"},
	})
	trimmed, changed := TrimToolResultEcho("Bash", raw)
	if changed {
		t.Errorf("meta without echo fields must not change")
	}
	if string(trimmed) != string(raw) {
		t.Errorf("meta bytes changed")
	}
}

func TestTrimToolResultEcho_NonObjectToolUseResultDropped(t *testing.T) {
	raw := mustMarshal(t, map[string]any{
		"toolName":        "Read",
		"is_error":        true,
		"tool_use_result": "plain string enrichment",
	})
	trimmed, changed := TrimToolResultEcho("Read", raw)
	if !changed {
		t.Fatalf("expected trim to report a change")
	}
	top := decodeObject(t, trimmed)
	if _, ok := top["tool_use_result"]; ok {
		t.Errorf("non-object tool_use_result should be dropped (no readable paths)")
	}
}

func TestTailExcerpt(t *testing.T) {
	t.Run("short string passes through", func(t *testing.T) {
		if got := tailExcerpt("hello\nworld", 100); got != "hello\nworld" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("multi-line keeps complete trailing lines", func(t *testing.T) {
		s := strings.Repeat("aaaa\n", 100) + "tail line"
		got := tailExcerpt(s, 50)
		if !strings.HasSuffix(got, "tail line") {
			t.Errorf("lost tail: %q", got)
		}
		if strings.HasPrefix(got, "a") && len(strings.Split(got, "\n")[0]) != 4 {
			t.Errorf("first line is a fragment: %q", got)
		}
	})
	t.Run("single giant line cuts on rune boundary", func(t *testing.T) {
		s := strings.Repeat("é", 5_000) // 2 bytes per rune
		got := tailExcerpt(s, 101)      // odd cap forces a mid-rune cut
		if len(got) > 101 {
			t.Errorf("excerpt %d bytes > cap", len(got))
		}
		for _, r := range got {
			if r != 'é' {
				t.Errorf("rune corruption: %q", r)
				break
			}
		}
	})
	t.Run("whitespace-only collapses to empty", func(t *testing.T) {
		if got := tailExcerpt("   \n\t  ", 100); got != "" {
			t.Errorf("got %q", got)
		}
	})
}
