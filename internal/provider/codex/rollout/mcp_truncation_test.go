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

// persistedOutput truncates `blocks` (raw JSON, in the server's key order,
// which the codex binary keeps) the way a paginated rollout persists them and
// returns what the import shows.
func persistedOutput(t *testing.T, blocks ...string) string {
	t.Helper()
	content := make([]any, 0, len(blocks))
	for _, block := range blocks {
		content = append(content, json.RawMessage(block))
	}
	original := map[string]any{"content": content, "isError": false}
	persisted := codexTruncateMCPResult(t, original, []string{"content", "isError"}, 64*1024)
	return mcpOutput(t, mcpItemLine(t, persisted))
}

func textBlock(t *testing.T, text string) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(text); err != nil {
		t.Fatalf("encode text: %v", err)
	}
	return `{"type":"text","text":` + strings.TrimSuffix(buf.String(), "\n") + `}`
}

const (
	gapNotePrefix = "[Codex truncated this MCP result when it saved the session: "
	gapNoteSuffix = " characters are missing here.]"
	endNote       = "[Codex truncated this MCP result when it saved the session: the rest is missing.]"
)

// One long text block, the usual oversized result. The head is the start of
// its text; the tail starts inside the same string, whose opening was cut
// away, so nothing after the cut can be attributed and the note ends it.
func TestParseRecoversTheHeadOfOneLongTextBlock(t *testing.T) {
	text := "BEGIN \"quoted\"\n" + strings.Repeat("line of \"text\" with a tab\t and a \\ backslash\n", 3000) + "END"
	got := persistedOutput(t, textBlock(t, text))

	if !strings.HasPrefix(got, "BEGIN \"quoted\"\nline of \"text\"") {
		t.Errorf("output starts %q, want the decoded text", got[:40])
	}
	if !strings.HasSuffix(got, "\n\n"+endNote) {
		t.Errorf("output ends %q, want the end note", got[len(got)-120:])
	}
	if strings.Contains(got, `{"content"`) || strings.Contains(got, `\"`) {
		t.Errorf("output still carries serialized JSON")
	}
	if !strings.HasPrefix(text, strings.TrimSuffix(got, "\n\n"+endNote)) {
		t.Errorf("recovered head is not a prefix of the real text")
	}
}

// Two text blocks, the second short enough to sit whole in the tail: the
// reader gets the head of the first, the note, and the whole second block.
func TestParseRecoversAWholeTextBlockFromTheTail(t *testing.T) {
	first := "FIRST " + strings.Repeat("alpha beta gamma\n", 6000)
	second := "SECOND block, short and whole."
	got := persistedOutput(t, textBlock(t, first), textBlock(t, second))

	if !strings.HasPrefix(got, "FIRST alpha beta gamma\n") {
		t.Errorf("output starts %q, want the first block's head", got[:30])
	}
	at := strings.Index(got, gapNotePrefix)
	if at < 0 || !strings.Contains(got[at:], gapNoteSuffix) {
		t.Fatalf("output has no gap note")
	}
	if !strings.HasSuffix(got, gapNoteSuffix+"\n\n"+second) {
		t.Errorf("output ends %q, want the note then the second block", got[len(got)-120:])
	}
}

// A long text block followed by an image, audio or link block, in each key
// order an MCP server can send: the head cut lands in the text and the tail
// cut in the other block. Nothing of the other block may read as text,
// whichever key comes last.
func TestParseDoesNotRunATextBlockIntoTheBlockAfterIt(t *testing.T) {
	head := "TEXT HEAD\n" + strings.Repeat("plain line of text\n", 3000)
	base64 := strings.Repeat("QUJD", 25000)
	for _, tc := range []struct {
		name  string
		block string
		leak  string
	}{
		{"image, data first", `{"type":"image","data":"` + base64 + `","mimeType":"image/png"}`, "QUJD"},
		{"image, data last", `{"type":"image","mimeType":"image/png","data":"` + base64 + `"}`, "QUJD"},
		{"image, type last", `{"data":"` + base64 + `","mimeType":"image/png","type":"image"}`, "QUJD"},
		{"audio", `{"type":"audio","mimeType":"audio/wav","annotations":{"priority":0.5},"data":"` + strings.Repeat("UklG", 25000) + `"}`, "UklG"},
		{"resource link", `{"type":"resource_link","name":"guide","uri":"file:///guide.md","mimeType":"text/markdown","description":"` + strings.Repeat("DESC ", 20000) + `"}`, "DESC"},
		{"embedded resource", `{"type":"resource","resource":{"uri":"file:///guide.md","mimeType":"text/markdown","text":"` + strings.Repeat("RES ", 25000) + `"}}`, "RES "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := persistedOutput(t, textBlock(t, head), tc.block)

			if !strings.HasPrefix(got, "TEXT HEAD\nplain line of text\n") {
				t.Errorf("output starts %q, want the text block's head", got[:30])
			}
			if !strings.HasSuffix(got, "\n\n"+endNote) {
				t.Errorf("output ends %q, want the end note", got[len(got)-120:])
			}
			if strings.Contains(got, tc.leak) {
				t.Errorf("output carries the %s block's content as text", tc.name)
			}
		})
	}
}

