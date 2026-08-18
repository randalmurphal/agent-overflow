package appidentity

import (
	"fmt"
	"path"
	"strings"
)

// ModeDev / ModeProd are the two build-stamped launcher modes (see
// cmd/agent-overflow-windows/main.go `launcherMode`). ModeSoak is a
// RUNTIME mode: the same .exe enters it when the operator passes
// --profile soak, and it must never be a build stamp — a soak build
// would be indistinguishable from the dev build the developer runs
// their real work in.
const (
	ModeDev  = "dev"
	ModeProd = "prod"
	ModeSoak = "soak"
)

// ProfileSoak is the one non-default launch profile: a second, fully
// isolated instance of the real app used for long-running soak
// reproductions (docs/architecture/soak-rig.md). Every piece of
// per-instance state the launcher owns is named through the helpers
// below, so a soak instance cannot be rejected by — or reach into —
// the developer's primary instance.
const ProfileSoak = "soak"

// NormalizeProfile validates a --profile / AGENT_OVERFLOW_PROFILE value.
// Empty (the default, unchanged behaviour) and "soak" are the only
// accepted values; anything else is an error rather than a silent
// fallback, because a typo'd profile that quietly resolves to the
// default would run the soak against the developer's real instance
// state — the exact outcome the axis exists to prevent.
func NormalizeProfile(raw string) (string, error) {
	profile := strings.ToLower(strings.TrimSpace(raw))
	switch profile {
	case "", ProfileSoak:
		return profile, nil
	default:
		return "", fmt.Errorf("unknown profile %q (valid: %q)", raw, ProfileSoak)
	}
}

// LauncherMode folds the build-stamped mode with the runtime profile
// into the single string every per-instance name is derived from. The
// profile wins: a soak launched from a dev build is still a soak, and
// must not share the dev instance's identity, WebView2 profile, log, or
// window state.
func LauncherMode(buildMode, profile string) string {
	if profile == ProfileSoak {
		return ModeSoak
	}
	if buildMode == ModeDev {
		return ModeDev
	}
	return ModeProd
}

// StateFileName suffixes a launcher-owned state file name for the given
// mode. Only the soak mode gets its own file: dev and prod deliberately
// share `launcher.log` / `window.json` (a developer expects one log and
// one remembered window placement), while a soak run must leave both
// untouched — its log is the evidence surface for a watchdog episode and
// has to be attributable to one run, and its 800x600 window must not
// overwrite where the real app reopens.
//
//	StateFileName("launcher.log", "soak") == "launcher-soak.log"
//	StateFileName("launcher.log", "dev")  == "launcher.log"
func StateFileName(base, mode string) string {
	if mode != ModeSoak {
		return base
	}
	ext := path.Ext(base)
	return strings.TrimSuffix(base, ext) + "-" + ModeSoak + ext
}

// WebviewProfileDir names the WebView2 user-data folder (relative to the
// app's %APPDATA% directory) for the given mode. Three separate
// profiles: a soak run must not share cache, cookies, localStorage, the
// IndexedDB thread replica, or the Crashpad/chrome_debug.log evidence
// trail with either the dev or the production instance.
func WebviewProfileDir(mode string) string {
	switch mode {
	case ModeDev:
		return "webview2-dev"
	case ModeSoak:
		return "webview2-soak"
	default:
		return "webview2"
	}
}

// RenderForensicsDir names the directory (relative to the app's
// %APPDATA% directory) where the WebView2 host writes render-hang
// evidence — renderer minidumps and the breadcrumb JSONL the wails
// fork's render watchdog captures at the moment it declares a hang.
// Split three ways for the same reason as WebviewProfileDir: a dump is
// evidence, and evidence must be attributable to the instance that
// produced it (a soak-rig hang and a real one are different findings).
func RenderForensicsDir(mode string) string {
	switch mode {
	case ModeDev:
		return "render-forensics-dev"
	case ModeSoak:
		return "render-forensics-soak"
	default:
		return "render-forensics"
	}
}

// DevToolsPort is the loopback CDP port the WebView2 exposes for a mode,
// or 0 when it must not expose one at all (production — the protocol is
// unauthenticated). Dev and soak get DIFFERENT ports on purpose: both
// instances can be up at once, and two WebView2s asked for the same
// remote-debugging port would leave whichever lost the bind unattachable
// with no diagnostic.
func DevToolsPort(mode string) int {
	switch mode {
	case ModeDev:
		return 9223
	case ModeSoak:
		return 9224
	default:
		return 0
	}
}
