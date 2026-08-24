//go:build windows

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"
)

// TestExportAppDataToWSL_NoAPPDATA covers the defensive branch: if
// %APPDATA% is unset, the function is a no-op (sentinel logs once).
// The WSL backend's WSLConfigDir() returns ok=false in that case and
// the Settings UI hides the WSL distro switcher.
func TestExportAppDataToWSL_NoAPPDATA(t *testing.T) {
	t.Setenv("APPDATA", "")
	t.Setenv(wsldistro.AppDataEnv, "leftover")
	t.Setenv("WSLENV", "")

	exportAppDataToWSL()

	if got := os.Getenv(wsldistro.AppDataEnv); got != "leftover" {
		t.Errorf("AppDataEnv was clobbered: got %q, want %q (function should no-op)", got, "leftover")
	}
	if got := os.Getenv("WSLENV"); got != "" {
		t.Errorf("WSLENV was clobbered: got %q, want empty", got)
	}
}

// TestExportAppDataToWSL_SetsBoth_NoPriorWSLENV is the cold-start
// path: APPDATA exported to AGENT_OVERFLOW_WIN_APPDATA, WSLENV set to
// the /p translation rule alone.
func TestExportAppDataToWSL_SetsBoth_NoPriorWSLENV(t *testing.T) {
	const want = `C:\Users\dev\AppData\Roaming`
	t.Setenv("APPDATA", want)
	t.Setenv(wsldistro.AppDataEnv, "")
	t.Setenv("WSLENV", "")

	exportAppDataToWSL()

	if got := os.Getenv(wsldistro.AppDataEnv); got != want {
		t.Errorf("AppDataEnv = %q, want %q", got, want)
	}
	wantWSLENV := wsldistro.AppDataEnv + "/p"
	if got := os.Getenv("WSLENV"); got != wantWSLENV {
		t.Errorf("WSLENV = %q, want %q", got, wantWSLENV)
	}
}

// TestExportAppDataToWSL_PrependsToExistingWSLENV pins the merge
// behavior: a developer with an existing WSLENV (e.g. PYTHONPATH/p)
// must keep their rules — the function prepends our entry so we don't
// silently drop another tool's translation.
func TestExportAppDataToWSL_PrependsToExistingWSLENV(t *testing.T) {
	const appdata = `C:\Users\dev\AppData\Roaming`
	const prior = "PYTHONPATH/p:GOPATH/p"
	t.Setenv("APPDATA", appdata)
	t.Setenv(wsldistro.AppDataEnv, "")
	t.Setenv("WSLENV", prior)

	exportAppDataToWSL()

	got := os.Getenv("WSLENV")
	wantPrefix := wsldistro.AppDataEnv + "/p:"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("WSLENV = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, prior) {
		t.Errorf("WSLENV = %q, lost prior rules %q", got, prior)
	}
}

// TestForwardDebugEnvToWSL_Unset_NoOp covers the production default:
// AGENT_OVERFLOW_DEBUG unset, the function must not touch WSLENV.
func TestForwardDebugEnvToWSL_Unset_NoOp(t *testing.T) {
	t.Setenv("AGENT_OVERFLOW_DEBUG", "")
	t.Setenv("WSLENV", "")

	forwardDebugEnvToWSL()

	if got := os.Getenv("WSLENV"); got != "" {
		t.Errorf("WSLENV = %q, want empty (function should no-op when var unset)", got)
	}
}

// TestForwardDebugEnvToWSL_Set_PrependsRule covers the dev-wsl path:
// AGENT_OVERFLOW_DEBUG=provider lands as a string rule in WSLENV so
// wsl.exe forwards it to the Linux backend.
func TestForwardDebugEnvToWSL_Set_PrependsRule(t *testing.T) {
	t.Setenv("AGENT_OVERFLOW_DEBUG", "provider")
	t.Setenv("WSLENV", "")

	forwardDebugEnvToWSL()

	if got := os.Getenv("WSLENV"); got != "AGENT_OVERFLOW_DEBUG" {
		t.Errorf("WSLENV = %q, want %q", got, "AGENT_OVERFLOW_DEBUG")
	}
}

