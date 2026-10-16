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
// Read as-is that is clipped raw JSON. recoverPersistedMCPText recovers the
// text it can attribute to a text block and says where Codex cut it.
//
// The head and tail are never joined into one string. The cut can fall in
// any two places, and a head cut inside one block's string joined to a tail
// cut inside another block's string can parse as one plausible block (a
// text block whose text runs on into an image's base64). Each side is
// scanned on its own instead:
//
//   - the head from its start, where the JSON state is known: every text
//     block it holds whole, then the string the head was cut in when that
//     string is the `text` of a text block;
//   - the tail from its end, where the state is known again (a quote is a
//     delimiter unless an odd run of backslashes precedes it, which can be
//     read backwards): every text block it holds whole. The fragment at the
//     tail's start belongs to a block whose opening was cut away, so it is
//     skipped whatever it looks like.
//
// Key order inside a block is whatever the MCP server sent (the codex binary
// builds serde_json with preserve_order), so nothing here assumes one.

// persistedMCPCut is truncate_middle_chars' marker. Its byte form (U+2026
// on both sides, "chars" not "tokens") is what tells this shape apart from
// a result that merely contains JSON.
var persistedMCPCut = regexp.MustCompile(`…(\d+) chars truncated…`)

const persistedMCPPrefix = `{"content":[`

// textBlockKeys are the keys an MCP TextContent block may carry (Codex
// 0.162.1 code-mode-protocol/src/description.rs, after the MCP schema). The
// other content types each add a key a text block never has: `data` and
// `mimeType` (image, audio), `resource`, `uri` and `name` (resource,
// resource_link).
var textBlockKeys = map[string]bool{"type": true, "text": true, "annotations": true, "_meta": true}

const (
	persistedNoteGap = "[Codex truncated this MCP result when it saved the session: %s characters are missing here.]"
	persistedNoteEnd = "[Codex truncated this MCP result when it saved the session: the rest is missing.]"
	persistedNoteRaw = "[Codex truncated this MCP result when it saved the session. The saved preview follows.]"
	// The text is whole; what was cut is the structured content or metadata
	// after it.
	persistedNoteData = "[Codex truncated this MCP result when it saved the session: its structured data is missing.]"
)

// recoverPersistedMCPText returns the text a reader should see for a result
// Codex truncated on disk, and false for any other content. When neither
// side yields text it shows the saved preview under a note.
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
	removed := preview[cut[2]:cut[3]]

	head, contentClosed := persistedHeadText(preview[:cut[0]])
	var tail []string
	if !contentClosed {
		tail = persistedTailText(preview[cut[1]:])
	}
	if len(head) == 0 && len(tail) == 0 {
		return persistedNoteRaw + "\n\n" + preview, true
	}

	var b strings.Builder
	b.WriteString(strings.Join(head, "\n"))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	if contentClosed {
		b.WriteString(persistedNoteData)
		return b.String(), true
	}
	if len(tail) == 0 {
		b.WriteString(persistedNoteEnd)
		return b.String(), true
	}
	b.WriteString(strings.Replace(persistedNoteGap, "%s", removed, 1))
	b.WriteString("\n\n")
	b.WriteString(strings.Join(tail, "\n"))
	return b.String(), true
}

