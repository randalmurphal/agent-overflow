package git

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxUntrackedScanBytes caps how much untracked-file content untrackedStats
// reads while counting "new file" insertions. It is deliberately generous -
// agent-driven workspaces routinely carry hundreds of untracked source files -
// and exists only to bound a pathological un-ignored tree (a build/data
// directory someone forgot to ignore, or a multi-GB log) from stalling the
// gitwatch hot path. The file *count* is unaffected by the cap; only the line
// tally stops once the budget is spent.
//
// This bound differs from the panel's on purpose: DiffWorkspaceVsHead errors
// out at its 10MB patch cap, whereas the badge degrades (caps the line tally,
// keeps the file count). On a pathological tree the panel shows an error while
// the badge shows a bounded number - but the panel isn't displaying a count to
// match in that state anyway, so the divergence is invisible in practice.
const maxUntrackedScanBytes = 64 * 1024 * 1024

// untrackedStats returns the insertions and file count the diff panel
// attributes to untracked, non-ignored files in cwd: every such file is "added"
// whole versus /dev/null. Counting happens here in Go rather than via a `git
// diff` per file because gitwatch runs this every 250ms-1500ms and a workspace
// can hold hundreds of untracked files - one subprocess each would swamp the
// watcher. New files have no deletions, so only insertions are returned.
//
// budget caps the total content bytes read across the whole scan (the caller
// passes maxUntrackedScanBytes); the file count is never capped - only the line
// tally stops once budget is spent. Passing it in rather than reading the const
// directly lets tests drive the budget-exhausted path without a giant fixture.
//
// Best-effort by design: a failed enumeration or a file that vanishes mid-scan
// (routine while an agent is writing) is skipped, so the ambient badge degrades
// gracefully rather than erroring - matching how branch/forge/pending degrade
// in baseStatus.
func (c *Core) untrackedStats(cwd string, budget int) (insertions, files int) {
	listing, err := c.run(cwd, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || listing.exitCode != 0 {
		return 0, 0
	}

	for rel := range strings.SplitSeq(listing.stdout, "\x00") {
		if rel == "" {
			continue
		}
		files++
		if budget <= 0 {
			continue // keep counting files; just stop tallying lines
		}
		ins, read := countUntrackedFileLines(filepath.Join(cwd, rel), budget)
		insertions += ins
		budget -= read
	}
	return insertions, files
}

// countUntrackedFileLines returns the added-line count git's numstat would
// report for the untracked entry at path, plus the bytes read (so the caller
// can debit its scan budget). It matches git's accounting without following
// links or opening special files - either of which would let the gitwatch hot
// path read outside the workspace or block forever on a FIFO/device:
//   - a symlink is counted as its target *text* (one line), exactly as git's
//     mode-120000 diff does - resolved via Lstat + Readlink, never opened;
//   - any non-regular entry (FIFO, socket, device, directory) counts 0;
//   - a regular file is read (capped at budget) and counted by countAddedLines.
//
// Divergence from the panel on symlinks whose target is not a regular file:
// the panel runs `git diff --no-index /dev/null <symlink>`, which follows the
// link - so a symlink to a directory errors out (contributing 0) and a symlink
// to a FIFO would block on open (the panel never finishes that file). This
// counter always reports the link text (1 line) and never touches the target,
// so the badge can read up to 1 line higher per such symlink. That is a
// deliberate robustness tradeoff: the hot path must never follow a link off the
// workspace or hang on a device. (A symlink to a regular file matches the panel:
// both count the one-line link text, since git --no-index does not follow it.)
func countUntrackedFileLines(path string, budget int) (insertions, bytesRead int) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0 // vanished mid-scan - routine while an agent is writing
	}
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return 0, 0
		}
		return countAddedLines([]byte(target)), 0
	}
	if !mode.IsRegular() {
		return 0, 0
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(budget)))
	if err != nil {
		return 0, len(data)
	}
	return countAddedLines(data), len(data)
}

// countAddedLines counts the lines git's numstat would attribute to data added
// whole (a new file versus /dev/null). It mirrors git's default content
// detection: a NUL byte within the first 8000 bytes (git's own probe window)
// marks the data binary - numstat "-", zero lines - otherwise every newline is
// a line, plus one more for a final line with no trailing newline.
//
// This must stay aligned with the diff panel's untracked-file count, which is a
// different code path: the panel sums the frontend's parse
// (frontend/src/lib/utils/patchFiles.ts) of `git diff --no-index --patch
// /dev/null <file>`, counting every '+' line that is not the '+++' header -
// which equals the newline count here (the "\ No newline" marker and "Binary
// files differ" lines start with '\'/'B', so neither side counts them).
// TestCountAddedLinesMatchesPanelParse and the panel-total assertion in
// TestStatusInsertionsIncludeUntracked pin the two together; keep them in sync.
//
// Known divergences (rare, documented rather than fixed):
//   - .gitattributes content overrides (binary, -text, -diff): the panel uses
//     real `git diff` and honours them; this raw heuristic does not.
//   - a file content line beginning with "++" (two or more '+'): git's patch
//     adds one more '+' prefix, so the panel sees "+++..." and the frontend's
//     startsWith('+++') header-skip drops it - the panel undercounts while this
//     counter is correct. The mirror "---" case cannot occur for an untracked
//     file: every content line is an addition prefixed "+", never "-".
func countAddedLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	probe := data
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return 0
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}
