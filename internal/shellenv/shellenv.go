// Package shellenv probes the user's login shell for PATH (and other
// shell-managed environment variables) and merges the result into the
// running process's environment.
//
// Why: when this binary is launched outside an interactive terminal —
// e.g. the WSL-side backend spawned by `wsl.exe -d <distro> -- <bin>`,
// a macOS .app double-clicked from Finder, or a Linux desktop entry —
// the inherited PATH is the minimal default the OS hands out, not the
// PATH the user's shell builds via .profile / .bashrc / .zshrc. As a
// result, exec.LookPath misses everything installed via nvm, asdf,
// volta, ~/.local/bin, ~/.npm-global/bin, and so on.
//
// Probe the user's login-interactive shell and merge its PATH entries into
// the inherited environment.
//
// Public API is a single function: Sync. Errors are best-effort —
// callers log them and proceed with the unmodified PATH. There is
// nothing here that should ever block startup.
package shellenv

import "context"

// Sync probes the user's login shell for PATH and merges the result
// into os.Getenv("PATH"). It is a no-op on Windows (the Windows .exe
// launcher in cmd/agent-overflow-windows never spawns provider
// children — providers run inside the WSL Linux backend, where this
// package is doSync()'d on startup).
//
// Best-effort: any failure (no shell, shell exited non-zero, sentinel
// markers missing, etc.) returns an error and leaves PATH untouched.
// Callers should log and proceed.
func Sync(ctx context.Context) error {
	return doSync(ctx)
}
