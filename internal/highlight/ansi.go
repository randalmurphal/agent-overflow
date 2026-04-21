package highlight

import (
	terminal "github.com/buildkite/terminal-to-html/v3"
)

// renderANSI converts terminal output to HTML. terminal-to-html escapes any
// literal HTML it encounters (so injected <script> tags end up as text, not
// markup), drops partial escape sequences at end-of-input without crashing,
// and emits stable classes (term-fg31, term-bg41, term-fg1, etc.) that the
// frontend styles via CSS.
//
// We apply the same MaxBytes cap as markdown: oversized inputs fall through
// to an HTML-escaped <pre><code> block so a runaway command output can't
// pin the renderer.
//
// OSC 8 hyperlinks (`\x1b]8;;URL\x1b\\text\x1b]8;;\x1b\\`) are stripped
// before rendering. terminal-to-html happily emits live <a href> from them,
// which would let untrusted tool output smuggle `javascript:` / `data:` URIs
// through the ANSI path into {@html}. We strip only the escape pair; the
// text between pairs is preserved so the user still sees the visible
// content.
func (r *Renderer) renderANSI(s string) string {
	if len(s) > r.maxBytes {
		return fallbackPreCode(s)
	}
	return string(terminal.Render([]byte(stripOSC8(s))))
}

// stripOSC8 removes OSC 8 hyperlink introducers/terminators while leaving
// the enclosed visible text in place. An OSC 8 sequence is
//
//	ESC ] 8 ; params ; URI ST
//
// where ST (the string terminator) is either `ESC \` or the single-byte
// `BEL` (0x07). We strip introducers (with URI) and terminators (with
// empty URI). Parameters and URI bytes inside the envelope are dropped;
// anything outside the envelope passes through unchanged so legitimate
// terminal color sequences still reach terminal-to-html.
func stripOSC8(s string) string {
	if len(s) == 0 {
		return s
	}
	const prefix = "\x1b]8;"
	out := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		if i+len(prefix) <= len(s) && s[i:i+len(prefix)] == prefix {
			end := findOSCEnd(s, i+len(prefix))
			if end < 0 {
				// No terminator before end-of-input; drop the partial
				// sequence entirely rather than passing its bytes
				// through (matches terminal-to-html's own end-of-input
				// discard for partial escapes).
				return string(out)
			}
			i = end
			continue
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}

// findOSCEnd returns the index immediately after the OSC string
// terminator (BEL or ESC\) starting the scan at `from`, or -1 if the
// sequence runs past the end of s without terminating.
func findOSCEnd(s string, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == 0x07 {
			return i + 1
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
			return i + 2
		}
	}
	return -1
}
