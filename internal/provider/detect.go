package provider

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const versionTimeout = 5 * time.Second

// ProviderStatus describes whether a provider binary is available and its version.
type ProviderStatus struct {
	Provider   string `json:"provider"`
	Installed  bool   `json:"installed"`
	Version    string `json:"version,omitempty"`
	BinaryPath string `json:"binaryPath"`
	Status     string `json:"status"`           // "ready", "not_found", "error"
	Message    string `json:"message,omitempty"`
}

// DetectProvider checks whether a provider binary exists on PATH (or at the
// given binaryPath) and attempts to retrieve its version.
//
// name must be "claude" or "codex" — it selects the version-parsing function.
// If the binary is found but the version command fails, the provider is still
// reported as "ready" with an empty version and the error in Message.
func DetectProvider(name, binaryPath string) ProviderStatus {
	resolvedPath, err := exec.LookPath(binaryPath)
	if err != nil {
		return ProviderStatus{
			Provider:   name,
			Installed:  false,
			BinaryPath: binaryPath,
			Status:     "not_found",
			Message:    fmt.Sprintf("binary not found: %s", binaryPath),
		}
	}

	var version string
	var versionErr error

	switch name {
	case string(Claude):
		version, versionErr = DetectClaudeVersion(resolvedPath)
	case string(Codex):
		version, versionErr = DetectCodexVersion(resolvedPath)
	default:
		version, versionErr = runVersionCommand(resolvedPath, "--version")
	}

	status := ProviderStatus{
		Provider:   name,
		Installed:  true,
		BinaryPath: resolvedPath,
		Status:     "ready",
		Version:    version,
	}

	if versionErr != nil {
		status.Message = fmt.Sprintf("version check failed: %v", versionErr)
	}

	return status
}

// DetectClaudeVersion runs "<binaryPath> --version" and returns the trimmed output.
func DetectClaudeVersion(binaryPath string) (string, error) {
	return runVersionCommand(binaryPath, "--version")
}

// DetectCodexVersion runs "<binaryPath> --version" and returns the trimmed output.
func DetectCodexVersion(binaryPath string) (string, error) {
	return runVersionCommand(binaryPath, "--version")
}

// runVersionCommand executes a binary with the given args under a 5-second timeout
// and returns the trimmed stdout.
func runVersionCommand(binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
