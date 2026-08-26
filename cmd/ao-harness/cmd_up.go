package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

// backendBinEnv is the same override e2e/src/harness.ts honours, so a
// developer who exported it once gets it everywhere.
const backendBinEnv = "AO_HARNESS_BIN"

// logDirName / backendStderrLog / backendStdoutLog name the capture
// files `up` gives the detached backend. The stderr file is what
// `ao-harness logs backend` tails: a detached instance has no terminal,
// so this file IS its console.
const (
	logDirName       = "logs"
	backendStderrLog = "backend-stderr.log"
	backendStdoutLog = "backend-stdout.log"
)

func runUp(e *env, args []string) error {
	flags := e.newFlagSet("up")
	window := flags.Bool("window", false, "open the real webview window instead of running headless (GUI builds only)")
	soak := flags.Bool("soak", false, "boot the launcher shell (the ordinary {port,token} bootstrap) instead of the harness shell")
	autopilot := flags.Bool("autopilot", false, "with --soak: arm the soak preset (seeded threads plus a turn that streams forever)")
	dataDir := flags.String("data-dir", "", "data root to boot on (default: this worktree's per-checkout root)")
	binary := flags.String("binary", "", "agent-overflow binary to run (default: $AO_HARNESS_BIN, else bin/agent-overflow beside this CLI)")
	mockProvider := flags.String("mock-provider", "", "ao-mockprovider path (default: the backend resolves it beside itself)")
	devAssets := flags.String("dev-assets", "", "serve the frontend from this Vite dev server URL instead of the embedded build")
	keepHome := flags.Bool("keep-home", false, "leave $HOME real so the instance can READ your provider session files (credentials stay isolated)")
	timeout := flags.Duration("timeout", harnessclient.DefaultLaunchTimeout, "how long to wait for the bootstrap line")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("up takes no positional arguments (got %v)", rest)
	}

	if *autopilot && !*soak {
		return usagef("--autopilot requires --soak (it arms the soak preset on the launcher-shell backend)")
	}
	dataRoot, err := upDataRoot(*dataDir, *autopilot)
	if err != nil {
		return err
	}
	// The backend's own refusals, run BEFORE this command creates
	// anything. `up` used to MkdirAll a log directory into a root the
	// backend was about to refuse, so a mistyped --data-dir left a
	// half-made tree inside (say) the real config root and then failed
	// with a message about the boot.
	if err := refuseUnsafeDataRoot(dataRoot); err != nil {
		return err
	}
	if err := refuseSecondInstance(e, dataRoot); err != nil {
		return err
	}

	bin, err := resolveBackendBinary(*binary)
	if err != nil {
		return err
	}

	appDataDir := filepath.Join(dataRoot, "agent-overflow")
	logDir := filepath.Join(appDataDir, logDirName)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", logDir, err)
	}
	stderrPath := filepath.Join(logDir, backendStderrLog)

	launched, err := harnessclient.Launch(context.Background(), harnessclient.LaunchOptions{
		Binary:       bin,
		DataRoot:     dataRoot,
		MockProvider: *mockProvider,
		Soak:         *soak,
		Autopilot:    *autopilot,
		Window:       *window,
		DevAssetsURL: *devAssets,
		KeepHome:     *keepHome,
		Timeout:      *timeout,
		Detach:       true,
		StdoutPath:   filepath.Join(logDir, backendStdoutLog),
		StderrPath:   stderrPath,
	})
	if err != nil {
		return err
	}

	bs := launched.Bootstrap
	id := instanceinfo.ID(dataRoot)
	// Mode follows the AUTOPILOT, not the shell: that is what the instance
	// itself stamps on its registry row, and a listing that disagreed with
	// the row would send a reader looking for the wrong instance.
	mode := instanceinfo.ModeHarness
	if *autopilot {
		mode = instanceinfo.ModeSoak
	}
	if e.jsonOutput() {
		return e.writeJSON(map[string]any{
			"id": id, "mode": mode, "window": *window, "pid": bs.PID, "port": bs.Port,
			"url": bs.URL, "dataRoot": bs.DataRoot, "dataDir": bs.DataDir,
			"mockProvider": bs.MockProvider, "version": bs.Version, "backendStderr": stderrPath,
		})
	}
	e.printf("instance %s (%s%s) is up\n", id, mode, windowSuffix(*window))
	e.printf("  pid       %d\n", bs.PID)
	e.printf("  url       %s\n", bs.URL)
	e.printf("  data dir  %s\n", bs.DataDir)
	e.printf("  stderr    %s\n", stderrPath)
	return nil
}

func windowSuffix(window bool) string {
	if window {
		return ", windowed"
	}
	return ""
}

