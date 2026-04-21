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
func (r *Renderer) renderANSI(s string) string {
	if len(s) > r.maxBytes {
		return fallbackPreCode(s)
	}
	return string(terminal.Render([]byte(s)))
}