// An image first and a short text block last: the head holds no text, the
// tail holds the whole text block.
func TestParseRecoversTextAfterALeadingImage(t *testing.T) {
	got := persistedOutput(t,
		`{"type":"image","mimeType":"image/png","data":"`+strings.Repeat("QUJD", 30000)+`"}`,
		textBlock(t, "caption after the image"),
	)
	if !strings.HasPrefix(got, gapNotePrefix) || !strings.HasSuffix(got, gapNoteSuffix+"\n\ncaption after the image") {
		t.Errorf("output = %q, want the note then the caption", got)
	}
	if strings.Contains(got, "QUJD") {
		t.Errorf("output carries base64")
	}
}

// A block whose server sends `text` before `type`: only a text block has a
// block-level `text` key, so the cut string still counts as text.
func TestParseRecoversTheHeadWhenTextPrecedesType(t *testing.T) {
	got := persistedOutput(t, `{"text":"`+strings.Repeat("ordered ", 20000)+`","type":"text"}`)
	if !strings.HasPrefix(got, "ordered ordered ") || !strings.HasSuffix(got, endNote) {
		t.Errorf("output = %q..., want the head and the end note", got[:40])
	}
}

// Multi-byte characters and escapes at both cut points: the head stops on a
// whole character and escape, and the tail's whole block decodes exactly.
func TestParseHandlesUnicodeAndEscapesAtTheCuts(t *testing.T) {
	unit := "é漢\"\\\n\u0001\t😀"
	for offset := 0; offset < 12; offset++ {
		first := strings.Repeat("x", offset) + strings.Repeat(unit, 9000)
		second := "tail " + unit + " end"
		got := persistedOutput(t, textBlock(t, first), textBlock(t, second))

		head, rest, found := strings.Cut(got, "\n\n"+gapNotePrefix)
		if !found {
			t.Fatalf("offset %d: no gap note in %q", offset, got[len(got)-200:])
		}
		if !strings.HasPrefix(first, head) {
			t.Errorf("offset %d: recovered head is not a prefix of the real text", offset)
		}
		if !strings.HasSuffix(rest, gapNoteSuffix+"\n\n"+second) {
			t.Errorf("offset %d: output ends %q, want the whole second block", offset, rest[len(rest)-60:])
		}
	}
}

// Codex's own fixture shape (truncate_mcp_tool_result_compacts_large_result):
// the tail lands in _meta, so only the head is text.
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
	if !strings.HasSuffix(got, "\n\n"+endNote) {
		t.Errorf("output ends %q, want the end note", got[len(got)-120:])
	}
	if strings.Contains(got, "meta-tail") {
		t.Errorf("output carries the _meta tail as text")
	}
}

// The whole text fits in the head and the cut falls in the structured
// content after it: the text is complete and the note says what is gone.
func TestParseKeepsWholeTextWhenOnlyStructuredDataWasCut(t *testing.T) {
	original := map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": "short summary"}},
		"structuredContent": map[string]any{"rows": strings.Repeat("r", 100000)},
		"isError":           false,
	}
	persisted := codexTruncateMCPResult(t, original, []string{"content", "structuredContent", "isError"}, 64*1024)

	got := mcpOutput(t, mcpItemLine(t, persisted))

	want := "short summary\n\n[Codex truncated this MCP result when it saved the session: its structured data is missing.]"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// When neither side holds text (an image alone) the saved preview is still
// shown, under the note.
func TestParseShowsTheSavedPreviewWhenNoTextCanBeRecovered(t *testing.T) {
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
