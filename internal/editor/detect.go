package editor

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"
)

// LaunchStyle picks the argv shape the editor expects when opening a
// path with cursor placement.
type LaunchStyle int

const (
	// LaunchStyleDirectPath passes the path as the only argument. Used
	// for editors that ignore (or don't have) line:col addressing —
	// Cursor's CLI, the generic $EDITOR fallback, etc.
	LaunchStyleDirectPath LaunchStyle = iota

	// LaunchStyleGoto is the VS Code family form: a single argument
	// `--goto path:line:col`.
	LaunchStyleGoto

	// LaunchStylePathLineColumn appends the position to the path
	// itself: `path:line:column`. Sublime Text ("Filenames may be
	// given a :line or :line:column suffix", sublimetext.com/docs/
	// command_line.html) and Zed (crates/cli: "Use `path:line:column`
	// syntax to open a file at the given line and column") both take
	// this form; neither has --line/--column flags.
	LaunchStylePathLineColumn
)

// Editor describes one candidate IDE. Available is populated by
// DetectEditors; ResolvedPath is the absolute path to the binary
// detection found (a PATH lookup, a /mnt/c walk, etc.).
//
// EnvFallback marks the synthetic "$EDITOR / $VISUAL" entry so
// preference resolution can treat it as a final fallback rather than a
// peer of the named editors. The Command field on that entry is the
// exact env-var value the user supplied — already PATH-resolved.
type Editor struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Command      string      `json:"command"`
	LaunchStyle  LaunchStyle `json:"launchStyle"`
	Available    bool        `json:"available"`
	ResolvedPath string      `json:"resolvedPath,omitempty"`
	EnvFallback  bool        `json:"envFallback,omitempty"`
}

// editorCatalog enumerates the editors the open-in-editor flow knows
// about. The slice order doubles as the priority list resolution falls
// back to when the user has not picked a preferred editor — VS Code
// first because it's the most common WSL bridge, then the rest of the
// VS Code family, then Cursor / Windsurf, then Sublime / Zed.
var editorCatalog = []Editor{
	{
		ID:          "code",
		Name:        "VS Code",
		Command:     "code",
		LaunchStyle: LaunchStyleGoto,
	},
	{
		ID:          "code-insiders",
		Name:        "VS Code Insiders",
		Command:     "code-insiders",
		LaunchStyle: LaunchStyleGoto,
	},
	{
		ID:          "cursor",
		Name:        "Cursor",
		Command:     "cursor",
		LaunchStyle: LaunchStyleGoto,
	},
	{
		ID:          "windsurf",
		Name:        "Windsurf",
		Command:     "windsurf",
		LaunchStyle: LaunchStyleGoto,
	},
	{
		ID:          "codium",
		Name:        "VSCodium",
		Command:     "codium",
		LaunchStyle: LaunchStyleGoto,
	},
	{
		ID:          "subl",
		Name:        "Sublime Text",
		Command:     "subl",
		LaunchStyle: LaunchStylePathLineColumn,
	},
	{
		ID:          "zed",
		Name:        "Zed",
		Command:     "zed",
		LaunchStyle: LaunchStylePathLineColumn,
	},
}

// EditorCatalog returns a fresh copy of the supported editor list.
// Useful for tests and call sites that need the pristine catalog
// without going through detection. The slice and elements are
// independent of the package-level fixture so callers can mutate
// freely.
func EditorCatalog() []Editor {
	out := make([]Editor, len(editorCatalog))
	copy(out, editorCatalog)
	return out
}

// detectEnv groups the injection seams DetectEditors uses so tests can
// fake PATH lookups and the WSL filesystem without touching real os
// state. Production calls leave every field nil and DetectEditors
// substitutes the live os.* implementations.
type detectEnv struct {
	// lookPath stands in for exec.LookPath. Tests inject a canned
	// table so a "code on PATH" scenario can pretend the binary
	// exists without writing files into the test process's PATH dirs.
	lookPath func(name string) (string, bool)
	// readFile substitutes os.ReadFile for the WSL detection (reads
	// /proc/sys/kernel/osrelease) and the shim-script content sniff.
	readFile func(path string) ([]byte, error)
	// readDir substitutes os.ReadDir for the /mnt/c/Users walk.
	readDir func(path string) ([]string, error)
	// stat substitutes os.Stat for the /mnt/c install-path probes.
	stat func(path string) (bool, error)
	// envValue substitutes os.LookupEnv. Used for WSL_DISTRO_NAME
	// (mapped via wsl.go) and for $EDITOR / $VISUAL fallbacks.
	envValue func(name string) (string, bool)
}

