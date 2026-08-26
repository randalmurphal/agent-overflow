// main_harness.go owns the --harness boot path: the agent test harness
// that runs the real backend + real SPA headless on an isolated data
// directory, with both provider binaries pointed at ao-mockprovider so
// no real Claude/Codex process (or account) is ever touched. The
// Harness RPC receiver (app_harness.go) is registered on the transport
// only on this path — every other boot leaves the harness surface off
// the wire entirely.
//
// See docs/architecture/agent-harness.md for the full guide.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

// harnessStdoutPrefix marks the harness bootstrap line on stdout. Kept
// distinct from bootstrapStdoutPrefix so a launcher scanning for the
// headless sentinel can never confuse the two payload shapes.
const harnessStdoutPrefix = "__AO_HARNESS__:"

// harnessKeepHomeEnv opts out of the HOME redirect for callers that
// deliberately want the harness to see the real ~/.claude / ~/.codex
// trees (e.g. replaying against real provider session files).
const harnessKeepHomeEnv = "AO_HARNESS_KEEP_HOME"

// harnessPaths carries every filesystem decision prepareHarness made so
// runHarness and the Harness receiver report the same values.
type harnessPaths struct {
	// DataRoot is the absolute --data-dir value.
	DataRoot string
	// DataDir is <DataRoot>/agent-overflow — where the DB, settings,
	// replay logs, and attachments actually live (initStores appends
	// the app subdirectory to the root override).
	DataDir string
	// HomeDir is the redirected $HOME (empty when AO_HARNESS_KEEP_HOME
	// was set). Provider-session fixtures (~/.claude/projects/...) are
	// seeded under it.
	HomeDir string
	// CredentialHome is the home the credential surface is pinned under
	// (App.credentialHomeOverride). Always <dataRoot>/home — identical to
	// HomeDir on a normal run, and still harness-owned when
	// AO_HARNESS_KEEP_HOME leaves $HOME real.
	CredentialHome string
	// MockProvider is the resolved ao-mockprovider binary path that
	// both provider binary settings point at.
	MockProvider string
}

// runHarness boots the agent test harness. The shape mirrors
// runHeadless — transport first, then App.Start, then MarkReady — with
// three harness-specific steps around it: environment preparation
// (prepareHarness), Harness receiver registration (via
// bootTransportOptions.HarnessReceiver), and the harness bootstrap line
// that hands agents the URL, token, and data paths in one place.
func runHarness(flags cliFlags) {
	if flags.window {
		// Before any work: a nogui payload can never open a window, and
		// finding that out after seeding a data directory would be a worse
		// error message for the same mistake.
		requireWindowedBuild()
	}
	paths, err := prepareHarness(flags)
	if err != nil {
		fatalf("harness: %v", err)
	}
	if flags.window {
		// After prepareHarness (its refusals compare against the real
		// config root) and before the first GLib call. See
		// isolateWebviewStorage.
		isolateWebviewStorage(paths.DataRoot)
	}

	appService := newIsolatedProviderApp(paths)
	h := newHarness(appService, paths)
	// The control server must listen before App.Start: it publishes its
	// address/token through App.providerExtraEnv (write-once before
	// Start), and the first session start could spawn a mock that needs
	// them.
	if err := h.startControl(); err != nil {
		fatalf("harness: start mock control server: %v", err)
	}
	defer h.shutdownControl()
	srv := bootTransport(appService, flags.listenAddr, bootTransportOptions{
		RequireReadyForBootstrap: true,
		HarnessReceiver:          h,
		AllowDevServerAssets:     true,
	})
	log.Printf("transport: harness mode (data dir %s)", paths.DataDir)

	// Route all subsequent output to stderr so stdout carries exactly
	// one parseable line (the harness bootstrap), mirroring the
	// headless path's stdout-hygiene contract. The real stdout is kept
	// aside for that single write.
	bootstrapOut := os.Stdout
	os.Stdout = os.Stderr
	log.SetOutput(os.Stderr)

	bootCtx, bootCancel := context.WithCancel(context.Background())
	defer bootCancel()
	if err := appService.Start(bootCtx); err != nil {
		log.Printf("app: service startup: %v", err)
		srv.MarkStartupFailed()
		if err := writeHarnessBootstrap(bootstrapOut, srv, paths, err); err != nil {
			log.Printf("harness: write bootstrap: %v", err)
		}
		waitForHeadlessShutdown(appService, srv)
		return
	}
	srv.MarkReady()

	if err := writeHarnessBootstrap(bootstrapOut, srv, paths, nil); err != nil {
		shutdownHeadless(appService, srv)
		fatalf("harness: write bootstrap: %v", err)
	}

	// Discovery files last: they advertise an instance that is ready to
	// attach, which is true only now.
	// No launcher pid: --harness is never spawned by the Windows launcher
	// (that shell is --soak), so nobody but this process hosts a window.
	instance := publishInstance(srv, paths, instanceinfo.ModeHarness, flags.window, 0)
	defer instance.remove()

	if flags.window {
		if err := runWindowedShell(appService, srv, isolatedWindowTitle(instanceinfo.ModeHarness, instance.id)); err != nil {
			instance.remove()
			h.shutdownControl()
			fatalf("harness: %v", err)
		}
		return
	}
	waitForHeadlessShutdown(appService, srv)
}

