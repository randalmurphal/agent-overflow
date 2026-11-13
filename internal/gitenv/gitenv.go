// Package gitenv holds the environment that keeps a git child from asking a
// person for credentials. Every package that spawns git builds the child's
// environment through NonInteractive (TestEveryGitSpawnIsNonInteractive).
// internal/git alone leaves it off, for the network commands a user asked
// for (commandSpec.allowCredentialPrompt).
package gitenv

import (
	"slices"
	"strings"

	"agent-overflow/internal/platform"
)

// nonInteractive closes every channel a git child can ask a person through.
// A remote that needs credentials then fails with git's own diagnosis
// ("could not read Username ...: terminal prompts disabled") instead of a
// Git Credential Manager or ssh-askpass window, or a hang on a terminal
// nobody is reading.
//
// Each entry closes a different channel:
//
//   - GIT_TERMINAL_PROMPT=0 stops the terminal prompt. It is not enough
//     alone: git tries an askpass helper first.
//   - GIT_ASKPASS= (empty, not unset) disables that helper. git reads
//     GIT_ASKPASS, core.askpass and SSH_ASKPASS in order and stops at the
//     first one that is set, so an empty value also shadows the other two.
//   - SSH_ASKPASS= and SSH_ASKPASS_REQUIRE=never cover ssh remotes, where
//     ssh prompts rather than git.
//   - GCM_INTERACTIVE=never stops Git Credential Manager from opening its
//     own sign-in window, which it does before git's prompt chain runs.
var nonInteractive = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=",
	"SSH_ASKPASS=",
	"SSH_ASKPASS_REQUIRE=never",
	"GCM_INTERACTIVE=never",
}

// windowsNames are the nonInteractive variables listed in WSLENV. Under WSL
// the credential helper is usually the Windows git-credential-manager.exe,
// and a Windows program started from WSL sees only the variables WSLENV
// names. Empty values are left out: WSL does not carry them, and the empty
// askpass entries concern git's own prompt chain on this side.
var windowsNames = func() []string {
	var names []string
	for _, entry := range nonInteractive {
		if name, value, _ := strings.Cut(entry, "="); value != "" {
			names = append(names, name)
		}
	}
	return names
}()

// NonInteractive returns env with every credential prompt closed, for a git,
// gh, glab or ssh child. The entries are appended, so they override the same
// variables inherited in env, and an existing WSLENV keeps its entries. Off
// WSL the WSLENV entry does nothing.
func NonInteractive(env []string) []string {
	env = append(slices.Clip(env), nonInteractive...)
	return platform.AppendWSLENV(env, windowsNames...)
}
