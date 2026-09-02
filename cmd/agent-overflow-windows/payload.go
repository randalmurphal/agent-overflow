//go:build windows

// payload.go owns embedded Linux backend management — installing the
// payload into the chosen WSL distro and resolving paths inside the
// distro. The picker / launcher main flow drives `ensurePayloadInstalled`
// which handles version-skew checks.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"
)

// ensurePayloadInstalled writes the embedded Linux binary into the
// distro if either (a) we've never installed in this distro or (b)
// the embedded payload version has changed since the last install.
// Returns the resolved bin path so the caller can pass it straight to
// wsllauncher.Launch without paying for a second wsl.exe round-trip.
//
// We could also `--version` the on-disk binary and compare against
// the embed, but the JSON-tracked version is simpler and matches
// what the Phase D spec asks for.
//
// Persisting the install-success state is deferred to
// persistSuccessfulLaunch so a fresh install followed by a Launch
// failure doesn't trap the user on a saved-but-broken distro on next
// boot.
// The returned `cached` is true when the path came from wsl.json without
// asking WSL, which is the case the caller must be ready to re-resolve
// (see launchAndShow's stale-path retry).
func (a *launcherApp) ensurePayloadInstalled(ctx context.Context, distro string) (binPath string, cached bool, err error) {
	started := time.Now()
	defer logBootPhase("launcher.payload.total", started)

	phaseStarted := time.Now()
	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &wsldistro.Config{}
	}
	logBootPhase("launcher.payload.load_config", phaseStarted)
	installed := cfg.InstalledVer == payloadVersion && cfg.InstalledDistro == distro
	if path := cachedPayloadPath(cfg, distro, payloadVersion); path != "" {
		// The common warm-restart case: nothing to install and the path is
		// on record, so no wsl.exe process is spawned at all here.
		log.Printf("boot: phase=launcher.payload.install skipped=true version=%q distro=%q path=recorded", payloadVersion, distro)
		return path, true, nil
	}

	phaseStarted = time.Now()
	binPath, err = wslHomePath(ctx, distro)
	logBootPhase("launcher.payload.wsl_home", phaseStarted)
	if err != nil {
		return "", false, err
	}
	if installed {
		// Installed by a launcher that predates the recorded path; the
		// post-launch persist records it for the next boot.
		log.Printf("boot: phase=launcher.payload.install skipped=true version=%q distro=%q path=resolved", payloadVersion, distro)
		return binPath, false, nil
	}
	if err := installPayload(ctx, distro, binPath); err != nil {
		return "", false, err
	}
	return binPath, false, nil
}

// cachedPayloadPath returns the install path wsl.json recorded, when that
// record is for exactly this payload version and distro. Anything else is
// "" and the caller resolves the path through WSL.
func cachedPayloadPath(cfg *wsldistro.Config, distro, version string) string {
	if cfg == nil || cfg.InstalledVer != version || cfg.InstalledDistro != distro {
		return ""
	}
	return strings.TrimSpace(cfg.InstalledBinPath)
}

// installPayload writes the embedded Linux binary to binPath inside the
// distro. Shared by the first install and the stale-path retry.
func installPayload(ctx context.Context, distro, binPath string) error {
	phaseStarted := time.Now()
	tmp, err := writeEmbeddedPayload()
	logBootPhase("launcher.payload.write_embedded", phaseStarted)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	phaseStarted = time.Now()
	err = wsllauncher.InstallPayload(ctx, distro, tmp, binPath)
	logBootPhase("launcher.payload.install", phaseStarted)
	return err
}

// writeEmbeddedPayload drops the //go:embed payload to a temp file
// on the Windows side. InstallPayload then reaches into that path
// from inside WSL via the /mnt/c automount.
func writeEmbeddedPayload() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	cacheDir := filepath.Join(dir, "agent-overflow")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(cacheDir, "agent-overflow-linux-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(linuxPayload); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// wslHomePath asks WSL for the user's HOME and joins the relative
// path. We resolve at runtime because installs aren't always under
// /home/<distro-default> — devcontainer-style images use /root, and
// some users mount homedirs from /mnt.
func wslHomePath(ctx context.Context, distro string) (string, error) {
	// Inline the resolution rather than adding a public function on
	// the wsllauncher package — it's a one-call helper that doesn't
	// belong in the launcher's stable surface.
	raw, err := runWSLOutput(ctx, distro, "/bin/sh", "-c", "echo $HOME")
	if err != nil {
		return "", fmt.Errorf("resolve WSL HOME in %q: %w", distro, err)
	}
	out := strings.TrimSpace(strings.TrimRight(raw, "\r\n"))
	if out == "" {
		return "", fmt.Errorf("resolve WSL HOME in %q: wsl.exe returned empty stdout", distro)
	}
	return out + "/.local/bin/agent-overflow", nil
}

// runWSLOutput is a thin wsl.exe -d <distro> --exec <cmd> wrapper that
// returns stdout as a string. Errors include the captured stderr so
// callers can surface useful diagnostics ("distro not found",
// "WSL2 vmcompute is broken", etc) to the user rather than silently
// swallowing them.
//
// --exec, never --: the -- form re-parses the joined argv through the
// user's login shell, which mangles quoting and pre-expands `$`
// references (wsllauncher.buildLaunchArgs has the incident note).
// Callers that need shell semantics pass an explicit /bin/sh -c.
//
// HideWindow keeps wsl.exe from flashing a console window every time
// we invoke it; the launcher's UI surface is the WebView, never the
// child's console.
func runWSLOutput(ctx context.Context, distro string, args ...string) (string, error) {
	full := append([]string{"-d", distro, "--exec"}, args...)
	cmd := exec.CommandContext(ctx, "wsl.exe", full...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	var stderr strings.Builder
	cmd.Stderr = &stderr

	b, err := cmd.Output()
	if err != nil {
		// wsl.exe writes errors to stderr in UTF-16 LE; the raw bytes
		// are still useful in the log even if not prettily decoded.
		// Callers see the wrapped error verbatim.
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			return "", fmt.Errorf("wsl.exe %v: %w (stderr: %s)", full, err, stderrText)
		}
		return "", fmt.Errorf("wsl.exe %v: %w", full, err)
	}
	return string(b), nil
}
