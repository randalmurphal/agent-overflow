package claude

import (
	"strings"
	"unicode"
)

// peername.go — the peer-visible session name.
//
// When cross-session messaging is on, every Claude session registers in
// `<CLAUDE_CONFIG_DIR>/sessions/<pid>.json` under a display name, and that
// name is the ADDRESS: a peer's `SendMessage` takes a `to` naming it, and
// `ListAgents` is how it was found. The name arrives either as `--name`
// or as CLAUDE_CODE_SESSION_NAME; Agent Overflow uses the flag.
//
// The default, if neither is given, is the session's cwd BASENAME — which
// for Agent Overflow would name every thread of a project identically, so
// a distinct name is not a nicety here.

// MaxPeerSessionNameRunes is the CLI's own cap, applied in CODE POINTS.
//
// Verified two ways against 2.1.237: the bundle's sanitizer is
// `[...s.replace(/[\x00-\x1f\x7f-\x9f]/g,"")].slice(0,200).join("")` —
// a spread, so the slice counts code points, not UTF-16 units and not
// bytes — and a live spawn with a 260-character `--name` registered
// exactly 200 (spike 2026-08-21, /tmp/spike-xsession/logs/q6).
const MaxPeerSessionNameRunes = 200

// SanitizePeerSessionName mirrors the CLI's own `--name` /
// CLAUDE_CODE_SESSION_NAME normalizer so Agent Overflow knows what name a
// session will actually answer to, rather than what it asked for.
//
// Mirroring rather than merely bounding matters because the name is an
// ADDRESS. AO records the name it intends and compares it against the
// name it would send next (`/rename` is skipped when they match); if the
// two normalizers disagreed, every reconcile would see a difference and
// re-send a rename that changes nothing.
//
// The CLI's pipeline, in order, and reproduced here exactly:
//
//  1. `trim()`.
//  2. Replace every RUN of Unicode Cc (control), Cf (format — this is the
//     one that catches zero-width spaces and bidi marks), U+2028 and
//     U+2029 with a SINGLE space. A run collapses to one space, which is
//     why this is not a per-rune map.
//  3. Delete any remaining `\x00-\x1f\x7f-\x9f`. Redundant after step 2
//     for well-formed input and kept because the CLI keeps it.
//  4. Truncate to MaxPeerSessionNameRunes code points.
//  5. `trim()` again — so a truncation that lands mid-space does not
//     register a trailing blank.
//
// An empty result is meaningful and is returned as such: the CLI treats
// an empty `--name` as ABSENT and falls back to the cwd basename, and
// `/rename` with an empty argument answers "That name is empty once
// invisible characters are removed" without changing anything. Callers
// must decide what to do with "" rather than send it.
func SanitizePeerSessionName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	inRun := false
	for _, r := range strings.TrimSpace(name) {
		if isPeerNameSeparatorRune(r) {
			// Collapse the RUN, not each rune — matches the CLI's `+` regex
			// quantifier. Leading runs are already gone via the trim above.
			if !inRun {
				b.WriteRune(' ')
				inRun = true
			}
			continue
		}
		inRun = false
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			// The CLI's second pass. Unreachable for anything the first
			// pass already classified, kept so the mirror stays a mirror.
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(truncateRunes(b.String(), MaxPeerSessionNameRunes))
}

// PeerSessionNameUsableAsArg reports whether a SANITIZED peer name can be
// handed to the CLI as the value of `--name`.
//
// Two names the CLI's own normalizer accepts are unusable as argv: the empty
// string (read as ABSENT — the CLI falls back to the cwd basename, which is
// the very collision the peer name exists to avoid) and one starting with a
// dash, which the argument parser reads as a FLAG rather than as this flag's
// value. A thread titled "-p" would therefore turn `--name -p` into a
// different CLI invocation entirely. Same hazard, same rule, and the same
// reasoning as SanitizeDisallowedTools' leading-dash drop.
//
// Exported so the layer that DERIVES a name (the app's
// peerSessionNameForThread) can fall back to its structural
// `<project>/<short id>` form instead of silently losing the flag at the
// argv boundary — the boundary still refuses, so the guarantee does not
// depend on that caller.
func PeerSessionNameUsableAsArg(name string) bool {
	return name != "" && !strings.HasPrefix(name, "-")
}

// isPeerNameSeparatorRune matches the CLI's `[\p{Cc}\p{Cf}\u2028\u2029]`
// class. Go's unicode.Cc / unicode.Cf are the same tables the JS `\p{...}`
// escapes resolve against.
//
// The two line separators are written as ESCAPES, never as literals: raw
// U+2028 / U+2029 in source are invisible in every editor and diff, so a
// literal here is indistinguishable from a plain space to anyone reviewing
// this mirror against the CLI's regex.
func isPeerNameSeparatorRune(r rune) bool {
	if r == '\u2028' || r == '\u2029' {
		return true
	}
	return unicode.In(r, unicode.Cc, unicode.Cf)
}

// truncateRunes cuts to at most n code points without splitting one.
//
// Unlike `truncate` (json_helpers.go) it appends NO ellipsis: this cut
// mirrors the CLI's own `.slice(0, 200)`, and a trailing "…" would make the
// name AO records differ from the name the session registers — which is the
// one thing this whole file exists to keep identical.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
