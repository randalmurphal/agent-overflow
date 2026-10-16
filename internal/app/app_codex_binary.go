package app

import (
	"os"
	"os/exec"
	"strings"
	"sync"

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

// defaultCodexBinaryCache holds the default setting's resolution for the PATH
// it was made under. Every Codex spawn, model lookup and capability check
// reads the binary, and resolving it walks PATH, which on WSL includes the
// Windows directories. The cache is dropped by the binary watch tick, which
// re-resolves within providerBinaryWatchInterval of an install, upgrade or
// removal, by a change to the setting and by a provider status read.
type defaultCodexBinaryCache struct {
	mu      sync.Mutex
	valid   bool
	pathEnv string
	binary  codex.NativeBinary
}

// defaultCodexBinary is what the default `codex` setting runs. An npm
// install puts a Node wrapper on PATH whose only job is to spawn its vendored
// native binary; running that binary directly saves the Node process every
// Codex thread, probe and one-shot would otherwise carry. A command that does
// not resolve, or is not a recognized wrapper, stays the bare default, so
// lookup and its errors read as before. An explicit path setting never comes
// here.
//
// A cached native binary is confirmed with one stat before it is returned:
// an npm uninstall or a Node version switch removes it, and spawning the
// missing file would fail where the wrapper or a fresh lookup could serve.
func (a *App) defaultCodexBinary() codex.NativeBinary {
	cache := &a.defaultCodexBinaryCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	pathEnv := os.Getenv("PATH")
	if cache.valid && cache.pathEnv == pathEnv && (cache.binary.Wrapper == "" || codex.IsExecutableFile(cache.binary.Path)) {
		return cache.binary
	}
	binary := a.resolveDefaultCodexBinary()
	// The lookup reads PATH again. An answer made while PATH changed belongs
	// to neither value, so it is returned and not kept.
	if os.Getenv("PATH") != pathEnv {
		cache.valid = false
		return binary
	}
	cache.binary, cache.pathEnv, cache.valid = binary, pathEnv, true
	return binary
}

func (a *App) resolveDefaultCodexBinary() codex.NativeBinary {
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

// invalidateDefaultCodexBinary makes the next read resolve the default
// setting again.
func (a *App) invalidateDefaultCodexBinary() {
	a.defaultCodexBinaryCache.mu.Lock()
	a.defaultCodexBinaryCache.valid = false
	a.defaultCodexBinaryCache.mu.Unlock()
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