// TestForwardDebugEnvToWSL_PrependsToExistingWSLENV pins co-existence
// with rules already added by exportAppDataToWSL (and any user rules).
// In production main() runs both functions in sequence; this verifies
// the second call doesn't drop the first's rule.
func TestForwardDebugEnvToWSL_PrependsToExistingWSLENV(t *testing.T) {
	const prior = "AGENT_OVERFLOW_WIN_APPDATA/p:PYTHONPATH/p"
	t.Setenv("AGENT_OVERFLOW_DEBUG", "all")
	t.Setenv("WSLENV", prior)

	forwardDebugEnvToWSL()

	got := os.Getenv("WSLENV")
	if !strings.HasPrefix(got, "AGENT_OVERFLOW_DEBUG:") {
		t.Errorf("WSLENV = %q, want prefix %q", got, "AGENT_OVERFLOW_DEBUG:")
	}
	if !strings.Contains(got, prior) {
		t.Errorf("WSLENV = %q, lost prior rules %q", got, prior)
	}
}

func TestOpenLogPermissionConstants(t *testing.T) {
	if launcherPrivateDirPerm != 0o700 {
		t.Fatalf("launcherPrivateDirPerm = %o, want 700", launcherPrivateDirPerm)
	}
	if launcherLogFilePerm != 0o600 {
		t.Fatalf("launcherLogFilePerm = %o, want 600", launcherLogFilePerm)
	}
}