// newIsolatedProviderApp builds the App for a boot mode whose providers
// are mocked: the agent test harness (--harness) and the soak rig
// (--soak). It is the ONE place these pins are applied, so a second
// mocked boot mode cannot ship with three of the four — enforced by
// TestMockedBootModesShareOneIsolationHelper.
//
// Every pin here is structural, not advisory: settings stay editable at
// runtime in both modes, and only the spawn-time override makes "this
// process can never reach a real provider binary" true regardless of
// what a later UpdateSettings writes.
func newIsolatedProviderApp(paths harnessPaths) *App {
	appService := newApp()
	// The REAL sender, the same one runHeadless installs — not a refusal
	// stub. An isolated boot's whole premise is that everything except the
	// provider processes is production code, and a stub made the
	// notification pipe the one exception: `HarnessNotify` returned "OS
	// notifications are unavailable" before emitting anything, so the
	// e2e spec asserted the stub's error message and the emission path it
	// was supposed to cover was never executed at all.
	//
	// This degrades correctly rather than presenting anything unwanted.
	// The sender EMITS on `notification:send`; presentation is the
	// subscriber's job. Under `--soak` on Windows that subscriber is the
	// real launcher, which is exactly the pipe a soak exists to exercise;
	// under a headless `--harness` nobody subscribes, and the sender logs
	// one line and succeeds. The bus does not exist yet at this point —
	// bootTransport installs it — and that is fine: the sender resolves it
	// per send.
	appService.osNotifications = newTransportNotificationSender(appService)
	// Pin provider spawns to the mock at resolution time, not just via
	// the seeded settings — UpdateSettings stays callable in these modes,
	// but it can never repoint a spawn at a real provider binary.
	appService.providerBinaryOverride = paths.MockProvider
	// The redirected $HOME isolates every file store but not the macOS
	// Keychain (the active Claude slot's service name ignores the home),
	// so credential storage is pinned to the file-backed stand-in.
	appService.fileKeychainOverride = true
	// Pin the credential surface under the mode-owned home
	// unconditionally: AO_HARNESS_KEEP_HOME keeps the real $HOME for
	// provider session-file reads, and this pin is what keeps that flag
	// read-only — slots, canonical credential, and the orphan prune must
	// never resolve against the developer's real provider homes.
	appService.credentialHomeOverride = paths.CredentialHome
	// No timer may run `git fetch` against the fixture repositories: e2e
	// assertions read ahead/behind counts as fixed state, and neither a
	// harness run nor an hours-long soak may reach a network.
	appService.backgroundFetchDisabled = true
	return appService
}