// persistedHeadText scans the head of a serialized CallToolResult from its
// start. It returns the text of every text block the head holds whole, plus
// the cut string when that string is the `text` of a text block, and
// whether the content array closed inside the head (then the tail is past
// the content and has no blocks to offer).
func persistedHeadText(head string) (texts []string, contentClosed bool) {
	var (
		depth      int    // 1 root, 2 content array, 3 block, deeper inside a block
		inString   bool   // inside a string literal
		stringFrom int    // index just past the opening quote
		isKey      bool   // the open string is an object key
		blockFrom  int    // index of the open block's '{'
		blockKey   string // the last key read at block depth
		blockType  string // the open block's "type", once read
		expectKey  bool   // the next string at the current object depth is a key
	)
	if !strings.HasPrefix(head, persistedMCPPrefix) {
		return nil, false
	}
	depth = 2
	for i := len(persistedMCPPrefix); i < len(head); i++ {
		c := head[i]
		if inString {
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
				if depth == 3 {
					value := head[stringFrom:i]
					if isKey {
						blockKey = decodeJSONString(value)
					} else if blockKey == "type" {
						blockType = decodeJSONString(value)
					}
				}
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			stringFrom = i + 1
			isKey = expectKey
			expectKey = false
		case ':':
			expectKey = false
		case ',':
			// A comma inside an object precedes a key; inside an array it
			// precedes a value. Depth 2 is the content array; odd depths
			// past 3 are not tracked, so only block depth matters here.
			expectKey = depth == 3
		case '{':
			depth++
			expectKey = true
			if depth == 3 {
				blockFrom, blockKey, blockType = i, "", ""
			}
		case '[':
			depth++
			expectKey = false
		case '}':
			if depth == 3 {
				if text, ok := wholeTextBlock(head[blockFrom : i+1]); ok {
					texts = append(texts, text)
				}
			}
			depth--
		case ']':
			depth--
			if depth == 1 {
				return texts, true
			}
		}
	}
	// The head ends inside a string. It is a text block's text when it is
	// the value of the block's own `text` key and the block is not some
	// other type. Only TextContent has a block-level `text` key, so a type
	// that has not been read yet (it comes after `text` in this server's
	// key order) does not disqualify it.
	if inString && !isKey && depth == 3 && blockKey == "text" && (blockType == "" || blockType == "text") {
		if text := decodeJSONString(trimPartialEscape(head[stringFrom:])); strings.TrimSpace(text) != "" {
			texts = append(texts, text)
		}
	}
	return texts, false
}

// persistedTailText scans the tail of a serialized CallToolResult from its
// end and returns, in order, the text of every text block of the content
// array that lies whole inside the tail.
func persistedTailText(tail string) []string {
	type container struct {
		close byte // '}' or ']'
		end   int  // index of the closing byte
	}
	var (
		stack    []container
		inString bool
		blocks   [][2]int // [start, end] of whole objects one level inside an unopened array
	)
	for i := len(tail) - 1; i >= 0; i-- {
		c := tail[i]
		if c == '"' && !escapedAt(tail, i) {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '}', ']':
			stack = append(stack, container{close: c, end: i})
		case '{', '[':
			if len(stack) == 0 {
				return nil // more openers than closers: not the shape expected
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			// A whole object whose parent is the array still open at this
			// point. Whether that array is the content array is settled
			// below, once the scan knows which containers never opened.
			if c == '{' && top.close == '}' && len(stack) == 2 && stack[1].close == ']' {
				blocks = append(blocks, [2]int{i, top.end})
			}
		}
	}
	// What never opened inside the tail: the root object and, when the tail
	// starts inside the content, the content array (with the cut block, if
	// the tail starts inside one). A tail that starts past the content array
	// leaves the root alone open, or the root and another value.
	if len(stack) < 2 || stack[0].close != '}' || stack[1].close != ']' {
		return nil
	}
	contentEnd := stack[1].end
	var texts []string
	for i := len(blocks) - 1; i >= 0; i-- {
		start, end := blocks[i][0], blocks[i][1]
		if end > contentEnd {
			continue // inside the root's later array values, not the content
		}
		if text, ok := wholeTextBlock(tail[start : end+1]); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

// wholeTextBlock decodes one complete content block and returns its text
// when it is a pure MCP text block.
func wholeTextBlock(raw string) (string, bool) {
	var block map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &block) != nil {
		return "", false
	}
	if kind, _ := rawString(block, "type"); kind != "text" {
		return "", false
	}
	for key := range block {
		if !textBlockKeys[key] {
			return "", false
		}
	}
	text, ok := rawString(block, "text")
	return text, ok && strings.TrimSpace(text) != ""
}

// escapedAt reports whether the byte at i is escaped: an odd run of
// backslashes directly before it.
func escapedAt(s string, i int) bool {
	run := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		run++
	}
	return run%2 == 1
}

// decodeJSONString decodes the body of a JSON string literal (without its
// quotes). A body that does not decode is returned as written.
func decodeJSONString(body string) string {
	var out string
	if json.Unmarshal([]byte(`"`+body+`"`), &out) != nil {
		return body
	}
	return out
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
