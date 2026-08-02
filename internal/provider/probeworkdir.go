package provider

import (
	"fmt"
	"path/filepath"
)

// ValidateProbeWorkDir enforces that an account probe was given an explicit,
// absolute working directory.
//
// Both CLIs discover configuration by walking up from their cwd, and that
// configuration can change the answer a probe returns: a project-scoped
// `.claude/settings.json` env block (CLAUDE_CODE_USE_BEDROCK and friends)
// repoints the CLI at a different backend, so "who is logged in" is a
// cwd-dependent question. An empty WorkDir does not mean "no directory" —
// it means the probe inherits whatever directory the app process happens to
// have been launched from, which is a Finder/Explorer default in one
// install and a developer's Bedrock repo in another. The cached answer is
// then attributed to every workspace.
//
// So the field is required rather than optional-and-ignored: a caller must
// state which environment it is asking about, and ProbeCacheKey carries the
// same value so no two environments can share an entry. Relative paths are
// refused for the same reason an empty one is — they resolve against the
// inherited cwd this rule exists to eliminate.
func ValidateProbeWorkDir(providerName, workDir string) error {
	if workDir == "" {
		return fmt.Errorf(
			"%s: probe WorkDir is required (an inherited cwd makes the answer depend on where the app was launched)",
			providerName,
		)
	}
	if !filepath.IsAbs(workDir) {
		return fmt.Errorf("%s: probe WorkDir must be absolute, got %q", providerName, workDir)
	}
	return nil
}