// prepareHarness makes every filesystem decision the harness boot
// needs, in dependency order: validate + create the data root, redirect
// HOME under it, resolve the mock provider binary, and seed the
// harness settings. Fails loudly on the first problem — a harness that
// half-boots against ambiguous state is worse than no harness.
//
// The soak rig (main_soak.go) runs the same preparation: same data-dir
// refusals, same HOME redirect, same mock resolution, same settings
// seed. Only the boot shell around it differs.
func prepareHarness(flags cliFlags) (harnessPaths, error) {
	dataRoot, err := filepath.Abs(flags.dataDir)
	if err != nil {
		return harnessPaths{}, fmt.Errorf("resolve --data-dir: %w", err)
	}
	if err := refuseRealDataDir(dataRoot); err != nil {
		return harnessPaths{}, err
	}
	// No harness-owned path may be a symlink: these directories are
	// seeded and wiped wholesale, and a planted link would aim those
	// operations at whatever it points to. That covers the root itself
	// (`make harness` derives a predictable /tmp path from the checkout —
	// a link there routes EVERY child path elsewhere; the real-data check
	// above only catches links aimed at the config root specifically),
	// the app data dir, and the redirected home below. Parent components
	// (e.g. macOS's /tmp -> /private/tmp) are deliberately not resolved —
	// only the leaves the harness owns are checked.
	if err := refuseSymlink(dataRoot); err != nil {
		return harnessPaths{}, err
	}
	// The root gets created and vetted BEFORE anything below it, and the
	// order is the point: refuseSymlink rules out a planted LINK at this
	// predictable path, and refuseUnsafeHarnessDir rules out a planted
	// world-writable DIRECTORY at it. Everything created afterwards is
	// created inside a directory nobody else can write, so a pre-existing
	// child cannot have been planted by a stranger.
	if err := ensureHarnessPrivateDir(dataRoot); err != nil {
		return harnessPaths{}, fmt.Errorf("create harness data root: %w", err)
	}
	if err := refuseUnsafeHarnessDir(dataRoot); err != nil {
		return harnessPaths{}, err
	}

	dataDir := filepath.Join(dataRoot, "agent-overflow")
	if err := refuseSymlink(dataDir); err != nil {
		return harnessPaths{}, err
	}
	if err := ensureAppPrivateDir(dataDir); err != nil {
		return harnessPaths{}, fmt.Errorf("create harness data dir: %w", err)
	}
	if err := refuseUnsafeHarnessDir(dataDir); err != nil {
		return harnessPaths{}, err
	}

	// Before ANY mutation of the root's contents: everything below this
	// line (the HOME redirect, the .gitconfig seed, the settings seed) is
	// a write into a tree a live backend may already own. The lock is held
	// for the process's lifetime and released by the kernel on death.
	if _, err := acquireHarnessInstanceLock(dataDir, harnessBootMode(flags)); err != nil {
		return harnessPaths{}, err
	}

	homeDir, err := isolateHarnessHome(dataRoot)
	if err != nil {
		return harnessPaths{}, err
	}
	// Checked whether or not AO_HARNESS_KEEP_HOME left $HOME real: the
	// credential surface is pinned under this directory either way.
	if err := refuseUnsafeHarnessDir(filepath.Join(dataRoot, "home")); err != nil {
		return harnessPaths{}, err
	}

	mockProvider, err := resolveMockProvider(flags.mockProvider)
	if err != nil {
		return harnessPaths{}, err
	}

	if err := seedHarnessSettings(dataDir, mockProvider); err != nil {
		return harnessPaths{}, err
	}

	return harnessPaths{
		DataRoot:       dataRoot,
		DataDir:        dataDir,
		HomeDir:        homeDir,
		CredentialHome: filepath.Join(dataRoot, "home"),
		MockProvider:   mockProvider,
	}, nil
}

// ensureHarnessPrivateDir creates a harness-owned directory at 0700,
// stamping the mode explicitly because MkdirAll only ever subtracts from
// its argument through the umask (an 0022 process would get 0755, an
// 0000 one 0777).
//
// A directory that ALREADY exists is left exactly as found, unlike
// ensureAppPrivateDir: the caller's next move is to refuse a planted one,
// and a chmod here would repair the very evidence that check reads.
func ensureHarnessPrivateDir(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, appdirs.PrivateDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, appdirs.PrivateDirPerm)
}

