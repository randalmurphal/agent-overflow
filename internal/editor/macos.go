package editor

import (
	"path"
	"runtime"
)

// macosAppRoots are the directories where macOS installs IDE bundles.
// `/Applications` for system-wide installs (drag-drop or signed
// installer), and `~/Applications` for user-only installs that don't
// require admin rights. We probe both, system-wide first to match the
// drag-drop default the average user picks.
//
// homeRelative entries are joined to the running user's home dir at
// detection time. Empty entries pin a system-wide path.
var macosAppRoots = []macosAppRoot{
	{system: "/Applications", homeRelative: ""},
	{homeRelative: "Applications"},
}

type macosAppRoot struct {
	system       string
	homeRelative string
}

// macosInstallTable maps editorID to the bundle-relative paths the
// editor's CLI binary lives at. Sourced from each vendor's
// "shell command from PATH" instructions plus inspecting the
// installed bundles.
//
// Editors missing from this table have no macOS .app fallback and
// rely on PATH only. Add a row when supporting a new IDE that ships
// as a .app bundle.
var macosInstallTable = map[string][]string{
	"code": {
		"Visual Studio Code.app/Contents/Resources/app/bin/code",
	},
	"code-insiders": {
		"Visual Studio Code - Insiders.app/Contents/Resources/app/bin/code",
	},
	"cursor": {
		"Cursor.app/Contents/Resources/app/bin/cursor",
	},
	"windsurf": {
		"Windsurf.app/Contents/Resources/app/bin/windsurf",
	},
	"codium": {
		"VSCodium.app/Contents/Resources/app/bin/codium",
	},
	"subl": {
		"Sublime Text.app/Contents/SharedSupport/bin/subl",
	},
	"zed": {
		"Zed.app/Contents/MacOS/cli",
		"Zed Preview.app/Contents/MacOS/cli",
	},
}

// findMacOSInstall walks the standard macOS app roots looking for an
// install of editorID. Returns the absolute path on success or "" on
// miss. Detection order: every entry in macosAppRoots, in order; for
// each root, every relative path in macosInstallTable[editorID].
//
// The function is a no-op (returns "") when not running on darwin so
// callers can invoke it unconditionally without a runtime.GOOS check.
func findMacOSInstall(editorID string, env detectEnv) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	rels, ok := macosInstallTable[editorID]
	if !ok {
		return ""
	}
	homeDir, _ := env.envValue("HOME")
	for _, root := range macosAppRoots {
		base := root.system
		if base == "" {
			if homeDir == "" {
				continue
			}
			base = path.Join(homeDir, root.homeRelative)
		}
		for _, rel := range rels {
			candidate := path.Join(base, rel)
			if ok, err := env.stat(candidate); err == nil && ok {
				return candidate
			}
		}
	}
	return ""
}
