package sessionimport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
)

// liteReadBufSize is the head/tail window size. Ported verbatim from
// Claude Code's LITE_READ_BUF_SIZE (sessionStoragePortable.ts): the
// listing is metadata-only, so a multi-MB transcript costs two 64 KB
// reads instead of a parse.
const liteReadBufSize = 64 * 1024

// errEmptySession marks a zero-byte (or unreadable-at-offset-0) file.
// Not a warning-worthy failure on its own — the caller decides.
var errEmptySession = errors.New("sessionimport: session file is empty")

// liteBufSize is the buffer one worker owns: the head window and the tail
// window side by side, so a whole listing pass allocates once per worker
// instead of two 64 KB strings per file. A real Claude home is a thousand
// transcripts; the string copies alone were 128 MB of garbage per listing.
const liteBufSize = 2 * liteReadBufSize

// liteFile is the head/tail/stat triple every listing field is derived
// from. Head and Tail ALIAS the caller's buffer and are only valid until
// the next readLite call with the same buffer — which is exactly the
// lifetime parseSessionInfo needs. They are the same bytes for files at or
// under liteReadBufSize, which is what makes "last X in tail || first X in
// head" collapse correctly on short sessions.
type liteFile struct {
	Head          []byte
	Tail          []byte
	Size          int64
	ModTimeMillis int64
}

// readLite opens path once and reads its first and last liteReadBufSize
// bytes into the caller-supplied buffer (one buffer per worker, no
// per-file allocation). buf must be at least liteBufSize long.
func readLite(path string, buf []byte) (liteFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return liteFile{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return liteFile{}, err
	}

	head, tail := buf[:liteReadBufSize], buf[liteReadBufSize:liteBufSize]
	n, err := f.ReadAt(head, 0)
	if n == 0 {
		if err != nil && !errors.Is(err, io.EOF) {
			return liteFile{}, err
		}
		return liteFile{}, errEmptySession
	}
	out := liteFile{
		Head:          head[:n],
		Size:          st.Size(),
		ModTimeMillis: st.ModTime().UnixMilli(),
	}
	out.Tail = out.Head

	if st.Size() > liteReadBufSize {
		m, err := f.ReadAt(tail, st.Size()-liteReadBufSize)
		if m > 0 {
			out.Tail = tail[:m]
		} else if err != nil && !errors.Is(err, io.EOF) {
			return liteFile{}, err
		}
	}
	return out, nil
}

// unescapeJSONString turns a raw JSON string body (the bytes between the
// quotes, escapes intact) into its decoded value. Allocation-free when
// there is nothing to unescape, and value-preserving when the escape
// sequence is malformed — a truncated tail read can cut a value in half
// and that is not a reason to lose the readable prefix.
func unescapeJSONString(raw []byte) string {
	if !bytes.ContainsRune(raw, '\\') {
		return string(raw)
	}
	quoted := make([]byte, 0, len(raw)+2)
	quoted = append(quoted, '"')
	quoted = append(quoted, raw...)
	quoted = append(quoted, '"')
	var out string
	if json.Unmarshal(quoted, &out) != nil {
		return string(raw)
	}
	return out
}

// jsonStringFieldPatterns are the two spellings Claude's writers emit for
// `"key":"value"`. Scanned as raw text rather than parsed because a
// head/tail window routinely cuts a line in half, and json.Unmarshal
// answers "nothing" for a truncated line that plainly contains the field.
func jsonStringFieldPatterns(key string) [2][]byte {
	return [2][]byte{[]byte(`"` + key + `":"`), []byte(`"` + key + `": "`)}
}

// scanStringValue walks the escape sequences of a JSON string body
// starting at valueStart and returns the decoded value plus the index of
// its closing quote. ok is false when the value never terminates inside
// text (truncated window).
func scanStringValue(text []byte, valueStart int) (value string, end int, ok bool) {
	for i := valueStart; i < len(text); {
		switch text[i] {
		case '\\':
			i += 2
		case '"':
			return unescapeJSONString(text[valueStart:i]), i, true
		default:
			i++
		}
	}
	return "", len(text), false
}

// extractJSONStringField returns the FIRST `"key":"value"` in text.
//
// First by POSITION IN THE BUFFER, across both spellings — never by which
// spelling was tried first. The two writers producing them are different
// versions of the CLI, so one window can hold both, and picking by pattern
// order would answer with a value from further down the file whenever the
// compact spelling happened to appear after the spaced one.
func extractJSONStringField(text []byte, key string) string {
	best := -1
	value := ""
	for _, pattern := range jsonStringFieldPatterns(key) {
		idx := bytes.Index(text, pattern)
		if idx < 0 || (best >= 0 && idx > best) {
			continue
		}
		candidate, _, ok := scanStringValue(text, idx+len(pattern))
		if !ok {
			// The window cut this value in half; a later occurrence of the
			// same spelling cannot terminate either (the pattern's own quotes
			// would have closed it), so there is nothing else to try here.
			continue
		}
		best, value = idx, candidate
	}
	return value
}

