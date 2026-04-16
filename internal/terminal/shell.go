package terminal

import (
	"os"
)

// resolveShell picks a shell binary and its argv. An explicit Shell wins; a
// non-empty SHELL env var is next; the final fallback is /bin/sh.
//
// args default to the empty slice so callers get a login-style interactive
// shell only when they explicitly pass -l or equivalent. This keeps test
// behaviour deterministic.
func resolveShell(shell string, args []string) (string, []string) {
	if shell == "" {
		if env := os.Getenv("SHELL"); env != "" {
			shell = env
		} else {
			shell = "/bin/sh"
		}
	}
	if args == nil {
		args = []string{}
	}
	return shell, args
}
