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
	paths, err := prepareHarness(flags)
	if err != nil {
		fatalf("harness: %v", err)
	}

	appService := newApp()
	appService.configureUnavailableNotifications("the agent test harness has no OS notification presenter")
	// Pin provider spawns to the mock at resolution time, not just via
	// the seeded settings — UpdateSettings stays callable in harness
	// mode, but it can never repoint a spawn at a real provider binary.
	appService.providerBinaryOverride = paths.MockProvider
	// The redirected harness $HOME isolates every file store but not the
	// macOS Keychain (the active Claude slot's service name ignores the
	// home), so credential storage is pinned to the file-backed stand-in.
	appService.fileKeychainOverride = true
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
	waitForHeadlessShutdown(appService, srv)
}

// prepareHarness makes every filesystem decision the harness boot
// needs, in dependency order: validate + create the data root, redirect
// HOME under it, resolve the mock provider binary, and seed the
// harness settings. Fails loudly on the first problem — a harness that
// half-boots against ambiguous state is worse than no harness.
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
	dataDir := filepath.Join(dataRoot, "agent-overflow")
	if err := refuseSymlink(dataDir); err != nil {
		return harnessPaths{}, err
	}
	if err := ensureAppPrivateDir(dataDir); err != nil {
		return harnessPaths{}, fmt.Errorf("create harness data dir: %w", err)
	}

	homeDir, err := isolateHarnessHome(dataRoot)
	if err != nil {
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
		DataRoot:     dataRoot,
		DataDir:      dataDir,
		HomeDir:      homeDir,
		MockProvider: mockProvider,
	}, nil
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
		return fmt.Errorf("--data-dir %s is the OS config root; the harness refuses to run against real app data (pick a scratch dir)", dataRoot)
	}
	if sameCanonicalPath(dataRoot, filepath.Join(defaultRoot, "agent-overflow")) {
		return fmt.Errorf("--data-dir %s is the real app data dir; the harness refuses to run against real app data (pick a scratch dir)", dataRoot)
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
		return fmt.Errorf("%s is a symlink; the harness refuses to operate through links (it seeds and wipes this directory wholesale)", path)
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
// opted out.
func isolateHarnessHome(dataRoot string) (string, error) {
	if os.Getenv(harnessKeepHomeEnv) != "" {
		log.Printf("harness: %s set — keeping real HOME %s", harnessKeepHomeEnv, os.Getenv("HOME"))
		return "", nil
	}
	homeDir := filepath.Join(dataRoot, "home")
	if err := refuseSymlink(homeDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return "", fmt.Errorf("create harness home: %w", err)
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
	// StartupError is set when App.Start failed; the transport still
	// serves (bootstrap returns the terminal failure state) so the
	// caller can read logs, but the harness is not usable.
	StartupError string `json:"startupError,omitempty"`
}

func writeHarnessBootstrap(out *os.File, srv *transport.Server, paths harnessPaths, startupErr error) error {
	bs := harnessBootstrap{
		URL:          srv.AppURL(),
		Port:         portFromAddr(srv.Addr()),
		Token:        srv.Token(),
		DataRoot:     paths.DataRoot,
		DataDir:      paths.DataDir,
		HomeDir:      paths.HomeDir,
		MockProvider: paths.MockProvider,
		PID:          os.Getpid(),
		Version:      version,
	}
	if startupErr != nil {
		bs.StartupError = startupErr.Error()
	}
	payload, err := json.Marshal(bs)
	if err != nil {
		return fmt.Errorf("marshal harness bootstrap: %w", err)
	}
	if _, err := fmt.Fprintf(out, "\n%s %s\n", harnessStdoutPrefix, payload); err != nil {
		return fmt.Errorf("write harness bootstrap: %w", err)
	}
	return nil
}
