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
// Every OSC (`ESC]...ST`) and APC (`ESC_...ST`) sequence is stripped before
// rendering. terminal-to-html happily emits live `<a href>` from OSC 8 and
// OSC 1339 and live `<img src>` from OSC 1337 / 1338, and its URL sanitizer
// only blocks the `javascript:` scheme — `vbscript:`, `data:text/html`,
// `data:image/svg+xml`, and `file:` all survive. That would let untrusted
// tool output smuggle dangerous URIs through the ANSI path into {@html}.
// The app does not rely on any OSC/APC-driven terminal feature, so
// stripping them wholesale is lossless for display and closes every
// cross-scheme vector in one pass.
func (r *Renderer) renderANSI(s string) string {
	if len(s) > r.maxBytes {
		return fallbackPreCode(s)
	}
	return string(terminal.Render([]byte(stripUnsafeEscapes(s))))
}

// stripUnsafeEscapes removes OSC and APC envelopes while leaving the
// enclosed visible text (outside the envelope) in place. Both sequence
// kinds share the same shape:
//
//	ESC ] params ST     (OSC — 0x1b 0x5d ... terminator)
//	ESC _ params ST     (APC — 0x1b 0x5f ... terminator)
//
// where ST is either `ESC \` or the single-byte `BEL` (0x07). Anything
// outside the envelope — including SGR sequences (`ESC [ ... m`), the
// bytes terminal-to-html renders into color spans — passes through
// unchanged.
func stripUnsafeEscapes(s string) string {
	if len(s) == 0 {
		return s
	}
	out := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == 0x1b && (s[i+1] == ']' || s[i+1] == '_') {
			end := findOSCEnd(s, i+2)
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

// findOSCEnd returns the index immediately after the OSC / APC string
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