// extractLastJSONStringField returns the LAST `"key":"value"` in text.
// Appended metadata records (customTitle, lastPrompt, gitBranch) are
// re-written on every session save, so the last one is the current one.
//
// Last by POSITION IN THE BUFFER, across both spellings, for the mirror of
// the reason above: whichever spelling the CLI wrote most recently is the
// current value, and scanning one spelling to exhaustion before the other
// would let an older record win purely by being spelled differently.
func extractLastJSONStringField(text []byte, key string) string {
	best := -1
	last := ""
	for _, pattern := range jsonStringFieldPatterns(key) {
		from := 0
		for from <= len(text) {
			rel := bytes.Index(text[from:], pattern)
			if rel < 0 {
				break
			}
			idx := from + rel
			value, end, ok := scanStringValue(text, idx+len(pattern))
			if ok && idx > best {
				best, last = idx, value
			}
			from = end + 1
		}
	}
	return last
}

// firstNonEmpty returns the first non-empty argument, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var (
	// sessionUUIDRe is the filename admission rule: a session transcript
	// is `<uuid>.jsonl`. Anything else in a project directory (subagent
	// subdirs, `memory/`, editor scratch files) is not a session.
	sessionUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

	// skipFirstPromptRe matches auto-generated first "prompts" that are
	// not something a user typed: an XML-ish wrapper tag (IDE context,
	// hook output, task notification) or the synthetic interrupt marker.
	skipFirstPromptRe = regexp.MustCompile(`^(?:\s*<[a-z][\w-]*[\s>]|\[Request interrupted by user[^\]]*\])`)

	commandNameRe = regexp.MustCompile(`<command-name>(.*?)</command-name>`)
	bashInputRe   = regexp.MustCompile(`(?s)<bash-input>(.*?)</bash-input>`)
)

// validSessionUUID reports whether name is a session-file basename.
func validSessionUUID(name string) bool {
	return sessionUUIDRe.MatchString(name)
}

// maxFirstPromptRunes bounds the derived title. Matches the CLI's own
// 200-character cut, measured in runes so a multi-byte character is
// never split.
const maxFirstPromptRunes = 200

// extractFirstPromptFromHead finds the first real user prompt in a head
// window. It is the last fallback of the title chain, so it filters
// everything that is not a person talking: tool-result echoes, meta
// caveats, compaction summaries, and wrapper-tag context injections. A
// slash-command echo is remembered as a weaker fallback rather than
// returned outright — "/compact" is a worse title than the prompt after
// it, but a better one than nothing.
//
// The per-line prefilter is a raw substring test on purpose: the head
// window's last line is usually truncated, and json.Unmarshal is only
// ever run on the one candidate line that survives the filters.
func extractFirstPromptFromHead(head []byte) string {
	var commandFallback string
	for rest := head; len(rest) > 0; {
		var line []byte
		if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
			line, rest = rest[:idx], rest[idx+1:]
		} else {
			line, rest = rest, nil
		}
		if !containsAny(line, `"type":"user"`, `"type": "user"`) {
			continue
		}
		if bytes.Contains(line, []byte(`"tool_result"`)) {
			continue
		}
		if containsAny(line, `"isMeta":true`, `"isMeta": true`) {
			continue
		}
		if containsAny(line, `"isCompactSummary":true`, `"isCompactSummary": true`) {
			continue
		}

		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Type != "user" {
			continue
		}
		for _, raw := range userContentTexts(entry.Message.Content) {
			result := strings.TrimSpace(strings.ReplaceAll(raw, "\n", " "))
			if result == "" {
				continue
			}
			if m := commandNameRe.FindStringSubmatch(result); m != nil {
				if commandFallback == "" {
					commandFallback = m[1]
				}
				continue
			}
			// Bash-mode input is a real prompt; format it the way the CLI
			// does before the generic wrapper-tag skip can discard it.
			if m := bashInputRe.FindStringSubmatch(result); m != nil {
				return "! " + strings.TrimSpace(m[1])
			}
			if skipFirstPromptRe.MatchString(result) {
				continue
			}
			return truncateRunes(result, maxFirstPromptRunes)
		}
	}
	return commandFallback
}

func containsAny(s []byte, needles ...string) bool {
	for _, n := range needles {
		if bytes.Contains(s, []byte(n)) {
			return true
		}
	}
	return false
}

// userContentTexts flattens a `message.content` value into its text
// bodies. Claude writes a typed prompt as a plain string and a prompt
// with attachments (or an AO-composed send) as an array of blocks, so
// both shapes are real and both must read back.
func userContentTexts(content json.RawMessage) []string {
	if len(content) == 0 {
		return nil
	}
	var asString string
	if json.Unmarshal(content, &asString) == nil {
		return []string{asString}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return texts
}

// truncateRunes cuts s to at most max runes, appending an ellipsis when
// it actually cut.
func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}
