package appidentity

import (
	"fmt"
	"path"
	"strings"
)

// ModeDev / ModeProd are the two build-stamped launcher modes (see
// cmd/agent-overflow-windows/main.go `launcherMode`). ModeHarness and
// ModeSoak are RUNTIME modes: the same .exe enters one when the operator
// passes --profile, and neither may ever be a build stamp — such a build
// would be indistinguishable from the dev build the developer runs their
// real work in.
const (
	ModeDev     = "dev"
	ModeProd    = "prod"
	ModeHarness = "harness"
	ModeSoak    = "soak"
)

// The two non-default launch profiles, each a fully isolated second
// instance of the real app (mocked providers, own data dir):
//
//   - ProfileHarness is THE isolated instance an agent or a developer
//     drives — the Windows-shell twin of `make harness`. It boots and
//     then does nothing on its own; whoever wants a scenario drives it
//     (bin/ao-harness, Playwright, or a human at the window).
//   - ProfileSoak is that same instance plus ONE preset: the soak
//     autopilot, which seeds two threads and starts a never-ending
//     streaming turn so a renderer/hang reproduction can be left running
//     for hours (docs/architecture/soak-rig.md).
//
// Every piece of per-instance state the launcher owns is named through
// the helpers below, so neither profile can be rejected by — or reach
// into — the developer's primary instance, or each other's.
const (
	ProfileHarness = "harness"
	ProfileSoak    = "soak"
)

// NormalizeProfile validates a --profile / AGENT_OVERFLOW_PROFILE value.
// Empty (the default, unchanged behaviour), "harness" and "soak" are the
// only accepted values; anything else is an error rather than a silent
// fallback, because a typo'd profile that quietly resolves to the
// default would run an isolated instance against the developer's real
// instance state — the exact outcome the axis exists to prevent.
func NormalizeProfile(raw string) (string, error) {
	profile := strings.ToLower(strings.TrimSpace(raw))
	switch profile {
	case "", ProfileHarness, ProfileSoak:
		return profile, nil
	default:
		return "", fmt.Errorf("unknown profile %q (valid: %q, %q)", raw, ProfileHarness, ProfileSoak)
	}
}

// LauncherMode folds the build-stamped mode with the runtime profile
// into the single string every per-instance name is derived from. The
// profile wins: an isolated instance launched from a dev build is still
// that profile's instance, and must not share the dev instance's
// identity, WebView2 profile, log, or window state.
func LauncherMode(buildMode, profile string) string {
	switch profile {
	case ProfileHarness:
		return ModeHarness
	case ProfileSoak:
		return ModeSoak
	}
	if buildMode == ModeDev {
		return ModeDev
	}
	return ModeProd
}

// isolatedMode reports whether a folded mode names one of the isolated
// profile instances. It is the one predicate the per-instance name
// helpers below branch on, so adding a profile cannot leave one of them
// silently answering with the developer's own name.
func isolatedMode(mode string) bool {
	return mode == ModeHarness || mode == ModeSoak
}

// StateFileName suffixes a launcher-owned state file name for the given
// mode. Only the isolated profile modes get their own file: dev and prod
// deliberately share `launcher.log` / `window.json` (a developer expects
// one log and one remembered window placement), while an isolated
// instance must leave both untouched — its log is the evidence surface
// for a watchdog episode and has to be attributable to one run, and its
// window placement must not overwrite where the real app reopens.
//
//	StateFileName("launcher.log", "soak")    == "launcher-soak.log"
//	StateFileName("launcher.log", "harness") == "launcher-harness.log"
//	StateFileName("launcher.log", "dev")     == "launcher.log"
func StateFileName(base, mode string) string {
	if !isolatedMode(mode) {
		return base
	}
	ext := path.Ext(base)
	return strings.TrimSuffix(base, ext) + "-" + mode + ext
}

// WebviewProfileDir names the WebView2 user-data folder (relative to the
// app's %APPDATA% directory) for the given mode. Four separate profiles:
// an isolated instance must not share cache, cookies, localStorage, the
// IndexedDB thread replica, or the Crashpad/chrome_debug.log evidence
// trail with the dev instance, the production instance, or the other
// isolated profile.
func WebviewProfileDir(mode string) string {
	switch mode {
	case ModeDev:
		return "webview2-dev"
	case ModeHarness:
		return "webview2-harness"
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
// Split per mode for the same reason as WebviewProfileDir: a dump is
// evidence, and evidence must be attributable to the instance that
// produced it (a soak-rig hang and a real one are different findings).
func RenderForensicsDir(mode string) string {
	switch mode {
	case ModeDev:
		return "render-forensics-dev"
	case ModeHarness:
		return "render-forensics-harness"
	case ModeSoak:
		return "render-forensics-soak"
	default:
		return "render-forensics"
	}
}

// DevToolsPort is the loopback CDP port the WebView2 exposes for a mode,
// or 0 when it must not expose one at all (production — the protocol is
// unauthenticated). Dev, harness and soak get DIFFERENT ports on
// purpose: all three instances can be up at once, and two WebView2s
// asked for the same remote-debugging port would leave whichever lost
// the bind unattachable with no diagnostic.
func DevToolsPort(mode string) int {
	switch mode {
	case ModeDev:
		return 9223
	case ModeSoak:
		return 9224
	case ModeHarness:
		return 9225
	default:
		return 0
	}
}
