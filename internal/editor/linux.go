package editor

import (
	"path"
	"runtime"
	"strings"
)

// linuxInstallTable lists well-known absolute install locations for
// each editor on Linux distros. PATH lookup catches the common case
// (apt/dpkg installs land /usr/bin/<bin>); this table covers the
// niches where a user's interactive shell PATH doesn't include the
// install location:
//
//   - Snap: /snap/bin is on most user PATHs but not always (login
//     shells without `bash --login`, system services).
//   - Flatpak: not on PATH by default; the wrapper scripts under
//     /var/lib/flatpak/exports/bin/ usually are, but the binary
//     itself lives under the flatpak runtime.
//   - Vendor tarball installs into /opt/<editor>/...
//   - User-local installs into ~/.local/bin and ~/Applications/.
//
// Entries are absolute (system) paths; "~" prefix is expanded against
// $HOME at detection time.
var linuxInstallTable = map[string][]string{
	"code": {
		"/usr/share/code/bin/code",
		"/usr/share/code/code",
		"/snap/bin/code",
		"/var/lib/flatpak/exports/bin/com.visualstudio.code",
		"/opt/visual-studio-code/bin/code",
		"~/.local/bin/code",
	},
	"code-insiders": {
		"/usr/share/code-insiders/bin/code-insiders",
		"/snap/bin/code-insiders",
		"/var/lib/flatpak/exports/bin/com.visualstudio.code.insiders",
		"~/.local/bin/code-insiders",
	},
	"cursor": {
		"/snap/bin/cursor",
		"/opt/cursor/cursor",
		"/opt/cursor/bin/cursor",
		"~/.local/bin/cursor",
		"~/Applications/cursor",
		"~/Applications/Cursor.AppImage",
	},
	"windsurf": {
		"/usr/share/windsurf/bin/windsurf",
		"/snap/bin/windsurf",
		"/opt/windsurf/bin/windsurf",
		"~/.local/bin/windsurf",
	},
	"codium": {
		"/usr/share/codium/bin/codium",
		"/snap/bin/codium",
		"/var/lib/flatpak/exports/bin/com.vscodium.codium",
		"~/.local/bin/codium",
	},
	"subl": {
		"/opt/sublime_text/sublime_text",
		"/snap/bin/subl",
		"~/.local/bin/subl",
	},
	"zed": {
		"/usr/bin/zeditor",
		"/usr/lib/zed-editor/zed-editor",
		"/snap/bin/zed",
		"/var/lib/flatpak/exports/bin/dev.zed.Zed",
		"~/.local/bin/zed",
	},
}

// findLinuxInstall walks the well-known Linux install paths for
// editorID. Returns the absolute path on success or "" on miss.
//
// No-op (returns "") on non-linux hosts so callers can call
// unconditionally. Skipped on WSL — the WSL bridge takes precedence
// per the editor-bridge feedback memory; a Linux-native install
// inside WSL is deliberately unreachable.
func findLinuxInstall(editorID string, env detectEnv) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	rels, ok := linuxInstallTable[editorID]
	if !ok {
		return ""
	}
	homeDir, _ := env.envValue("HOME")
	for _, p := range rels {
		candidate := p
		if strings.HasPrefix(candidate, "~/") {
			if homeDir == "" {
				continue
			}
			candidate = path.Join(homeDir, candidate[2:])
		}
		if ok, err := env.stat(candidate); err == nil && ok {
			return candidate
		}
	}
	return ""
}
