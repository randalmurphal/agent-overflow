package app

import (
	"os/exec"
	"strings"

	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
)

// codexBinary is the binary every Codex spawn runs: the harness mock, an
// explicit path setting as given, or the default setting's resolution.
func (a *App) codexBinary() codex.NativeBinary {
	if a.providerBinaryOverride != "" {
		return codex.NativeBinary{Path: a.providerBinaryOverride}
	}
	if path := strings.TrimSpace(a.currentSettings().CodexBinaryPath); path != "" && path != settings.DefaultSettings.CodexBinaryPath {
		return codex.NativeBinary{Path: path}
	}
	return a.defaultCodexBinary()
}

// defaultCodexBinary is what the default `codex` setting runs. An npm
// install puts a Node wrapper on PATH whose only job is to spawn its vendored
// native binary; running that binary directly saves the Node process every
// Codex thread, probe and one-shot would otherwise carry. A command that does
// not resolve, or is not a recognized wrapper, stays the bare default, so
// lookup and its errors read as before. An explicit path setting never comes
// here.
//
// Resolved per call rather than cached: an npm upgrade, uninstall or Node
// version switch moves or removes the native binary, and a stale answer would
// fail a spawn the wrapper would have served.
func (a *App) defaultCodexBinary() codex.NativeBinary {
	command := settings.DefaultSettings.CodexBinaryPath
	lookPath := a.codexLookPathFn
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	resolved, err := lookPath(command)
	if err != nil {
		return codex.NativeBinary{Path: command}
	}
	native := codex.ResolveNativeBinary(resolved)
	if native.Wrapper == "" {
		return codex.NativeBinary{Path: command}
	}
	return native
}

// codexWrapperFor names the npm wrapper the Codex binary at binaryPath was
// resolved from, "" when Codex runs binaryPath as configured. The settings
// page shows it beside the binary that runs.
func (a *App) codexWrapperFor(binaryPath string) string {
	if native := a.codexBinary(); native.Path == binaryPath {
		return native.Wrapper
	}
	return ""
}
