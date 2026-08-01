package codex

import (
	"encoding/json"
	"strconv"
	"strings"
)

// decodeTopLevel decodes a JSON object into its top-level raw fields.
// Returns nil on malformed input, which every readRaw* helper treats as
// key-absent — the same "" / nil fallbacks the per-field readTopLevel*
// helpers produce. Use it on hot paths that read several fields from
// the same params blob so the map decode happens once.
func decodeTopLevel(data json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m
}

// readTopLevelString reads a string from the top level of a JSON object.
func readTopLevelString(data json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func readTopLevelIDString(data json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var num json.Number
	if json.Unmarshal(raw, &num) == nil {
		return num.String()
	}
	return ""
}

// readTopLevelBool reads a boolean from the top level of a JSON object.
// Returns false if the key is missing or the value is not a boolean.
func readTopLevelBool(data json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	raw, ok := m[key]
	if !ok {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false
	}
	return b
}

// readNestedString reads a string by walking through nested object keys.
// E.g., readNestedString(data, "turn", "id") reads data.turn.id.
func readNestedString(data json.RawMessage, keys ...string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for i, key := range keys {
		raw, ok := m[key]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
			return ""
		}
		if json.Unmarshal(raw, &m) != nil {
			return ""
		}
	}
	return ""
}

// extractCodexUserMessageText flattens the content of a Codex
// `userMessage` item into a plain string. Codex emits content in two
// shapes: a plain string (the simplest case, also matches Claude's
// SDK replay envelope) or an array of blocks
// `[{type:"text",text:"..."}, ...]`. Image blocks are silently
// skipped — the rendered transcript shows them out-of-band, so
// concatenating a JSON dump into the user-text body would just add
// noise. Mirrors `extractToolResultText` in claude/parse_user.go;
// kept separate from `contentBlocksText` because the MCP path falls
// back to `compactJSON` for unknown blocks (which is wrong for user
// text — we want a clean drop, not JSON spillover).
func extractCodexUserMessageText(item map[string]json.RawMessage) string {
	if item == nil {
		return ""
	}
	contentRaw, ok := item["content"]
	if !ok || len(contentRaw) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(contentRaw, &asString) == nil {
		return asString
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil {
		return ""
	}
	var builder []byte
	for _, block := range blocks {
		if readRawString(block, "type") != "text" {
			// image / other non-text blocks: no text body to extract.
			continue
		}
		var text string
		if rawText, present := block["text"]; present {
			if json.Unmarshal(rawText, &text) == nil {
				builder = append(builder, text...)
			}
		}
	}
	return string(builder)
}

func extractCodexReasoningText(item map[string]json.RawMessage) (string, bool) {
	if item == nil {
		return "", false
	}
	for _, key := range []string{"summary", "content"} {
		raw, ok := item[key]
		if !ok {
			continue
		}
		return extractCodexTextField(raw), true
	}
	return "", false
}

func extractCodexTextField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var builder []byte
	for _, block := range blocks {
		if text := readRawString(block, "text"); text != "" {
			builder = append(builder, text...)
		}
	}
	return string(builder)
}

func extractCodexPlanMarkdown(data json.RawMessage) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}

	candidates := []string{
		readNestedString(data, "item", "text"),
		readNestedString(data, "item", "summary"),
		readNestedString(data, "item", "title"),
		readNestedString(data, "item", "result", "text"),
		readNestedString(data, "item", "result", "summary"),
		readTopLevelString(data, "text"),
		readTopLevelString(data, "summary"),
		readTopLevelString(data, "message"),
	}
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}

	var item map[string]json.RawMessage
	if rawItem, ok := payload["item"]; ok && json.Unmarshal(rawItem, &item) == nil {
		if rawResult, ok := item["result"]; ok {
			var result map[string]json.RawMessage
			if json.Unmarshal(rawResult, &result) == nil {
				if command := readRawString(result, "command"); command != "" {
					return command
				}
			}
		}
	}

	return ""
}

func readRawString(m map[string]json.RawMessage, key string) string {
	value, _ := readRawStringPresent(m, key)
	return value
}

func readRawStringPresent(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", true
	}
	return s, true
}

func readRawObject(m map[string]json.RawMessage, key string) map[string]json.RawMessage {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return obj
}

func decodeFunctionArguments(raw string) (map[string]json.RawMessage, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &args) != nil {
		return nil, false
	}
	return args, true
}

func readFlexibleString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var num json.Number
	if json.Unmarshal(raw, &num) == nil {
		return num.String()
	}
	return ""
}

func firstRaw(m map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, key := range keys {
		if raw, ok := m[key]; ok {
			return raw
		}
	}
	return nil
}

func firstRawString(m map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if value := readRawString(m, key); strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// parseCodexRetryCounts extracts an "N/M" attempt/total pair from a
// Codex retry message ("Reconnecting... 2/5"). Returns zeros when no
// pair is found — the caller treats unknown counts as "show on every
// attempt" so a one-off reconnect surface stays visible. Codex doesn't
// structure these counts, so this is the best-effort sibling of
// Claude's wire-typed `attempt`/`max_retries` fields.
func parseCodexRetryCounts(message string) (attempt int, maxRetries int) {
	m := codexRetryCountsRE.FindStringSubmatch(message)
	if m == nil {
		return 0, 0
	}
	attempt, _ = strconv.Atoi(m[1])
	maxRetries, _ = strconv.Atoi(m[2])
	return attempt, maxRetries
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func prettyJSONIfMeaningful(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return ""
		}
	case []any:
		if len(typed) == 0 {
			return ""
		}
	case map[string]any:
		if len(typed) == 0 {
			return ""
		}
	}
	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(out)
}

func compactJSON(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return strings.TrimSpace(string(raw))
	}
	out, err := json.Marshal(value)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(out)
}

func readNestedObject(data json.RawMessage, keys ...string) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	for i, key := range keys {
		raw, ok := m[key]
		if !ok {
			return nil
		}
		if i == len(keys)-1 {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) != nil {
				return nil
			}
			return nested
		}
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
	}
	return nil
}

func readRawStringArray(m map[string]json.RawMessage, key string) []string {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

func readRawInt(m map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := m[key]
	if !ok {
		return 0, false
	}
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

// readRawJSONObject decodes the value at key as a `map[string]any` so it
// can be merged back into Meta without losing structure. Returns nil if
// the key is missing, empty, or the value is not a JSON object — a null
// return is indistinguishable from absent, which matches the callers'
// "only include when populated" semantics. Used for agentsStates
// enrichment where we want the nested {status, message?} shape to
// survive the re-encode.
func readRawJSONObject(m map[string]json.RawMessage, key string) map[string]any {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	if len(obj) == 0 {
		return nil
	}
	return obj
}

// mergeMetaKeys decodes base as a JSON object, overlays extras on top, and
// returns the re-encoded result. If base is not a JSON object (or decoding
// fails) we fall back to marshaling extras alone so the enrichment keys are
// still present. Used by turn/completed and item enrichment to carry both
// raw wire fields and synthesized top-level keys in the same Meta blob.
func mergeMetaKeys(base json.RawMessage, extras map[string]any) json.RawMessage {
	var merged map[string]any
	if err := json.Unmarshal(base, &merged); err != nil || merged == nil {
		merged = map[string]any{}
	}
	for k, v := range extras {
		merged[k] = v
	}
	out, err := json.Marshal(merged)
	if err != nil {
		// Shouldn't happen: merged is built from decoded JSON + known-safe
		// values. Preserve base rather than drop everything.
		return base
	}
	return out
}
