package git

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// untrackedCacheTTL bounds how long a workspace's line cache survives
// without a scan touching it. Closed workspaces stop scanning, so their
// entries age out on the next store's sweep instead of pinning one map
// per historical cwd for the process lifetime.
const untrackedCacheTTL = 30 * time.Minute

// untrackedLineEntry memoizes one regular file's added-line count keyed
// to its (size, mtime) at read time - the same stat-based change
// detection git's own index uses. Only fully-read files are cached: a
// budget-truncated read carries a partial tally that must not be
// replayed as if complete.
type untrackedLineEntry struct {
	size    int64
	mtimeNs int64
	lines   int
}

// untrackedLineCache is one workspace's rel-path → entry map plus the
// recency stamp the TTL sweep reads. The files map is immutable after
// store: every scan builds a fresh map and replaces it wholesale, so a
// snapshot returned to a concurrent scan is always safe to read.
type untrackedLineCache struct {
	lastUsed time.Time
	files    map[string]untrackedLineEntry
}

// untrackedStats returns the insertions and file count the diff panel
// attributes to untracked, non-ignored files in cwd: every such file is "added"
// whole versus /dev/null. Counting happens here in Go rather than via a `git
// diff` per file because gitwatch runs this every 250ms-1500ms and a workspace
// can hold hundreds of untracked files - one subprocess each would swamp the
// watcher.
//
// The per-file tallies are memoized against (size, mtime): a refresh in a
// workspace whose untracked files did not change costs one stat per file and
// zero content reads, instead of re-reading every file's bytes on every
// refresh (which profiled at GBs of read churn per hour under agent
// activity). New files have no deletions, so only insertions are returned.
//
// budget caps the total content bytes read across the whole scan (the caller
// passes maxUntrackedScanBytes); the file count is never capped - only the line
// tally stops once budget is spent. Cache hits consume no budget - the budget
// bounds I/O, not accuracy - so on a tree too large to read in one scan the
// tally converges upward across scans as earlier files' counts replay from
// cache and the freed budget reaches files beyond the previous cutoff. Passing
// budget in rather than reading the const directly lets tests drive the
// budget-exhausted path without a giant fixture.
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

	prev := c.untrackedLineCacheSnapshot(cwd)
	next := make(map[string]untrackedLineEntry, len(prev))

	for rel := range strings.SplitSeq(listing.stdout, "\x00") {
		if rel == "" {
			continue
		}
		files++
		if budget <= 0 {
			continue // keep counting files; just stop tallying lines
		}
		path := filepath.Join(cwd, rel)
		info, err := os.Lstat(path)
		if err != nil {
			continue // vanished mid-scan - routine while an agent is writing
		}
		if entry, ok := prev[rel]; ok && info.Mode().IsRegular() &&
			entry.size == info.Size() && entry.mtimeNs == info.ModTime().UnixNano() {
			next[rel] = entry
			insertions += entry.lines
			continue
		}
		ins, read := countUntrackedFileLines(info, path, budget)
		insertions += ins
		budget -= read
		// Cache only complete reads of regular files. read < size means
		// the budget truncated the tally; a mid-write size change makes
		// the entry self-invalidate via the mtime/size key next scan.
		if info.Mode().IsRegular() && int64(read) == info.Size() {
			next[rel] = untrackedLineEntry{
				size:    info.Size(),
				mtimeNs: info.ModTime().UnixNano(),
				lines:   ins,
			}
		}
	}
	c.storeUntrackedLineCache(cwd, next)
	return insertions, files
}

// untrackedLineCacheSnapshot returns cwd's current entry map (nil when
// absent). The map is replace-only (see untrackedLineCache), so reading
// it outside the lock is safe.
func (c *Core) untrackedLineCacheSnapshot(cwd string) map[string]untrackedLineEntry {
	c.untrackedMu.Lock()
	defer c.untrackedMu.Unlock()
	if cache, ok := c.untrackedLines[cwd]; ok {
		return cache.files
	}
	return nil
}

// storeUntrackedLineCache replaces cwd's entry map wholesale - entries
// for files no longer untracked simply aren't in the new map, so the
// cache self-cleans per scan - and sweeps workspaces whose last scan is
// past untrackedCacheTTL. Concurrent scans of the same cwd (a watcher
// refresh racing an RPC Status call) both store complete maps; last
// writer wins and neither map is ever mutated after store.
func (c *Core) storeUntrackedLineCache(cwd string, files map[string]untrackedLineEntry) {
	now := c.nowFn()
	c.untrackedMu.Lock()
	defer c.untrackedMu.Unlock()
	c.untrackedLines[cwd] = &untrackedLineCache{lastUsed: now, files: files}
	for key, cache := range c.untrackedLines {
		if now.Sub(cache.lastUsed) > untrackedCacheTTL {
			delete(c.untrackedLines, key)
		}
	}
}

// countUntrackedFileLines returns the added-line count git's numstat would
// report for the untracked entry described by info at path, plus the bytes
// read (so the caller can debit its scan budget). The caller has already
// Lstat'ed the path; this function matches git's accounting without following
// links or opening special files - either of which would let the gitwatch hot
// path read outside the workspace or block forever on a FIFO/device:
//   - a symlink is counted as its target *text* (one line), exactly as git's
//     mode-120000 diff does - resolved via Readlink, never opened;
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
func countUntrackedFileLines(info os.FileInfo, path string, budget int) (insertions, bytesRead int) {
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
		return 0, 0 // vanished mid-scan - routine while an agent is writing
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
