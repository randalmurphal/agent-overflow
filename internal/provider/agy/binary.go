// Package agy implements the Google Antigravity (`agy acp`) provider adapter
// on top of the shared multiplexed ACP v1 client (internal/provider/acp).
package agy

import (
	"os"
	"os/exec"
	"runtime"
	"strings"

	"agent-overflow/internal/provider"
)

const (
	DefaultBinaryName  = "agy"
	DefaultAppDataName = "antigravity-cli"
)

// LaunchArgs returns the CLI arguments for spawning a shared multiplexed
// `agy acp` server. Intentionally omits `--process-per-session` (measured 16x
// lower per-session memory with verified same-name HTTP MCP & cwd isolation)
// and omits `--dangerously-skip-permissions` so every AO RuntimeMode and plan
// mode switches live without restarting the server.
func LaunchArgs(geminiDir, appDataSubdir string) []string {
	args := []string{"acp"}
	if strings.TrimSpace(geminiDir) != "" {
		args = append(args, "--gemini-dir", geminiDir)
		if strings.TrimSpace(appDataSubdir) != "" {
			args = append(args, "--app-data-dir", appDataSubdir)
		}
	}
	return args
}

// LaunchEnv adds what every `agy acp` process runs with to in: CLI
// auto-update pinned off, so AO's providerupdate service owns upgrades, and
// the ambient GCP/Gemini credentials a shell exports cleared, so they cannot
// hijack a Google OAuth session. A value the user's custom environment sets
// for one of them still applies.
func LaunchEnv(in provider.EnvInput) provider.EnvInput {
	return in.Pin(provider.AgyDisableAutoUpdateEnv, "1").Clear(provider.AgyAmbientCredentialEnvs...)
}

// WithFileStorage pins file-backed OAuth token storage, so a managed or
// isolated account's token lands in its app-data directory instead of the
// user's global OS keyring slot.
func WithFileStorage(in provider.EnvInput) provider.EnvInput {
	return in.Pin(provider.AgyForceFileStorageEnv, "1").
		Pin(provider.AgySSHConnectionEnv, "127.0.0.1 0 127.0.0.1 0")
}

// ResolveBinary returns the configured agy binary path, falling back to PATH
// and macOS Antigravity.app bundle locations when using the default name.
func ResolveBinary(configured string) string {
	trimmed := strings.TrimSpace(configured)
	if trimmed != "" && trimmed != DefaultBinaryName {
		return trimmed
	}
	if p, err := exec.LookPath(DefaultBinaryName); err == nil && p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		for _, candidate := range []string{
			"/Applications/Antigravity.app/Contents/Resources/app/bin/agy",
			"/usr/local/bin/agy",
			"/opt/homebrew/bin/agy",
		} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return DefaultBinaryName
}