// detectionCacheTTL is how long DetectEditors will reuse a cached
// detection result before re-walking PATH and /mnt/c. Editors don't
// install themselves between clicks, but a fresh install during a
// running session should surface within a reasonable window.
//
// On WSL each detection run crosses 9P (PATH lookups, /mnt/c stats,
// shim-content reads) at ~10-30ms each — clicking through 20 path
// links in a chat would otherwise re-walk synchronously per click. A
// 60s TTL trades a little staleness for a 20x reduction in WSL probes.
const detectionCacheTTL = 60 * time.Second

// detectionCache carries the most recent DetectEditors result and its
// timestamp. mu is held for both read and write because the cached
// slice is shared by value-but-not-deep-copied — we hand out a copy to
// each caller, but the read needs to be coherent with concurrent
// writes from a Refresh that's mid-rewrite.
//
// The cache is process-global rather than tied to a service struct
// because DetectEditors is called from multiple App methods (Open,
// ListAvailable) and threading a *cache parameter through the public
// surface would be ceremony for no benefit. The mutex contention is
// negligible (this is a 60-second-stale cache, not a hot path).
var detectionCache struct {
	mu        sync.Mutex
	editors   []Editor
	timestamp time.Time
}

// DetectEditors returns every supported editor with Available
// populated. Available editors carry a ResolvedPath the spawn step can
// hand to exec.Command; unavailable editors keep ResolvedPath empty.
//
// Detection order per editor:
//
//  1. exec.LookPath (the boring case — editor on PATH).
//  2. On WSL: walk well-known /mnt/c install locations and accept the
//     first hit. This handles distros where the Windows PATH is not
//     forwarded into the Linux shell.
//
// On WSL we additionally re-check shims found via PATH: a `code`
// binary that does NOT eventually exec a `/mnt/c/...` Windows target
// is treated as unavailable. This is the load-bearing rule from the
// WSL editor-bridge feedback memory — a Linux-native install (e.g.
// apt-installed `code-oss`) would render via WSLg and miss the user's
// actual editor environment.
//
// The synthetic $EDITOR / $VISUAL entry is appended last so the
// preference resolver can treat it as a final fallback even when the
// catalog has nothing available.
//
// Results are cached for detectionCacheTTL — see the cache var above
// for the rationale. RefreshEditors invalidates the cache; the
// settings UI calls it after a deliberate user pick so the picker
// surfaces fresh state instead of stale entries.
func DetectEditors(ctx context.Context) []Editor {
	if cached, ok := readDetectionCache(); ok {
		return cached
	}
	fresh := detectEditorsWithEnv(ctx, liveDetectEnv())
	storeDetectionCache(fresh)
	return fresh
}

// RefreshEditors clears the cached DetectEditors result so the next
// call re-walks PATH and /mnt/c. Used by SetEditorSettings so a user
// who flips the preference sees fresh availability state for the
// picker, not whatever was cached before they made the change.
func RefreshEditors() {
	detectionCache.mu.Lock()
	defer detectionCache.mu.Unlock()
	detectionCache.editors = nil
	detectionCache.timestamp = time.Time{}
}

// readDetectionCache returns the cached editors if they are within TTL.
// Returns a defensive copy so the caller can mutate freely without
// corrupting the cached slice.
func readDetectionCache() ([]Editor, bool) {
	detectionCache.mu.Lock()
	defer detectionCache.mu.Unlock()
	if detectionCache.editors == nil {
		return nil, false
	}
	if time.Since(detectionCache.timestamp) > detectionCacheTTL {
		return nil, false
	}
	out := make([]Editor, len(detectionCache.editors))
	copy(out, detectionCache.editors)
	return out, true
}

// PeekDetectionCache is the cross-package read accessor for the
// detection cache. Used by App-level tests that need to assert on the
// cache invalidation hook without depending on RefreshEditors timing.
// Production code should not call this — it has no use outside test
// code; the cache is an implementation detail of DetectEditors.
//
// Returns the same defensive copy + ok bool as readDetectionCache so
// callers can introspect cache state without learning the internal lock.
func PeekDetectionCache() ([]Editor, bool) {
	return readDetectionCache()
}

// storeDetectionCache replaces the cache with a defensive copy of
// editors so a downstream mutation can't corrupt the cached slice.
func storeDetectionCache(editors []Editor) {
	detectionCache.mu.Lock()
	defer detectionCache.mu.Unlock()
	detectionCache.editors = make([]Editor, len(editors))
	copy(detectionCache.editors, editors)
	detectionCache.timestamp = time.Now()
}

