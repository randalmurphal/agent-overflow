package editor

import (
	"bytes"
	"path"
	"strings"
	"sync"
)

// wslOSReleasePath is the kernel-reported runtime identifier on Linux.
// On WSL this string contains "microsoft" (case-insensitive); on
// native Linux it does not. The path is exposed as a package var so
// tests can swap it without touching the production lookup.
const wslOSReleasePath = "/proc/sys/kernel/osrelease"

// wslMntCRoot is the canonical path the Windows C:\ drive is mounted
// at inside WSL. Default mount point per Microsoft docs; users who
// remap their drives will need to set the editor binary path
// explicitly via $PATH or settings.
const wslMntCRoot = "/mnt/c"

// wslUsersSkip enumerates Windows /mnt/c/Users entries that don't
// represent real user profiles. Probing them would either fail (Default
// User is a junction with restrictive ACLs) or hit cached profile
// templates, neither of which contains a usable editor install.
var wslUsersSkip = map[string]struct{}{
	"Default":      {},
	"Public":       {},
	"All Users":    {},
	"Default User": {},
}

// wslInstallPath maps a catalog editor ID to the relative path under a
// Windows user profile (or absolute system path) where the vendor
// installs the WSL bridge CLI. The user-relative path is preferred
// because a per-user install is current; the system paths are
// fallbacks for shared installs.
type wslInstallPath struct {
	// userRelative is appended to /mnt/c/Users/<user>/.
	userRelative string
	// systemPaths are absolute /mnt/c paths to check after the user
	// profiles. Empty means "no known system install location".
	systemPaths []string
}

// wslInstallTable holds the well-known Windows install locations for
// each editor we detect on WSL. Sourced from forge's open.ts plus
// CodexMonitor's Tauri bridge — these paths match what the vendors
// install during a default Windows setup. Editors missing from this
// table have no /mnt/c fallback and rely on PATH only.
var wslInstallTable = map[string]wslInstallPath{
	"code": {
		userRelative: "AppData/Local/Programs/Microsoft VS Code/bin/code",
		systemPaths:  []string{"/mnt/c/Program Files/Microsoft VS Code/bin/code"},
	},
	"code-insiders": {
		userRelative: "AppData/Local/Programs/Microsoft VS Code Insiders/bin/code-insiders",
		systemPaths:  []string{"/mnt/c/Program Files/Microsoft VS Code Insiders/bin/code-insiders"},
	},
	"cursor": {
		userRelative: "AppData/Local/Programs/cursor/resources/app/bin/cursor",
		systemPaths:  []string{"/mnt/c/Program Files/Cursor/resources/app/bin/cursor"},
	},
	"windsurf": {
		userRelative: "AppData/Local/Programs/Windsurf/bin/windsurf",
		systemPaths:  []string{"/mnt/c/Program Files/Windsurf/bin/windsurf"},
	},
	"codium": {
		userRelative: "AppData/Local/Programs/VSCodium/bin/codium",
		systemPaths:  []string{"/mnt/c/Program Files/VSCodium/bin/codium"},
	},
}

// wslDetectionOnce caches the live /proc lookup. WSL membership doesn't
// change at runtime, so a single read is fine — the cache prevents the
// repeated detection calls (one per editor in the catalog) from each
// stat'ing /proc.
var (
	wslDetectionOnce  sync.Once
	wslDetectionValue bool
)

// IsWSL reports whether the running process is inside a WSL
// distribution. The result is cached after the first call.
func IsWSL() bool {
	wslDetectionOnce.Do(func() {
		wslDetectionValue = readWSLOSRelease(liveDetectEnv())
	})
	return wslDetectionValue
}

// isWSLEnv is the test-friendly variant: it reads through the supplied
// env so each test can inject its own /proc fixture without poisoning
// the sync.Once cache.
func isWSLEnv(env detectEnv) bool {
	if env.readFile == nil {
		return IsWSL()
	}
	return readWSLOSRelease(env)
}

func readWSLOSRelease(env detectEnv) bool {
	data, err := env.readFile(wslOSReleasePath)
	if err != nil {
		return false
	}
	return bytes.Contains(bytes.ToLower(data), []byte("microsoft"))
}

// findWindowsInstall walks /mnt/c looking for a Windows-side install
// of editorID. Returns the absolute Linux-side path on success or "" on
// miss. Per the WSL editor-bridge feedback memory, the user-install
// path takes precedence over the system one — a per-user install
// reflects the user's actual editor environment.
func findWindowsInstall(editorID string, env detectEnv) string {
	entry, ok := wslInstallTable[editorID]
	if !ok {
		return ""
	}

	usersDir := path.Join(wslMntCRoot, "Users")
	if names, err := env.readDir(usersDir); err == nil {
		for _, name := range names {
			if _, skip := wslUsersSkip[name]; skip {
				continue
			}
			candidate := path.Join(usersDir, name, entry.userRelative)
			if ok, err := env.stat(candidate); err == nil && ok {
				return candidate
			}
		}
	}

	for _, sys := range entry.systemPaths {
		if ok, err := env.stat(sys); err == nil && ok {
			return sys
		}
	}
	return ""
}

// pathTargetsWindows reports whether the script content at resolved
// eventually exec's a Windows-side binary under /mnt/c. The Microsoft
// VS Code WSL shim is a small bash script that detects WSL and
// re-invokes the Windows CLI; matching on the literal "/mnt/c" string
// catches that pattern (and the equivalent shims Cursor / Windsurf /
// VSCodium ship). When the resolved path is itself /mnt/c/... we
// short-circuit because that's already the canonical Windows binary.
//
// Read failures (binary file, permission denied) collapse to "not a
// Windows target". The detection is best-effort by design — we'd
// rather report unavailable than misroute to a Linux-native install.
func pathTargetsWindows(resolved string, env detectEnv) bool {
	if strings.HasPrefix(resolved, wslMntCRoot+"/") || resolved == wslMntCRoot {
		return true
	}
	data, err := env.readFile(resolved)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte(wslMntCRoot))
}

// ToWSLUNCPath converts a Linux path to the Windows UNC form
// (\\wsl.localhost\<distro>\<linux-path-with-backslashes>). Used by
// callers that want to hand a path to a Windows-side process that
// can't read /mnt/<drive> mappings — most notably explorer.exe.
//
// distro is read from WSL_DISTRO_NAME; on a non-WSL host the env is
// empty and ToWSLUNCPath returns the input unchanged so callers can
// invoke it unconditionally.
func ToWSLUNCPath(linuxPath string) string {
	return toWSLUNCPathWithEnv(linuxPath, liveDetectEnv())
}

func toWSLUNCPathWithEnv(linuxPath string, env detectEnv) string {
	distro, ok := env.envValue("WSL_DISTRO_NAME")
	if !ok || distro == "" {
		return linuxPath
	}
	windows := strings.ReplaceAll(linuxPath, "/", `\`)
	return `\\wsl.localhost\` + distro + windows
}
