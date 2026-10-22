package claudetui

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// sessionStartMarker opens the block a model request carries a SessionStart
// run's contexts in, joined by line breaks, inside a system reminder. It
// names no source, so a resumed transcript's earlier runs carry the same
// marker (docs/references/claude-wire.md, SessionStart hooks).
const sessionStartMarker = "SessionStart hook additional context: "

// observeMainRequest settles an expected SessionStart delivery on the first
// main request after a start, a resume or a compaction, and keeps its
// receipt: the request carries the hook's context, under its heading and
// stamp, when it delivered. The body is searched only while a delivery is
// expected.
func (s *Session) observeMainRequest(body []byte) {
	s.sessionStartMu.Lock()
	if s.sessionStart.Expecting() == "" {
		s.sessionStartMu.Unlock()
		return
	}
	if context, ok := sessionStartContext(body, *s.sessionStart.Hook(), s.sessionStartSince, s.logf); ok {
		s.sessionStart.Deliver(context)
	}
	warnings := []string{s.sessionStart.Settle(s.SessionID()), s.sessionStart.Keep()}
	s.sessionStartMu.Unlock()
	for _, warning := range warnings {
		if warning != "" {
			s.emitNonFatalError(warning)
		}
	}
}

// sessionStartContext is the latest context of hook in a model request body
// that a run at or after since delivered. A context runs from the hook's
// heading through its ending, or to the end of its block when the ending is
// missing, and counts only when the line after the heading is the hook's
// stamp of a time not before since: an earlier run's block, which a resumed
// transcript or a compaction keeps, or a summary that quotes the heading, is
// not this run's delivery. The blocks are read from the last, so the search
// usually decodes one.
func sessionStartContext(body []byte, hook provider.SessionStartHook, since time.Time, logf func(string, ...any)) (string, bool) {
	// A stamp has milliseconds and no finer.
	since = since.Truncate(time.Millisecond)
	marker := []byte(sessionStartMarker)
	for end := len(body); ; {
		at := bytes.LastIndex(body[:end], marker)
		if at < 0 {
			return "", false
		}
		end = at
		block, ok := sessionStartBlock(body[at+len(marker):])
		if !ok {
			logf("claudetui: a SessionStart context block in a model request is not a JSON string; its contexts do not count")
			continue
		}
		if context, ok := stampedContext(block, hook, since); ok {
			return context, true
		}
	}
}

// sessionStartBlock decodes the rest of the JSON string that rest, the bytes
// after a marker, opens inside.
func sessionStartBlock(rest []byte) (string, bool) {
	end := 0
	for end < len(rest) && rest[end] != '"' {
		if rest[end] == '\\' {
			end++
		}
		end++
	}
	if end >= len(rest) {
		return "", false
	}
	var block string
	if err := json.Unmarshal(append(append([]byte{'"'}, rest[:end]...), '"'), &block); err != nil {
		return "", false
	}
	return block, true
}

// stampedContext is hook's context in block if one carries a stamp of a time
// not before since.
func stampedContext(block string, hook provider.SessionStartHook, since time.Time) (string, bool) {
	opening := hook.Heading + "\n" + hook.Stamp
	for from := 0; ; {
		at := strings.Index(block[from:], opening)
		if at < 0 {
			return "", false
		}
		at += from
		from = at + len(opening)
		stamp := block[from:]
		stamp = stamp[:strings.IndexFunc(stamp+"]", func(r rune) bool { return !strings.ContainsRune("0123456789-:.TZ+", r) })]
		if when, err := time.Parse(time.RFC3339, stamp); err != nil || when.Before(since) {
			continue
		}
		context := block[at:]
		if end := strings.Index(context, hook.Ending); end >= 0 {
			context = context[:end+len(hook.Ending)]
		}
		return context, true
	}
}
