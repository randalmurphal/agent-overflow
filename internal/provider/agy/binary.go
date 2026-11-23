// Package agy implements the Google Antigravity (`agy acp`) provider adapter
// on top of the shared multiplexed ACP v1 client (internal/provider/acp).
package agy

import (
	"maps"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const (
	DefaultBinaryName  = "agy"
	DefaultAppDataName = "antigravity-cli"
)

// ambientCredentialEnvVars are scrubbed from agy subprocess environments unless
// explicitly configured by the user so ambient shell GCP/Gemini API exports do
// not hijack Google OAuth sessions.
var ambientCredentialEnvVars = []string{
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_CLOUD_LOCATION",
	"GOOGLE_GENAI_USE_VERTEXAI",
	"GCLOUD_PROJECT",
	"CLOUDSDK_CORE_PROJECT",
}

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

// LaunchEnv returns the merged environment map and unset list for spawning an
// `agy acp` process. Disables CLI auto-update so AO's providerupdate service
// owns upgrades.
func LaunchEnv(customEnv map[string]string, extraEnv map[string]string) (env map[string]string, unset []string) {
	env = make(map[string]string)
	maps.Copy(env, customEnv)
	maps.Copy(env, extraEnv)
	if _, exists := env["AGY_CLI_DISABLE_AUTO_UPDATE"]; !exists {
		env["AGY_CLI_DISABLE_AUTO_UPDATE"] = "1"
	}
	for _, key := range ambientCredentialEnvVars {
		if _, userSet := customEnv[key]; !userSet {
			unset = append(unset, key)
		}
	}
	return env, unset
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
