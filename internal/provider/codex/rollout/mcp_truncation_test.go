package rollout

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// codexTruncateMCPResult is a test-only port of rust-v0.162.1's
// truncate_mcp_tool_result (utils/output-truncation/src/lib.rs) over
// truncate_middle_chars (utils/string/src/truncate.rs), so fixtures carry
// the exact bytes a paginated rollout persists for an oversized result.
// `result` must already be in serde's field order: content, structuredContent,
// isError, _meta.
func codexTruncateMCPResult(t *testing.T, result map[string]any, order []string, maxBytes int) map[string]any {
	t.Helper()
	serialized := serdeJSON(t, result, order)
	if len(serialized) <= maxBytes {
		t.Fatalf("fixture result is %d bytes, not over the %d cap", len(serialized), maxBytes)
	}
	budget := maxBytes
	for {
		preview := ""
		if budget > 0 {
			preview = truncateMiddleChars(serialized, budget)
		}
		replacement := map[string]any{"content": []any{map[string]any{"type": "text", "text": preview}}}
		replacementOrder := []string{"content"}
		if isError, ok := result["isError"]; ok {
			replacement["isError"] = isError
			replacementOrder = append(replacementOrder, "isError")
		}
		size := len(serdeJSON(t, replacement, replacementOrder))
		if size <= maxBytes || budget == 0 {
			return replacement
		}
		next := budget * maxBytes / size
		if next > budget-1 {
			next = budget - 1
		}
		budget = next
	}
}

// truncateMiddleChars ports truncate_middle_chars for a string over budget.
func truncateMiddleChars(s string, maxBytes int) string {
	left := maxBytes / 2
	right := maxBytes - left
	tailStart := len(s) - right
	prefixEnd, suffixStart, removed := 0, len(s), 0
	suffixStarted := false
	for idx, r := range s {
		end := idx + utf8.RuneLen(r)
		if end <= left {
			prefixEnd = end
			continue
		}
		if idx >= tailStart {
			if !suffixStarted {
				suffixStart = idx
				suffixStarted = true
			}
			continue
		}
		removed++
	}
	return s[:prefixEnd] + "…" + strconv.Itoa(removed) + " chars truncated…" + s[suffixStart:]
}

// serdeJSON encodes an object with the given key order and serde_json's
// escaping (no HTML escapes).
func serdeJSON(t *testing.T, obj map[string]any, order []string) string {
	t.Helper()
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(key))
		b.WriteByte(':')
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(orderedBlocks(obj[key])); err != nil {
			t.Fatalf("encode %s: %v", key, err)
		}
		b.WriteString(strings.TrimSuffix(buf.String(), "\n"))
	}
	b.WriteByte('}')
	return b.String()
}

// orderedBlocks keeps MCP content blocks in the server's {"type","text"}
// order, which the codex binary preserves (serde_json preserve_order).
func orderedBlocks(value any) any {
	blocks, ok := value.([]any)
	if !ok {
		return value
	}
	out := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		m, ok := block.(map[string]any)
		if !ok || m["type"] != "text" || len(m) != 2 {
			raw, _ := json.Marshal(block)
			out = append(out, raw)
			continue
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(m["text"])
		out = append(out, json.RawMessage(`{"type":"text","text":`+strings.TrimSuffix(buf.String(), "\n")+`}`))
	}
	return out
}

func mcpItemLine(t *testing.T, result map[string]any) string {
	return pagLine(t, 5, "2026-08-07T19:07:50.000Z", map[string]any{
		"type": "McpToolCall", "id": "mcp-1", "server": "docs", "tool": "fetch",
		"arguments": map[string]any{"page": "guide"}, "status": "completed", "result": result,
	}, 1786133870000, 1786133870500)
}

func mcpOutput(t *testing.T, lines ...string) string {
	t.Helper()
	path := writeRollout(t, testSessionID, append([]string{paginatedMetaLine, taskStartedLine}, append(lines, taskCompleteLn)...)...)
	res := parseFixture(t, path)
	rows := toolsByName(res.Events, "docs__fetch")
	if len(rows) != 1 {
		t.Fatalf("mcp rows = %d, want 1", len(rows))
	}
	return rows[0].Content
}

// One long text block, the usual oversized result: both cut points fall in
// its text, so the reader gets the start and the end of the real text with
// a note where Codex cut it, not escaped JSON.
func TestParseRecoversTheTextOfAnMCPResultCodexTruncatedOnDisk(t *testing.T) {
	text := "BEGIN \"quoted\"\n" + strings.Repeat("line of \"text\" with a tab\t and a \\ backslash\n", 3000) + "END"
	original := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": false,
	}
	persisted := codexTruncateMCPResult(t, original, []string{"content", "isError"}, 64*1024)

	got := mcpOutput(t, mcpItemLine(t, persisted))

	if !strings.HasPrefix(got, "BEGIN \"quoted\"\nline of \"text\"") {
		t.Errorf("output starts %q, want the decoded text", got[:40])
	}
	if !strings.HasSuffix(got, "backslash\nEND") {
		t.Errorf("output ends %q, want the decoded end of the text", got[len(got)-40:])
	}
	if strings.Contains(got, `{"content"`) || strings.Contains(got, `\"`) {
		t.Errorf("output still carries serialized JSON")
	}
	if !strings.Contains(got, "[Codex truncated this MCP result when it saved the session: ") ||
		!strings.Contains(got, " characters are missing here.]") {
		t.Errorf("output has no truncation note: %q", got[len(got)/2-200:len(got)/2+200])
	}
}

