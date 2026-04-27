//go:build windows

// payload.go owns embedded Linux backend management — installing the
// payload into the chosen WSL distro and resolving paths inside the
// distro. The picker / launcher main flow drives `ensurePayloadInstalled`
// which handles version-skew checks.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"
)

// ensurePayloadInstalled writes the embedded Linux binary into the
// distro if either (a) we've never installed in this distro or (b)
// the embedded payload version has changed since the last install.
//
// We could also `--version` the on-disk binary and compare against
// the embed, but the JSON-tracked version is simpler and matches
// what the Phase D spec asks for.
//
// Persisting the install-success state is deferred to
// persistSuccessfulLaunch so a fresh install followed by a Launch
// failure doesn't trap the user on a saved-but-broken distro on next
// boot.
func (a *launcherApp) ensurePayloadInstalled(ctx context.Context, distro string) error {
	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &wsldistro.Config{}
	}
	if cfg.InstalledVer == payloadVersion && cfg.InstalledDistro == distro {
		return nil
	}

	tmp, err := writeEmbeddedPayload()
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	wslPath, err := wslHomePath(ctx, distro)
	if err != nil {
		return err
	}

	if err := wsllauncher.InstallPayload(ctx, distro, tmp, wslPath); err != nil {
		return err
	}
	return nil
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
	if _, err := f.Write(linuxPayload); err != nil {
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

// runWSLOutput is a thin wsl.exe -d <distro> -- <cmd> wrapper that
// returns stdout as a string. Errors include the captured stderr so
// callers can surface useful diagnostics ("distro not found",
// "WSL2 vmcompute is broken", etc) to the user rather than silently
// swallowing them.
//
// HideWindow keeps wsl.exe from flashing a console window every time
// we invoke it; the launcher's UI surface is the WebView, never the
// child's console.
func runWSLOutput(ctx context.Context, distro string, args ...string) (string, error) {
	full := append([]string{"-d", distro, "--"}, args...)
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