// refuseRealDataDir rejects a --data-dir that resolves to the OS
// default config root — the one place the user's real app data lives.
// The harness seeds, wipes, and restores its data dir wholesale; one
// mistyped flag must not point that at real threads.
func refuseRealDataDir(dataRoot string) error {
	defaultRoot, err := os.UserConfigDir()
	if err != nil {
		// No default config dir resolvable means nothing to protect.
		return nil
	}
	if sameCanonicalPath(dataRoot, defaultRoot) {
		return fmt.Errorf("--data-dir %s is the OS config root; an isolated boot (--harness / --soak) refuses to run against real app data (pick a scratch dir)", dataRoot)
	}
	if sameCanonicalPath(dataRoot, filepath.Join(defaultRoot, "agent-overflow")) {
		return fmt.Errorf("--data-dir %s is the real app data dir; an isolated boot (--harness / --soak) refuses to run against real app data (pick a scratch dir)", dataRoot)
	}
	return nil
}

// refuseSymlink rejects a path that exists as a symlink. Harness-owned
// directories are created and wiped wholesale; following a planted link
// would aim those operations at whatever it points to.
func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; an isolated boot (--harness / --soak) refuses to operate through links (it seeds and wipes this directory wholesale)", path)
	}
	return nil
}

// sameCanonicalPath compares two paths after symlink resolution +
// cleaning, tolerating paths that don't exist yet (falls back to
// lexical comparison).
func sameCanonicalPath(a, b string) bool {
	ca, errA := filepath.EvalSymlinks(a)
	cb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ca == cb
}

// isolateHarnessHome redirects $HOME (and %USERPROFILE% on Windows) to
// <dataRoot>/home so everything the backend resolves through the home
// directory — ~/.claude session scans, ~/.codex rollout tails, git's
// global config — is harness-local and seedable. A minimal .gitconfig
// is written so git commits made by checkpoint/test flows don't fail
// identity detection under the empty home.
//
// Returns the redirected home path, or "" when AO_HARNESS_KEEP_HOME
// opted out. The harness home directory is created in BOTH cases: even a
// keep-home run pins the credential surface (provideraccounts slots,
// canonical credential, orphan prune) under it via
// App.credentialHomeOverride, so the flag only ever widens what the
// harness can READ (~/.claude session files, ~/.codex rollouts) — never
// what it can destroy. Before that pin existed, a keep-home run whose
// reused data dir already listed a mock account handed the boot-time
// prune a non-empty foreign keep-set aimed at the real ~/.claude — the
// 2026-07-29 incident class.
func isolateHarnessHome(dataRoot string) (string, error) {
	homeDir := filepath.Join(dataRoot, "home")
	if err := refuseSymlink(homeDir); err != nil {
		return "", err
	}
	if err := ensureHarnessPrivateDir(homeDir); err != nil {
		return "", fmt.Errorf("create harness home: %w", err)
	}
	if os.Getenv(harnessKeepHomeEnv) != "" {
		log.Printf(
			"harness: %s set — keeping real HOME %s for provider session reads; credential storage stays in %s",
			harnessKeepHomeEnv,
			os.Getenv("HOME"),
			homeDir,
		)
		return "", nil
	}
	gitconfig := filepath.Join(homeDir, ".gitconfig")
	if _, err := os.Stat(gitconfig); errors.Is(err, os.ErrNotExist) {
		identity := "[user]\n\tname = Agent Overflow Harness\n\temail = harness@agent-overflow.invalid\n[init]\n\tdefaultBranch = main\n"
		if err := os.WriteFile(gitconfig, []byte(identity), 0o600); err != nil {
			return "", fmt.Errorf("write harness .gitconfig: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("stat harness .gitconfig: %w", err)
	}
	if err := os.Setenv("HOME", homeDir); err != nil {
		return "", fmt.Errorf("redirect HOME: %w", err)
	}
	if runtime.GOOS == "windows" {
		if err := os.Setenv("USERPROFILE", homeDir); err != nil {
			return "", fmt.Errorf("redirect USERPROFILE: %w", err)
		}
	}
	return homeDir, nil
}

// resolveMockProvider locates the ao-mockprovider binary: the explicit
// --mock-provider flag wins, otherwise it must sit next to the running
// executable (where `make harness` puts it). The path is validated
// eagerly — a missing mock provider would otherwise only surface as a
// session-start failure deep into a test run.
func resolveMockProvider(flagPath string) (string, error) {
	candidate := flagPath
	if candidate == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate own executable for mock provider lookup: %w", err)
		}
		name := "ao-mockprovider"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		candidate = filepath.Join(filepath.Dir(exe), name)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve mock provider path: %w", err)
	}
	if _, err := exec.LookPath(abs); err != nil {
		return "", fmt.Errorf("mock provider binary not runnable at %s (build it with `make mockprovider`, or pass --mock-provider): %w", abs, err)
	}
	return abs, nil
}

