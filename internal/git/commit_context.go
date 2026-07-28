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
func (c *Core) StagedSummary(cwd string) (string, error) {
	result, err := c.run(cwd, "diff", "--cached", "--name-status")
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
// patches when the default heuristic would produce a larger one.
func (c *Core) StagedPatch(cwd string) (string, error) {
	result, err := c.run(cwd, "diff", "--cached", "--patch", "--minimal")
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
