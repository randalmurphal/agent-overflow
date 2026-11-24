package provider

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"agent-overflow/internal/appimage"
)

const versionTimeout = 5 * time.Second

// ProviderStatus describes whether a provider binary is available and its version.
//
// Status values:
//   - "ready"           — binary resolved and passed every version check.
//   - "not_found"       — exec.LookPath(binaryPath) failed.
//   - "version_too_old" — binary ran but its version is below the minimum we support
//     (Codex: see MinimumCodexCLIVersion).
//   - "unauthenticated" — binary is present and new enough but has no logged-in
//     account (Claude: ProbeAccount returned a zero-value AccountInfo).
//   - "error"           — the version check itself failed (binary exited non-zero,
//     garbled output, etc.).
type ProviderStatus struct {
	Provider   string `json:"provider"`
	Installed  bool   `json:"installed"`
	Version    string `json:"version,omitempty"`
	BinaryPath string `json:"binaryPath"`
	// WrapperPath is the launcher on PATH that BinaryPath was resolved
	// from, when Agent Overflow runs the binary that launcher would spawn
	// (the npm Codex wrapper's native binary). Empty otherwise.
	WrapperPath string `json:"wrapperPath,omitempty"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
}

// DetectProvider checks whether a provider binary exists on PATH (or at the
// given binaryPath) and attempts to retrieve its version.
//
// If the binary is found but the version command fails, the provider is
// reported as installed-but-unusable with Status="error".
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

	version, versionErr := runVersionCommand(resolvedPath, "--version")

	status := ProviderStatus{
		Provider:   name,
		Installed:  true,
		BinaryPath: resolvedPath,
		Status:     "ready",
		Version:    version,
	}

	if versionErr != nil {
		status.Status = "error"
		status.Message = fmt.Sprintf("version check of %s failed: %v", resolvedPath, versionErr)
		return status
	}

	if name == string(Codex) {
		// Output with no version in it says nothing about the build, so the
		// status stays ready; the app-server's own `initialize` is what
		// admits or refuses the process that runs.
		if parsed := ParseCodexCLIVersion(version); parsed != "" {
			if err := CheckCodexVersion(resolvedPath, parsed); err != nil {
				status.Status = "version_too_old"
				status.Message = err.Error()
			}
		}
	}

	return status
}

// runVersionCommand executes a binary with the given args under a 5-second timeout
// and returns the trimmed stdout.
func runVersionCommand(binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	// The version probe wants the inherited environment, minus the AppImage
	// launch artifacts a provider CLI would otherwise resolve its own runtime
	// against. nil outside an AppImage launch, which keeps exec.Cmd on its own
	// inherit path.
	cmd.Env = appimage.ScrubInherited()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