// liveDetectEnv builds a detectEnv backed by the real os package.
// Split out so tests can construct a fake env without depending on
// reflection or exported indirection.
func liveDetectEnv() detectEnv {
	return detectEnv{
		lookPath: func(name string) (string, bool) {
			path, err := lookPath(name)
			if err != nil {
				return "", false
			}
			return path, true
		},
		readFile: os.ReadFile,
		readDir: func(path string) ([]string, error) {
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(entries))
			for _, e := range entries {
				out = append(out, e.Name())
			}
			return out, nil
		},
		stat: func(path string) (bool, error) {
			_, err := os.Stat(path)
			if err == nil {
				return true, nil
			}
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		},
		envValue: os.LookupEnv,
	}
}

func detectEditorsWithEnv(ctx context.Context, env detectEnv) []Editor {
	wsl := isWSLEnv(env)
	out := make([]Editor, 0, len(editorCatalog)+1)
	for _, e := range editorCatalog {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return out
			}
		}
		out = append(out, detectOne(e, wsl, env))
	}
	if envEditor, ok := detectEnvEditor(env); ok {
		out = append(out, envEditor)
	}
	return out
}

// detectOne resolves a single editor. The WSL branch is intentionally
// stricter than PATH lookup — see DetectEditors' contract.
//
// Detection order, after PATH:
//   - WSL: walk /mnt/c install table only. A Linux-native install
//     inside WSL is deliberately rejected so the user gets the
//     Remote-WSL setup hint rather than a wrong editor environment.
//   - macOS (non-WSL): walk standard .app bundle locations under
//     /Applications and ~/Applications.
//   - Linux (non-WSL): walk well-known system + snap + flatpak +
//     ~/.local/bin paths.
//
// On WSL, a PATH-resolved shim must additionally pass
// validateWindowsCodeShim. PATH order on Windows-shadowed environments
// often lists a stale system install ahead of a working user install;
// without validation we'd accept the broken candidate, exec it, and
// the editor would never appear (the shim suppresses its own stderr,
// so the spawn looks successful).
func detectOne(e Editor, wsl bool, env detectEnv) Editor {
	resolved, ok := env.lookPath(e.Command)
	if ok {
		if !wsl {
			e.Available = true
			e.ResolvedPath = resolved
			return e
		}
		if pathTargetsWindows(resolved, env) && validateWindowsCodeShim(resolved, env) {
			e.Available = true
			e.ResolvedPath = resolved
			return e
		}
		// WSL + Linux-native install OR a /mnt/c shim whose VERSIONFOLDER
		// target is missing: fall through to /mnt/c discovery so we
		// either find a real working editor or report "not available".
	}
	if wsl {
		if winPath := findWindowsInstall(e.ID, env); winPath != "" {
			e.Available = true
			e.ResolvedPath = winPath
			return e
		}
		return e
	}
	if macPath := findMacOSInstall(e.ID, env); macPath != "" {
		e.Available = true
		e.ResolvedPath = macPath
		return e
	}
	if linuxPath := findLinuxInstall(e.ID, env); linuxPath != "" {
		e.Available = true
		e.ResolvedPath = linuxPath
		return e
	}
	return e
}

// detectEnvEditor turns an $EDITOR / $VISUAL value into an Editor
// entry. Visual is checked first because POSIX convention favours it
// for full-screen editors (which is what we want — vi vs vim). The
// returned entry is marked EnvFallback so preference resolution can
// rank it after the named catalog without a special case.
//
// We intentionally do not parse arguments out of $EDITOR ("code -w"):
// shell-quoting rules differ across platforms, and a user who set a
// multi-word EDITOR is doing something exotic enough that the boring
// case (just the binary) is the right default.
func detectEnvEditor(env detectEnv) (Editor, bool) {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		raw, ok := env.envValue(name)
		if !ok {
			continue
		}
		cmd := strings.TrimSpace(raw)
		if cmd == "" {
			continue
		}
		path, ok := env.lookPath(cmd)
		if !ok {
			continue
		}
		return Editor{
			ID:           "env:" + strings.ToLower(name),
			Name:         "$" + name,
			Command:      cmd,
			LaunchStyle:  LaunchStyleDirectPath,
			Available:    true,
			ResolvedPath: path,
			EnvFallback:  true,
		}, true
	}
	return Editor{}, false
}