// Codex's own fixture shape (truncate_mcp_tool_result_compacts_large_result):
// the tail lands in _meta, so only the head is text. The reader gets the
// head of the text and a note that the rest is gone.
func TestParseRecoversTheHeadWhenTheCutEndsOutsideTheText(t *testing.T) {
	original := map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": "head\n" + strings.Repeat("\"\\\n", 30000) + "\ntail"}},
		"structuredContent": map[string]any{"x": strings.Repeat("y", 30000)},
		"isError":           true,
		"_meta":             map[string]any{"private": "meta-head" + strings.Repeat("z", 30000) + "meta-tail"},
	}
	persisted := codexTruncateMCPResult(t, original, []string{"content", "structuredContent", "isError", "_meta"}, 64*1024)

	got := mcpOutput(t, mcpItemLine(t, persisted))

	if !strings.HasPrefix(got, "head\n\"\\\n\"\\\n") {
		t.Errorf("output starts %q, want the decoded head", got[:20])
	}
	if !strings.HasSuffix(got, "\n\n[Codex truncated this MCP result when it saved the session: the rest is missing.]") {
		t.Errorf("output ends %q, want the head-only note", got[len(got)-120:])
	}
	if strings.Contains(got, "meta-tail") {
		t.Errorf("output carries the _meta tail as text")
	}
}

// A long text block followed by a non-text block: the head cut lands in the
// text and the tail cut in the other block's string. Rejoined, the text's
// string runs on into that block's tail and its remaining keys close the
// text block, which still parses. Only a pure text block may carry the
// note, so this falls back to the head. Blocks are in the server's key
// order, which the codex binary keeps (serde_json preserve_order); the
// shapes are the MCP content types Codex 0.162.1 documents
// (code-mode-protocol/src/description.rs).
func TestParseDoesNotRunATextBlockIntoTheBlockAfterIt(t *testing.T) {
	head := "TEXT HEAD\n" + strings.Repeat("plain line of text\n", 3000)
	for _, tc := range []struct {
		name  string
		block string
		leak  string
	}{
		{
			name:  "image",
			block: `{"type":"image","data":"` + strings.Repeat("QUJD", 25000) + `","mimeType":"image/png"}`,
			leak:  "QUJD",
		},
		{
			name: "audio",
			block: `{"type":"audio","data":"` + strings.Repeat("UklG", 25000) +
				`","mimeType":"audio/wav","annotations":{"priority":0.5}}`,
			leak: "UklG",
		},
		{
			name: "resource link",
			block: `{"type":"resource_link","name":"guide","uri":"file:///guide.md","description":"` +
				strings.Repeat("DESC ", 20000) + `","mimeType":"text/markdown"}`,
			leak: "DESC",
		},
		{
			name: "embedded resource",
			block: `{"type":"resource","resource":{"uri":"file:///guide.md","mimeType":"text/markdown","text":"` +
				strings.Repeat("RES ", 25000) + `"}}`,
			leak: "RES ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": head},
					json.RawMessage(tc.block),
				},
				"isError": false,
			}
			persisted := codexTruncateMCPResult(t, original, []string{"content", "isError"}, 64*1024)

			got := mcpOutput(t, mcpItemLine(t, persisted))

			if !strings.HasPrefix(got, "TEXT HEAD\nplain line of text\n") {
				t.Errorf("output starts %q, want the text block's head", got[:30])
			}
			if !strings.HasSuffix(got, "[Codex truncated this MCP result when it saved the session: the rest is missing.]") {
				t.Errorf("output ends %q, want the head-only note", got[len(got)-120:])
			}
			if strings.Contains(got, tc.leak) {
				t.Errorf("output carries the %s block's content as text", tc.name)
			}
		})
	}
}

// When nothing parses (the cut is outside every text block) the saved
// preview is still shown, under the note.
func TestParseShowsTheSavedPreviewWhenNoTextCanBeRebuilt(t *testing.T) {
	original := map[string]any{
		"content": []any{map[string]any{"type": "image", "data": strings.Repeat("A", 100000), "mimeType": "image/png"}},
	}
	persisted := codexTruncateMCPResult(t, original, []string{"content"}, 64*1024)
	preview := persisted["content"].([]any)[0].(map[string]any)["text"].(string)

	got := mcpOutput(t, mcpItemLine(t, persisted))

	want := "[Codex truncated this MCP result when it saved the session. The saved preview follows.]\n\n" + preview
	if got != want {
		t.Errorf("output = %q..., want the note and the saved preview", got[:120])
	}
}

// A result that only looks like JSON, or carries no marker, is shown as
// the server returned it.
func TestParseLeavesOrdinaryMCPResultsAlone(t *testing.T) {
	for _, text := range []string{
		`{"content":"a tool that returns JSON"}`,
		"plain …3 chars truncated… text",
	} {
		got := mcpOutput(t, mcpItemLine(t, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
		}))
		if got != text {
			t.Errorf("output = %q, want %q unchanged", got, text)
		}
	}
}

// A cut that leaves a partial escape at the end of the head is trimmed
// rather than breaking the rebuilt JSON.
func TestTrimPartialEscape(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`abc\`:     `abc`,
		`abc\\`:    `abc\\`,
		`abc\u00`:  `abc`,
		`abcé`:     `abcé`,
		`abc\\u00`: `abc\\u00`,
		`abc`:      `abc`,
	} {
		if got := trimPartialEscape(in); got != want {
			t.Errorf("trimPartialEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
