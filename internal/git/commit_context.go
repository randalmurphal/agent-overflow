package git

import (
	"fmt"
	"strings"
)

// CommitContextLimits caps how large the staged summary / patch sections
// can get before we truncate and append a `[truncated]` marker. Matches
// t3-code's GitCore-layer limits — the prompt layer further trims before
// shipping to the CLI, but we cap here too so an unusually huge staged
// diff doesn't eat memory for no benefit.
const (
	// StagedSummaryLimit bounds the `--name-status` output. File lists
	// grow roughly O(files_changed); 8000 chars covers well over a
	// thousand modified files.
	StagedSummaryLimit = 8_000
	// StagedPatchLimit bounds the raw patch. Large generated files
	// (lockfiles, snapshots) blow through this easily; truncation is
	// fine because the model uses the summary to pick up what the patch
	// doesn't show.
	StagedPatchLimit = 50_000
)

// StagedSummary returns `git diff --cached --name-status` output for the
// working tree at cwd, truncated to StagedSummaryLimit bytes with a
// trailing `[truncated]` marker when the output is larger.
//
// `--no-color --no-ext-diff --no-textconv` is the flag set every diff this
// app parses or feeds to a model carries (see internal/gitdiff and
// mergetree.go for the sibling call sites). They neutralise the three
// pieces of user git config that would otherwise replace git's own output
// with something else: `color.ui = always` (ANSI escapes inline),
// `diff.external` / GIT_EXTERNAL_DIFF (a third-party differ's output, or a
// GUI that launches and never returns), and `diff.*.textconv` (a filter's
// rendering of the file instead of its content).
func (c *Core) StagedSummary(cwd string) (string, error) {
	result, err := c.run(cwd, "diff", "--cached", "--name-status",
		"--no-color", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("git diff --cached --name-status failed: %s", strings.TrimSpace(result.stderr))
	}
	return limitSection(result.stdout, StagedSummaryLimit), nil
}

// StagedPatch returns `git diff --cached --patch --minimal` output,
// truncated to StagedPatchLimit bytes. `--minimal` biases toward smaller
// patches when the default heuristic would produce a larger one. See
// StagedSummary for why the three `--no-*` flags are mandatory here.
func (c *Core) StagedPatch(cwd string) (string, error) {
	result, err := c.run(cwd, "diff", "--cached", "--patch", "--minimal",
		"--no-color", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		return "", fmt.Errorf("git diff --cached --patch failed: %s", strings.TrimSpace(result.stderr))
	}
	return limitSection(result.stdout, StagedPatchLimit), nil
}

// limitSection caps s at maxBytes, appending a "\n\n[truncated]" marker
// so the reader (human or model) knows bytes were dropped. Mirrors
// t3-code's limitSection in git/Utils.ts.
func limitSection(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "\n\n[truncated]"
}
