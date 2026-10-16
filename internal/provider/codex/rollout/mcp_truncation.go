package rollout

import (
	"encoding/json"
	"regexp"
	"strings"
)

// A paginated rollout caps a persisted MCP result at 64 KiB
// (PERSISTED_MCP_RESULT_MAX_BYTES, codex-rs/rollout/src/policy.rs). An
// oversized result is replaced by truncate_mcp_tool_result
// (codex-rs/utils/output-truncation/src/lib.rs) with ONE text block whose
// text is the original CallToolResult serialized to JSON and cut in the
// middle by truncate_middle_chars (codex-rs/utils/string/src/truncate.rs):
//
//	{"content":[{"type":"text","text":"{\"content\":[{\"type\":\"text\",\"text\":\"head…12345 chars truncated…tail\"}],\"isError\":false}"}],"isError":false}
//
// Read as-is that is clipped raw JSON. recoverPersistedMCPText rebuilds
// the readable text from it and says where Codex cut it.

// persistedMCPCut is truncate_middle_chars' marker. Its byte form (U+2026
// on both sides, "chars" not "tokens") is what tells this shape apart from
// a result that merely contains JSON.
var persistedMCPCut = regexp.MustCompile(`…(\d+) chars truncated…`)

const persistedMCPPrefix = `{"content":`

// recoverPersistedMCPText returns the text a reader should see for a result
// Codex truncated on disk, and false for any other content.
//
// It tries, in order:
//  1. the whole text: the head and tail rejoined around a note, which
//     parses when both cut points fall inside the same run of string
//     content (one long text block, the usual oversized result);
//  2. the head alone, closed after a note, which parses when the head ends
//     inside a top-level text block;
//  3. the saved preview as Codex wrote it, after a note.
func recoverPersistedMCPText(content json.RawMessage) (string, bool) {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil || len(blocks) != 1 || blocks[0].Type != "text" {
		return "", false
	}
	preview := blocks[0].Text
	if !strings.HasPrefix(preview, persistedMCPPrefix) {
		return "", false
	}
	cut := persistedMCPCut.FindStringSubmatchIndex(preview)
	if cut == nil {
		return "", false
	}
	head := trimPartialEscape(preview[:cut[0]])
	tail := preview[cut[1]:]
	removed := preview[cut[2]:cut[3]]

	middle := "\n\n[Codex truncated this MCP result when it saved the session: " +
		removed + " characters are missing here.]\n\n"
	if text, ok := persistedResultText(head + jsonStringBody(middle) + tail); ok {
		return text, true
	}
	end := "\n\n[Codex truncated this MCP result when it saved the session: the rest is missing.]"
	if text, ok := persistedResultText(head + jsonStringBody(end) + `"}]}`); ok {
		return text, true
	}
	return "[Codex truncated this MCP result when it saved the session. The saved preview follows.]\n\n" +
		preview, true
}

// persistedResultText reads the text blocks out of a rebuilt CallToolResult.
func persistedResultText(candidate string) (string, bool) {
	var result struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal([]byte(candidate), &result) != nil {
		return "", false
	}
	text, present := contentText(result.Content)
	if !present || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// trimPartialEscape drops an escape sequence the cut left unfinished at the
// end of s: a lone backslash, or `\u` with fewer than four hex digits.
func trimPartialEscape(s string) string {
	if i := strings.LastIndex(s, `\u`); i >= 0 && len(s)-i < 6 && oddBackslashRunEndingAt(s, i) {
		return s[:i]
	}
	if strings.HasSuffix(s, `\`) && oddBackslashRunEndingAt(s, len(s)-1) {
		return s[:len(s)-1]
	}
	return s
}

// oddBackslashRunEndingAt reports whether the backslash at i starts an
// escape, i.e. is not itself escaped by the one before it.
func oddBackslashRunEndingAt(s string, i int) bool {
	run := 0
	for j := i; j >= 0 && s[j] == '\\'; j-- {
		run++
	}
	return run%2 == 1
}

// jsonStringBody is s encoded as JSON string content, without the quotes.
func jsonStringBody(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded[1 : len(encoded)-1])
}
