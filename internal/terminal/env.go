//go:build !windows

package terminal

import (
	"os"
	"slices"
	"strings"

	"agent-overflow/internal/appimage"
)

// terminalCapabilityVars are replaced rather than inherited — see
// normalizeTerminalEnv.
var terminalCapabilityVars = []string{"TERM", "COLORTERM"}

// normalizeTerminalEnv returns a private environment slice describing the
// PTY xterm.js renders, with the launching process's own packaging
// artifacts removed.
//
// Terminal capabilities: desktop app launches commonly inherit no TERM (or
// TERM=dumb), especially from Finder/launchd on macOS; interactive shells
// then disable the cursor movement and line-clearing sequences their editors
// need, leaving stale prompt glyphs smeared across the screen. Always
// replacing these two keys is intentional — values inherited from the process
// that launched Agent Overflow (for example screen-256color from tmux)
// describe that parent terminal, not this PTY, and xterm.js supports the
// xterm-256color capability set and true color on every platform where this
// POSIX implementation runs.
//
// AppImage: the packaging artifacts are removed by internal/appimage, which
// is the same scrub every other process Agent Overflow spawns gets. It is
// marker-gated, so a dev, .deb, or macOS launch comes back byte-identical.
//
// The Windows launcher uses process_windows.go and never reaches this path;
// its terminals run in the WSL-side Linux backend.
func normalizeTerminalEnv(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	base = appimage.Scrub(base)

	out := make([]string, 0, len(base)+2)
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && slices.Contains(terminalCapabilityVars, key) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "TERM=xterm-256color", "COLORTERM=truecolor")
}
