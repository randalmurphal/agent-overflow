package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
	soak := flags.Bool("soak", false, "boot the soak shell (autopilot + soak defaults) instead of the harness shell")
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

	dataRoot, err := upDataRoot(*dataDir, *soak)
	if err != nil {
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
	mode := instanceinfo.ModeHarness
	if *soak {
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
// per-worktree default for the shell being booted. The soak default
// carries its own suffix, exactly as the backend's own flag default
// does — the two must agree or `up --soak` and `make soak-window` would
// mean different instances.
func upDataRoot(flagValue string, soak bool) (string, error) {
	if flagValue != "" {
		abs, err := filepath.Abs(flagValue)
		if err != nil {
			return "", fmt.Errorf("resolve --data-dir %q: %w", flagValue, err)
		}
		return abs, nil
	}
	if soak {
		return instanceinfo.DefaultSoakDataRoot(), nil
	}
	return instanceinfo.DefaultDataRoot(), nil
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