func TestOpenLogCreatesLogUnderAppData(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("APPDATA", appData)

	f, err := openLog("")
	if err != nil {
		t.Fatalf("openLog: %v", err)
	}
	defer f.Close()

	want := filepath.Join(appData, "agent-overflow", "launcher.log")
	if f.Name() != want {
		t.Fatalf("log path = %q, want %q", f.Name(), want)
	}
	if _, err := f.WriteString("hello\n"); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

// TestOpenLogSoakProfileWritesItsOwnFile is the evidence-attribution
// gate: a soak run's watchdog lines have to be greppable on their own,
// not interleaved with every dev launch that shares %APPDATA%.
func TestOpenLogSoakProfileWritesItsOwnFile(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("APPDATA", appData)

	f, err := openLog(appidentity.ProfileSoak)
	if err != nil {
		t.Fatalf("openLog(soak): %v", err)
	}
	defer f.Close()

	want := filepath.Join(appData, "agent-overflow", "launcher-soak.log")
	if f.Name() != want {
		t.Fatalf("soak log path = %q, want %q", f.Name(), want)
	}
}

// TestBrowserArgs covers three invariants for the WebView2 flag set:
//
//  1. Dev is a strict superset of prod: dev mode only layers on top of
//     the always-applied flags. If a flag should drop from dev, it must
//     drop from prod first — otherwise dev users lose a safety net.
//  2. Prod must never include --remote-debugging-*. That port is
//     unauthenticated CDP and only belongs in developer builds.
//  3. No raw --disable-features/--enable-features switch, in any mode.
//     Wails emits its own --disable-features (seeded with
//     msSmartScreenProtection) and Chromium keeps only the last
//     occurrence of a duplicated switch — a raw flag here clobbers
//     Wails' list. Feature toggles go through browserDisabledFeatures,
//     which Wails merges into its single switch.
func TestBrowserArgs(t *testing.T) {
	prod := browserArgs("prod")
	dev := browserArgs("dev")

	devSet := make(map[string]struct{}, len(dev))
	for _, a := range dev {
		devSet[a] = struct{}{}
	}
	for _, a := range prod {
		if _, ok := devSet[a]; !ok {
			t.Errorf("prod arg %q missing from dev — dev must be a strict superset", a)
		}
	}

	for _, a := range prod {
		if strings.HasPrefix(a, "--remote-debugging-") {
			t.Errorf("prod must not include %q — CDP port is unauthenticated", a)
		}
	}

	for _, a := range dev {
		if strings.HasPrefix(a, "--disable-features") || strings.HasPrefix(a, "--enable-features") {
			t.Errorf("raw feature switch %q would clobber Wails' merged --disable-features — use browserDisabledFeatures/browserEnabledFeatures", a)
		}
	}
}

// TestBrowserFeatures pins the feature toggles that back specific fixes:
//
//   - OverscrollHistoryNavigation disabled — the two-finger-trackpad
//     back/forward gesture that WebView2's IsSwipeNavigationEnabled=false
//     no longer suppresses (WebView2Feedback #4502). Without it a
//     horizontal trackpad swipe navigates the window back to the
//     boot-time picker page.
func TestBrowserFeatures(t *testing.T) {
	disabled := browserDisabledFeatures()

	// The full disabled set is pinned by name so a list edit that drops
	// one (each backs a specific fix or hygiene decision — see the
	// comments in browserDisabledFeatures) fails loudly instead of
	// silently re-enabling a browser subsystem.
	wantDisabled := []string{
		"Translate",
		"AutofillServerCommunication",
		"MediaRouter",
		"DialMediaRouteProvider",
		"OptimizationHints",
		"IsolateOrigins",
		"site-per-process",
		"BackForwardCache",
		"Prerender2",
		"OverscrollHistoryNavigation",
	}
	disabledSet := make(map[string]struct{}, len(disabled))
	for _, f := range disabled {
		disabledSet[f] = struct{}{}
	}
	for _, f := range wantDisabled {
		if _, ok := disabledSet[f]; !ok {
			t.Errorf("feature %q missing from disabled features", f)
		}
	}
	if len(disabled) != len(wantDisabled) {
		t.Errorf("disabled features = %v, want exactly %v — update both together", disabled, wantDisabled)
	}
}

func TestBrowserArgsLeaveTextAntialiasingAtChromiumDefault(t *testing.T) {
	for _, arg := range browserArgs("prod") {
		if arg == "--disable-lcd-text" {
			t.Fatal("browser args force grayscale text antialiasing")
		}
	}
}

func TestWSLSingleInstanceModeUsesLauncherMode(t *testing.T) {
	orig := launcherMode
	origProfile := activeProfile
	t.Cleanup(func() { launcherMode = orig; activeProfile = origProfile })
	activeProfile = ""

	launcherMode = "dev"
	if got := wslSingleInstanceMode(); got != "dev" {
		t.Fatalf("dev launcher mode = %q", got)
	}
	launcherMode = "prod"
	if got := wslSingleInstanceMode(); got != "prod" {
		t.Fatalf("prod launcher mode = %q", got)
	}
	launcherMode = ""
	if got := wslSingleInstanceMode(); got != "prod" {
		t.Fatalf("empty launcher mode = %q, want prod", got)
	}
}

func TestSingleInstanceIDs(t *testing.T) {
	dev := appidentity.SingleInstanceID("wsl", "dev")
	prod := appidentity.SingleInstanceID("wsl", "prod")
	if dev == prod {
		t.Fatal("dev and prod single-instance IDs must differ")
	}
	if dev != "com.agentoverflow.wsl.dev" {
		t.Fatalf("dev single-instance ID = %q", dev)
	}
	if prod != "com.agentoverflow.wsl" {
		t.Fatalf("prod single-instance ID = %q", prod)
	}
}

func TestProbeBootstrapRetriesServiceUnavailable(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "backend not ready", http.StatusServiceUnavailable)
			return
		}
		writeProbeBootstrap(t, w, r, "test-token")
	}))
	defer server.Close()

	_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	err = probeBootstrapWithConfig(port, "test-token", bootstrapProbeConfig{
		AttemptTimeout: 100 * time.Millisecond,
		Deadline:       time.Second,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatalf("probeBootstrapWithConfig: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestProbeBootstrapTreatsNonReadyHTTPErrorAsTerminal(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()

	_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	err = probeBootstrapWithConfig(port, "test-token", bootstrapProbeConfig{
		AttemptTimeout: 100 * time.Millisecond,
		Deadline:       time.Second,
		PollInterval:   time.Millisecond,
	})
	if err == nil {
		t.Fatal("probeBootstrapWithConfig accepted 404, want error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want terminal response after 1 attempt", got)
	}
}

func TestProbeBootstrapReturnsTypedStartupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "backend startup failed", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	err = probeBootstrapWithConfig(port, "test-token", bootstrapProbeConfig{
		AttemptTimeout: 100 * time.Millisecond,
		Deadline:       time.Second,
		PollInterval:   time.Millisecond,
	})
	var httpErr bootstrapHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want bootstrapHTTPError", err)
	}
	if httpErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", httpErr.StatusCode, http.StatusInternalServerError)
	}
}