// upDataRoot picks where a new instance lives: the flag, else the
// per-worktree default for the PRESET being booted. The soak default
// carries its own suffix, exactly as the backend's own flag default
// does — the two must agree or `up --soak --autopilot` and `make
// soak-window` would mean different instances. It keys on the autopilot
// rather than the shell because the suffix exists to stop a soak landing
// on a root holding threads it did not seed, which is a property of the
// preset.
func upDataRoot(flagValue string, autopilot bool) (string, error) {
	if flagValue != "" {
		abs, err := filepath.Abs(flagValue)
		if err != nil {
			return "", fmt.Errorf("resolve --data-dir %q: %w", flagValue, err)
		}
		return abs, nil
	}
	if autopilot {
		return instanceinfo.DefaultSoakDataRoot(), nil
	}
	return instanceinfo.DefaultDataRoot(), nil
}

// refuseUnsafeDataRoot mirrors the two refusals prepareHarness runs at
// boot: the data root must not resolve to the OS config root or the real
// app data dir, and neither the root nor its app directory may BE a
// symlink. An isolated boot seeds and wipes those directories wholesale,
// so a planted link aims that at whatever it points to, and one mistyped
// flag must not point it at real threads.
//
// Deliberately reimplemented rather than imported: this binary links no
// App code, which is what keeps it from becoming a second way to drive
// the app. The duplication is three small checks, and the cost of them
// drifting is a worse ERROR MESSAGE — the backend still refuses. The
// cost of not having them is a directory tree created inside the config
// root before anyone says no.
func refuseUnsafeDataRoot(dataRoot string) error {
	realRoot, err := appdirs.Root()
	if err == nil {
		configRoot := filepath.Dir(realRoot)
		for _, forbidden := range []string{configRoot, realRoot} {
			if sameResolvedPath(dataRoot, forbidden) {
				return fmt.Errorf(
					"--data-dir %s is %s, where the real app data lives; an isolated boot refuses to run against it (pick a scratch dir, or omit --data-dir for this worktree's own root)",
					dataRoot, forbidden)
			}
		}
	}
	for _, path := range []string{dataRoot, filepath.Join(dataRoot, appDataDirName)} {
		if err := refuseSymlinkedPath(path); err != nil {
			return err
		}
	}
	return nil
}

// refuseSymlinkedPath refuses a path that EXISTS as a symlink. A path
// that does not exist yet is fine: `up` is about to create it, and the
// backend re-checks after it does.
func refuseSymlinkedPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"%s is a symlink; an isolated boot refuses to operate through links (it seeds and wipes this directory wholesale)", path)
	}
	return nil
}

// sameResolvedPath compares two paths after symlink resolution, falling
// back to a lexical comparison for a path that does not exist yet —
// which is what a fresh data root is.
func sameResolvedPath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// refuseSecondInstance stops a boot onto a data root something is
// already serving. Two backends on one SQLite file is the failure this
// exists to prevent, and it is not one the second boot would report
// clearly on its own.
//
// A leftover instance file whose pid is dead is not a refusal: that is
// what a killed instance leaves behind, and refusing there would make a
// crash require manual cleanup.
func refuseSecondInstance(e *env, dataRoot string) error {
	dataDir := filepath.Join(dataRoot, "agent-overflow")
	bs, err := harnessclient.ReadInstanceFile(dataDir)
	if err == nil && instanceinfo.ProcessAlive(bs.PID) {
		return fmt.Errorf(
			"instance %s is already running on %s (pid %d, %s); stop it with `ao-harness down --instance %s`",
			instanceinfo.ID(dataRoot), dataRoot, bs.PID, bs.URL, instanceinfo.ID(dataRoot))
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A file we cannot read is not proof of anything; say so and keep
		// going rather than blocking a boot on a parse failure.
		fmt.Fprintf(e.stderr, "ao-harness: ignoring unreadable %s: %v\n", harnessclient.InstanceFilePath(dataDir), err)
	}
	return nil
}

// resolveBackendBinary finds the app binary to boot: the flag, then
// $AO_HARNESS_BIN, then the sibling of this CLI — `make harness-build`
// puts both in bin/, so the common case needs no configuration at all.
func resolveBackendBinary(flagValue string) (string, error) {
	name := "agent-overflow"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidates := []string{flagValue, os.Getenv(backendBinEnv)}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		abs, err := filepath.Abs(candidate)
		if err != nil {
			return "", fmt.Errorf("resolve backend binary %q: %w", candidate, err)
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		return abs, nil
	}
	return "", fmt.Errorf(
		"no %s binary found (pass --binary, set %s, or run `make harness-build` which puts it in bin/ beside this CLI)",
		name, backendBinEnv)
}
