// Package editor owns "open file in IDE" detection and spawn for the
// open-in-editor binding. It is provider-agnostic: there is one
// well-known set of editors (VS Code family, Cursor, Windsurf, Sublime,
// Zed) plus the user's $EDITOR / $VISUAL fallback, and one launch
// pipeline (detect → resolve preference → spawn detached).
//
// The WSL bridge lives here too: when the backend runs inside WSL,
// editor detection prefers the Windows-installed app reachable via the
// vendor's WSL bridge over any Linux-native install. Linux-native
// installs (e.g. `apt install code-oss`) are deliberately treated as
// "no editor available" because they would render via WSLg and miss
// the user's actual extensions / sync / shell integration.
package editor