func TestProbeBootstrapRejectsInvalidSuccessBody(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"wsUrl":"ws://127.0.0.1:1/ws","token":"wrong"}`))
	}))
	defer server.Close()

	_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	err = probeBootstrapWithConfig(port, "test-token", bootstrapProbeConfig{
		AttemptTimeout: 100 * time.Millisecond,
		Deadline:       time.Second,
		PollInterval:   time.Millisecond,
	})
	if err == nil {
		t.Fatal("probeBootstrapWithConfig accepted invalid success body, want error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want invalid 200 to be terminal", got)
	}
}

func writeProbeBootstrap(t *testing.T, w http.ResponseWriter, r *http.Request, token string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		t.Fatalf("split request host: %v", err)
	}
	_, _ = fmt.Fprintf(w, `{"wsUrl":"ws://127.0.0.1:%s/ws","token":%q}`, port, token)
}

func TestResolveChosenDistro(t *testing.T) {
	distros := []wsllauncher.Distro{
		{Name: "Ubuntu-24.04", Default: true, Version: 2, State: "Running"},
		{Name: "Debian", Version: 2, State: "Stopped"},
	}
	soloDistros := []wsllauncher.Distro{
		{Name: "Ubuntu-24.04", Default: true, Version: 2, State: "Running"},
	}

	cases := []struct {
		name          string
		flags         launcherFlags
		cfg           *wsldistro.Config
		distros       []wsllauncher.Distro
		wantChosen    string
		wantTransient bool
	}{
		{
			name:          "override matches installed distro",
			flags:         launcherFlags{Distro: "Debian"},
			cfg:           &wsldistro.Config{Distro: "Ubuntu-24.04"},
			distros:       distros,
			wantChosen:    "Debian",
			wantTransient: true,
		},
		{
			name:          "override beats saved config when both match",
			flags:         launcherFlags{Distro: "Debian"},
			cfg:           &wsldistro.Config{Distro: "Debian"},
			distros:       distros,
			wantChosen:    "Debian",
			wantTransient: true,
		},
		{
			name:          "override pointing at unknown distro falls through to picker",
			flags:         launcherFlags{Distro: "Fedora"},
			cfg:           &wsldistro.Config{Distro: "Ubuntu-24.04"},
			distros:       distros,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "saved config used when no override",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: "Ubuntu-24.04"},
			distros:       distros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "stale saved config falls through to picker",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: "Removed-Distro"},
			distros:       distros,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "no override no saved config returns empty",
			flags:         launcherFlags{},
			cfg:           nil,
			distros:       distros,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "no override and empty cfg.Distro returns empty",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: ""},
			distros:       distros,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "no distros installed, override fails through to picker",
			flags:         launcherFlags{Distro: "Ubuntu"},
			cfg:           nil,
			distros:       nil,
			wantChosen:    "",
			wantTransient: false,
		},
		{
			name:          "single distro auto-picks when no override and no saved config",
			flags:         launcherFlags{},
			cfg:           nil,
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "single distro auto-picks when no override and empty saved config",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: ""},
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "single distro auto-picks even when saved config is stale",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: "Removed-Distro"},
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "saved config still wins over auto-pick when it matches",
			flags:         launcherFlags{},
			cfg:           &wsldistro.Config{Distro: "Ubuntu-24.04"},
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: false,
		},
		{
			name:          "override beats single-distro auto-pick and stays transient",
			flags:         launcherFlags{Distro: "Ubuntu-24.04"},
			cfg:           nil,
			distros:       soloDistros,
			wantChosen:    "Ubuntu-24.04",
			wantTransient: true,
		},
		{
			name:          "stale override does not fall through to single-distro auto-pick",
			flags:         launcherFlags{Distro: "Fedora"},
			cfg:           nil,
			distros:       soloDistros,
			wantChosen:    "",
			wantTransient: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotChosen, gotTransient := resolveChosenDistro(tc.flags, tc.cfg, tc.distros)
			if gotChosen != tc.wantChosen {
				t.Errorf("chosen = %q, want %q", gotChosen, tc.wantChosen)
			}
			if gotTransient != tc.wantTransient {
				t.Errorf("transient = %v, want %v", gotTransient, tc.wantTransient)
			}
		})
	}
}

// TestWebviewDataDir covers the per-mode profile split and the
// unresolvable-%APPDATA% fallback to "" (Wails default).
func TestWebviewDataDir(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("APPDATA", appData)

	base := filepath.Join(appData, "agent-overflow")
	if got, want := webviewDataDir("dev"), filepath.Join(base, "webview2-dev"); got != want {
		t.Errorf("dev dir = %q, want %q", got, want)
	}
	if got, want := webviewDataDir("prod"), filepath.Join(base, "webview2"); got != want {
		t.Errorf("prod dir = %q, want %q", got, want)
	}
	if got, want := webviewDataDir(appidentity.ModeSoak), filepath.Join(base, "webview2-soak"); got != want {
		t.Errorf("soak dir = %q, want %q", got, want)
	}
}

// TestSoakProfileFoldsEveryPerInstanceName is the launcher half of the
// isolation guard: with --profile soak set, every name the launcher
// derives must differ from what the same build would use without it.
// One of them sharing would put the soak instance inside the
// developer's live app (same single-instance id → the soak URL opens in
// their window; same WebView2 dir → same localStorage/IndexedDB).
func TestSoakProfileFoldsEveryPerInstanceName(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("APPDATA", appData)
	origMode, origProfile := launcherMode, activeProfile
	t.Cleanup(func() { launcherMode, activeProfile = origMode, origProfile })
	launcherMode = "dev"

	activeProfile = ""
	devMode := launcherRuntimeMode()
	devState, _ := windowStatePath()
	devWebview := webviewDataDir(devMode)
	devInstance := wslSingleInstanceMode()

	activeProfile = appidentity.ProfileSoak
	soakMode := launcherRuntimeMode()
	soakState, _ := windowStatePath()
	soakWebview := webviewDataDir(soakMode)
	soakInstance := wslSingleInstanceMode()

	if soakMode != appidentity.ModeSoak {
		t.Fatalf("runtime mode = %q, want %q (a soak launched from the dev build is soak)", soakMode, appidentity.ModeSoak)
	}
	for _, pair := range []struct {
		name       string
		dev, soaky string
	}{
		{"window state path", devState, soakState},
		{"webview data dir", devWebview, soakWebview},
		{"single-instance mode", devInstance, soakInstance},
	} {
		if pair.dev == pair.soaky {
			t.Errorf("%s is shared between the dev instance and the soak instance (%q)", pair.name, pair.dev)
		}
	}
}

// TestProfileBackendArgs: the soak profile must spell --soak on the WSL
// backend's argv. Without it the launcher would point at a backend
// running with the developer's real data dir and real provider binaries
// — the exact failure the profile axis exists to make impossible.
func TestProfileBackendArgs(t *testing.T) {
	orig := activeProfile
	t.Cleanup(func() { activeProfile = orig })

	activeProfile = ""
	if got := profileBackendArgs(); len(got) != 0 {
		t.Errorf("default profile backend args = %v, want none", got)
	}

	activeProfile = appidentity.ProfileSoak
	got := profileBackendArgs()
	if len(got) != 1 || got[0] != "--soak" {
		t.Fatalf("soak backend args = %v, want [--soak]", got)
	}
}

// TestSoakWindowIsSmallEnoughToParkBesideRealWork guards the ergonomic
// contract in docs/architecture/soak-rig.md: the soak window sits
// visible for hours next to the developer's actual app.
func TestSoakWindowIsSmallEnoughToParkBesideRealWork(t *testing.T) {
	if soakWindowWidth != 800 || soakWindowHeight != 600 {
		t.Fatalf("soak window = %dx%d, want 800x600", soakWindowWidth, soakWindowHeight)
	}
}

// TestWailsLogLevelSoakIsDebug: the fork logs half the render-watchdog
// narrative ("armed", "standing down", "re-navigating") at debug. A soak
// that drops those has episode starts with no story.
func TestWailsLogLevelSoakIsDebug(t *testing.T) {
	if got := wailsLogLevel(appidentity.ModeSoak); got != slog.LevelDebug {
		t.Fatalf("soak wails log level = %v, want debug", got)
	}
}

// TestRotateChromeDebugLog covers the pre-launch log rotation: the live
// chrome_debug.log becomes chrome_debug.previous.log (replacing any older
// rotation), and missing inputs are silent no-ops.
func TestRotateChromeDebugLog(t *testing.T) {
	t.Run("renames live log over previous", func(t *testing.T) {
		dataDir := t.TempDir()
		ebDir := filepath.Join(dataDir, "EBWebView")
		if err := os.MkdirAll(ebDir, 0o700); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(ebDir, "chrome_debug.log")
		dst := filepath.Join(ebDir, "chrome_debug.previous.log")
		if err := os.WriteFile(src, []byte("fresh session"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte("stale rotation"), 0o600); err != nil {
			t.Fatal(err)
		}

		rotateChromeDebugLog(dataDir)

		if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("live log still present after rotation: %v", err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read rotated log: %v", err)
		}
		if string(got) != "fresh session" {
			t.Errorf("rotated content = %q, want %q", got, "fresh session")
		}
	})

	t.Run("no live log is a no-op", func(t *testing.T) {
		dataDir := t.TempDir()
		rotateChromeDebugLog(dataDir)
		if _, err := os.Stat(filepath.Join(dataDir, "EBWebView", "chrome_debug.previous.log")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unexpected previous log: %v", err)
		}
	})

	t.Run("empty dataDir is a no-op", func(t *testing.T) {
		rotateChromeDebugLog("")
	})
}

// TestBrowserArgsWebviewLogGate covers the opt-in Chromium logging flags:
// present only when AGENT_OVERFLOW_WEBVIEW_LOG is set (in any mode — the
// gate is the env var, not dev mode), absent otherwise so the WebView2
// console-window side effect (WebView2Feedback #3192) stays out of normal
// runs.
func TestBrowserArgsWebviewLogGate(t *testing.T) {
	hasLogging := func(args []string) bool {
		for _, a := range args {
			if a == "--enable-logging" {
				return true
			}
		}
		return false
	}

	t.Setenv(webviewLogEnv, "")
	if hasLogging(browserArgs("dev")) || hasLogging(browserArgs("prod")) {
		t.Error("logging flags present without opt-in")
	}

	t.Setenv(webviewLogEnv, "1")
	if !hasLogging(browserArgs("dev")) {
		t.Error("dev: logging flags missing despite opt-in")
	}
	if !hasLogging(browserArgs("prod")) {
		t.Error("prod: logging flags missing despite opt-in")
	}
}

// TestBrowserArgsWebviewSoftwareGate covers the opt-in software-rendering
// flag: --disable-gpu present only when AGENT_OVERFLOW_WEBVIEW_SOFTWARE
// is set (in any mode — the gate is the env var, not dev mode), absent
// otherwise so normal runs keep GPU acceleration.
func TestBrowserArgsWebviewSoftwareGate(t *testing.T) {
	hasDisableGpu := func(args []string) bool {
		for _, a := range args {
			if a == "--disable-gpu" {
				return true
			}
		}
		return false
	}

	t.Setenv(webviewSoftwareEnv, "")
	if hasDisableGpu(browserArgs("dev")) || hasDisableGpu(browserArgs("prod")) {
		t.Error("--disable-gpu present without opt-in")
	}

	t.Setenv(webviewSoftwareEnv, "1")
	if !hasDisableGpu(browserArgs("dev")) {
		t.Error("dev: --disable-gpu missing despite opt-in")
	}
	if !hasDisableGpu(browserArgs("prod")) {
		t.Error("prod: --disable-gpu missing despite opt-in")
	}
}

// TestProbeBootstrapUnreachableIsRetryable pins the signal the
// fresh-port retry keys on: a probe that never got a single HTTP
// response back over Windows localhost. Nothing is listening on the
// probed port here, which is exactly what a Hyper-V excluded port range
// looks like from the Windows side while the WSL backend serves
// happily inside the distro.
func TestProbeBootstrapUnreachableIsRetryable(t *testing.T) {
	// Bind and release so the port is almost certainly free, then probe
	// it: every attempt is refused at the transport layer.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	_, portStr, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	err = probeBootstrapWithConfig(port, "test-token", bootstrapProbeConfig{
		AttemptTimeout: 100 * time.Millisecond,
		Deadline:       50 * time.Millisecond,
		PollInterval:   time.Millisecond,
	})
	if err == nil {
		t.Fatal("probeBootstrapWithConfig succeeded against a dead port")
	}
	if !errors.Is(err, errBackendUnreachable) {
		t.Fatalf("error = %v, want it to carry errBackendUnreachable", err)
	}
	if !retryWithFreshTransportPort(err) {
		t.Fatal("an unreachable backend must be retried on a fresh transport port")
	}
}

// TestProbeBootstrapAnsweredFailuresAreNotRetryable is the other half:
// once the backend has answered ANYTHING over Windows localhost, the
// port is demonstrably reachable and moving it would churn the webview
// origin (and every origin-scoped browser store) for nothing.
func TestProbeBootstrapAnsweredFailuresAreNotRetryable(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "startup failure",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "backend startup failed", http.StatusInternalServerError)
			},
		},
		{
			name: "credential rejected",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
		},
		{
			name: "never becomes ready",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "still booting", http.StatusServiceUnavailable)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()

			_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatalf("split test server addr: %v", err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				t.Fatalf("parse test server port: %v", err)
			}

			err = probeBootstrapWithConfig(port, "test-token", bootstrapProbeConfig{
				AttemptTimeout: 100 * time.Millisecond,
				Deadline:       50 * time.Millisecond,
				PollInterval:   time.Millisecond,
			})
			if err == nil {
				t.Fatal("probeBootstrapWithConfig succeeded, want failure")
			}
			if retryWithFreshTransportPort(err) {
				t.Fatalf("error %v was classified as unreachable; a fresh port cannot fix an answered failure", err)
			}
		})
	}
}

// TestRetryWithFreshTransportPortIgnoresUnrelatedErrors keeps the
// classifier from widening by accident: only the sentinel qualifies.
func TestRetryWithFreshTransportPortIgnoresUnrelatedErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("boom"), errLaunchFailed, bootstrapHTTPError{StatusCode: 500, URL: "u"}} {
		if retryWithFreshTransportPort(err) {
			t.Errorf("retryWithFreshTransportPort(%v) = true", err)
		}
	}
}

// TestResetTransportPortArgMatchesTheBackendFlag pins the wire word of
// the cross-binary contract. Both sides already derive the name from
// wsllauncher.ResetTransportPortFlag, so they cannot disagree with each
// other — this pins that neither drifts away from the flag an already
// installed backend (an older payload in the distro) understands.
func TestResetTransportPortArgMatchesTheBackendFlag(t *testing.T) {
	const expected = "--reset-transport-port"
	if resetTransportPortArg != expected {
		t.Fatalf("resetTransportPortArg = %q, want %q (keep it in step with the backend flag)", resetTransportPortArg, expected)
	}
}