// seedHarnessSettings points both provider binary settings at the mock
// provider and switches on the NDJSON event log so every harness
// session is recordable for wire-level replay. Runs through the real
// settings service (validation included) and re-applies on every boot —
// the mock provider path can move between runs (rebuilds, different
// checkouts) and a stale path would fail only at session start.
func seedHarnessSettings(dataDir, mockProvider string) error {
	svc := settings.NewService(dataDir)
	_, err := svc.Update(map[string]any{
		"claudeBinaryPath":             mockProvider,
		"codexBinaryPath":              mockProvider,
		"observabilityEventLogEnabled": true,
	})
	if err != nil {
		return fmt.Errorf("seed harness settings: %w", err)
	}
	return nil
}

// harnessBootstrap is the one-line JSON contract agents parse off
// stdout. Everything needed to attach is in this line: the page URL
// (token included), the WS token for direct RPC clients, and the data
// paths where evidence (DB, traces, event logs) accumulates.
type harnessBootstrap struct {
	URL          string `json:"url"`
	Port         int    `json:"port"`
	Token        string `json:"token"`
	DataRoot     string `json:"dataRoot"`
	DataDir      string `json:"dataDir"`
	HomeDir      string `json:"homeDir,omitempty"`
	MockProvider string `json:"mockProvider"`
	PID          int    `json:"pid"`
	Version      string `json:"version"`
	// ClientID is this instance's durable UI-state identity
	// (ensureClientID), already threaded onto URL as `&cid=`. It is
	// reported separately as well because a caller that builds its own
	// page URL — the Windows launcher opening a WebView2 window on a
	// --soak backend, an e2e run pointing Playwright at the instance —
	// must attach the SAME id, or the frontend's per-client ui_state
	// bucket changes identity on every launch and every persisted UI
	// preference reads as unset.
	//
	// Isolated boots resolve it under their own --data-dir
	// (bootSettingsDir honors the override), so it is the harness's own
	// id and never the developer's.
	ClientID string `json:"clientId,omitempty"`
	// StartupError is set when App.Start failed; the transport still
	// serves (bootstrap returns the terminal failure state) so the
	// caller can read logs, but the harness is not usable.
	StartupError string `json:"startupError,omitempty"`
}

// newHarnessBootstrap assembles the payload. Split from the write so
// the same fields reach the stdout line and <dataDir>/harness-instance.json
// (main_harness_instance.go) — a tool that attaches to a running
// instance must not be reading a second, drifting description of it.
func newHarnessBootstrap(srv *transport.Server, paths harnessPaths, startupErr error) harnessBootstrap {
	clientID := ensureClientID()
	bs := harnessBootstrap{
		URL:          appURLWithClientID(srv.AppURL(), clientID),
		Port:         portFromAddr(srv.Addr()),
		Token:        srv.Token(),
		DataRoot:     paths.DataRoot,
		DataDir:      paths.DataDir,
		HomeDir:      paths.HomeDir,
		MockProvider: paths.MockProvider,
		PID:          os.Getpid(),
		Version:      version,
		ClientID:     clientID,
	}
	if startupErr != nil {
		bs.StartupError = startupErr.Error()
	}
	return bs
}

func writeHarnessBootstrap(out *os.File, srv *transport.Server, paths harnessPaths, startupErr error) error {
	payload, err := json.Marshal(newHarnessBootstrap(srv, paths, startupErr))
	if err != nil {
		return fmt.Errorf("marshal harness bootstrap: %w", err)
	}
	if _, err := fmt.Fprintf(out, "\n%s %s\n", harnessStdoutPrefix, payload); err != nil {
		return fmt.Errorf("write harness bootstrap: %w", err)
	}
	return nil
}
