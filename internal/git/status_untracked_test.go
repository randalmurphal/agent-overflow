package git

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

// TestCountAddedLines pins countAddedLines to git's numstat accounting for a
// file added whole versus /dev/null: a trailing-newline-aware line count, with
// git's binary heuristic (a NUL inside the first 8000 bytes) zeroing the count.
// The two boundary cases at exactly 8000 bytes are the load-bearing ones.
func TestCountAddedLines(t *testing.T) {
	// NUL at index 7999 sits *inside* git's 8000-byte probe window -> binary -> 0.
	nulInProbe := append(bytes.Repeat([]byte("x"), 7999), 0)
	// NUL at index 8000 sits *past* the probe window -> treated as text; the
	// 8000 x's plus the unterminated NUL byte are a single line.
	nulPastProbe := append(bytes.Repeat([]byte("x"), 8000), 0)

	cases := []struct {
		name string
		in   []byte
		want int
	}{
		{"empty", []byte(""), 0},
		{"trailing newline", []byte("a\nb\nc\n"), 3},
		{"no trailing newline", []byte("a\nb\nc"), 3},
		{"single line no newline", []byte("abc"), 1},
		{"lone newline", []byte("\n"), 1},
		{"nul in content", []byte("a\x00b\n"), 0},
		{"nul inside probe window", nulInProbe, 0},
		{"nul past probe window", nulPastProbe, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countAddedLines(tc.in); got != tc.want {
				t.Fatalf("countAddedLines(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestCountUntrackedFileLines covers the per-file counter that backs the
// untracked-insertion tally: regular files (with/without trailing newline,
// binary), the scan-budget cap, and the security-critical symlink case
// (counted as its link text, never followed). The vanished-mid-scan case
// lives at the untrackedStats level now that the caller owns the Lstat.
// The FIFO no-hang guard lives in status_fifo_test.go (needs syscall.Mkfifo).
func TestCountUntrackedFileLines(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	count := func(path string, budget int) (int, int) {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("lstat %s: %v", path, err)
		}
		return countUntrackedFileLines(info, path, budget)
	}

	// Regular text file: every newline is a line; bytesRead is the full size.
	if ins, read := count(write("text.txt", "a\nb\nc\n"), maxUntrackedScanBytes); ins != 3 || read != 6 {
		t.Fatalf("text file: got (%d, %d), want (3, 6)", ins, read)
	}
	// No trailing newline still counts the final partial line.
	if ins, read := count(write("tail.txt", "x\ny"), maxUntrackedScanBytes); ins != 2 || read != 3 {
		t.Fatalf("no-trailing-newline: got (%d, %d), want (2, 3)", ins, read)
	}
	// Binary content (NUL in the probe window) -> 0 lines, but bytes are read.
	if ins, read := count(write("blob.bin", "\x00\x01\x02"), maxUntrackedScanBytes); ins != 0 || read != 3 {
		t.Fatalf("binary file: got (%d, %d), want (0, 3)", ins, read)
	}
	// Budget cap: only `budget` bytes are read, so trailing lines beyond it are
	// not tallied and bytesRead never exceeds the budget. "a\n" -> 1 line, 2 bytes.
	if ins, read := count(write("capped.txt", "a\nb\nc\n"), 2); ins != 1 || read != 2 {
		t.Fatalf("budget cap: got (%d, %d), want (1, 2)", ins, read)
	}

	// A symlink is counted as its target *text* (one line), never followed -
	// the security/parity fix. "text.txt" is an 8-char link with no newline.
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink("text.txt", link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if ins, read := count(link, maxUntrackedScanBytes); ins != 1 || read != 0 {
		t.Fatalf("symlink: got (%d, %d), want (1, 0) - link text counted, target not opened", ins, read)
	}
}

// TestUntrackedStatsBudgetExhaustion proves the whole-scan byte budget caps the
// line tally without ever capping the file count: once budget is spent, every
// remaining untracked file still increments FileCount but contributes zero
// insertions. That is the contract keeping a single huge un-ignored file (a
// forgotten build/data dir, a multi-GB log) from zeroing the count the user
// sees. The const's generous default makes this impractical to hit with a real
// fixture, which is why untrackedStats takes budget as a parameter.
func TestUntrackedStatsBudgetExhaustion(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	// Three identical untracked files, 3 lines / 6 bytes each. Identical content
	// makes the result independent of ls-files iteration order. Backdated so
	// the racy-mtime guard lets the first scan memoize them.
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeRepoFile(t, repo, name, "1\n2\n3\n")
		backdateRepoFile(t, repo, name)
	}

	// Full budget: every file is read and tallied - 3 files x 3 lines.
	if ins, files := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 9 || files != 3 {
		t.Fatalf("full budget: got (%d insertions, %d files), want (9, 3)", ins, files)
	}

	// Tiny budget on a COLD cache (fresh Core): the first file's 1-byte read
	// spends the whole budget, so files 2 and 3 hit the budget<=0
	// short-circuit - counted but not tallied. The file count must survive;
	// only the line tally stops.
	if ins, files := NewCore().untrackedStats(repo, 1); ins != 1 || files != 3 {
		t.Fatalf("budget exhausted: got (%d insertions, %d files), want (1, 3) - "+
			"file count must survive budget exhaustion", ins, files)
	}

	// Tiny budget on a WARM cache: the budget bounds I/O, not accuracy -
	// unchanged files replay their cached tallies without spending budget,
	// so the full count survives a budget that couldn't read a single file.
	if ins, files := core.untrackedStats(repo, 1); ins != 9 || files != 3 {
		t.Fatalf("warm cache + tiny budget: got (%d insertions, %d files), want (9, 3) - "+
			"cache hits must not be budget-gated", ins, files)
	}
}

// TestUntrackedStatsCacheAvoidsReread proves the (size, mtime) memo does its
// job across the state transitions that matter: an unchanged file is never
// re-opened (asserted by making it unreadable - a re-read would tally 0), a
// changed file is re-read, and a deleted file's entry leaves the cache so a
// later re-create with different content is counted fresh.
func TestUntrackedStatsCacheAvoidsReread(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()
	path := filepath.Join(repo, "f.txt")

	writeRepoFile(t, repo, "f.txt", "1\n2\n3\n")
	backdateRepoFile(t, repo, "f.txt")
	if ins, files := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 3 || files != 1 {
		t.Fatalf("first scan: got (%d, %d), want (3, 1)", ins, files)
	}

	// chmod 0000 changes ctime, not (size, mtime): the cache key still
	// matches, so the tally must come from the memo. A regression that
	// re-opens the file gets an EACCES and tallies 0 instead of 3.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if ins, _ := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 3 {
		t.Fatalf("unchanged file: got %d insertions, want 3 from cache without re-reading", ins)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod back: %v", err)
	}

	// Content change (different size) misses the cache and is re-read.
	writeRepoFile(t, repo, "f.txt", "1\n")
	if ins, _ := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 1 {
		t.Fatalf("changed file: got %d insertions, want 1 re-read", ins)
	}

	// Deletion drops the entry (the wholesale-replace store self-cleans)...
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if ins, files := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 0 || files != 0 {
		t.Fatalf("after delete: got (%d, %d), want (0, 0)", ins, files)
	}
	core.untrackedMu.Lock()
	entries := len(core.untrackedLines[repo].files)
	core.untrackedMu.Unlock()
	if entries != 0 {
		t.Fatalf("cache after delete: %d entries, want 0 (wholesale replace must evict)", entries)
	}

	// ...so a re-created file is counted from a fresh read.
	writeRepoFile(t, repo, "f.txt", "1\n2\n")
	if ins, _ := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 2 {
		t.Fatalf("re-created file: got %d insertions, want 2", ins)
	}
}

// TestUntrackedStatsPartialReadNotCached proves a budget-truncated tally is
// never memoized: caching the partial count would replay it forever once the
// budget recovers, permanently under-reporting the file.
func TestUntrackedStatsPartialReadNotCached(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	writeRepoFile(t, repo, "f.txt", "1\n2\n3\n")
	// Budget 2 reads "1\n" only: partial tally of 1.
	if ins, _ := core.untrackedStats(repo, 2); ins != 1 {
		t.Fatalf("truncated scan: got %d insertions, want 1", ins)
	}
	// Full budget must re-read and reach the true count - a cached partial
	// entry would short-circuit to 1 here.
	if ins, _ := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 3 {
		t.Fatalf("recovered budget: got %d insertions, want 3 (partial reads must not be cached)", ins)
	}
}

// TestUntrackedStatsCachedTailSurvivesBudgetExhaustion is the regression
// guard for cache hits being budget-gated: a large uncached file early in
// listing order exhausts the budget, but every later file with a valid memo
// entry must still replay (hits cost no I/O) AND be carried into the next
// cache generation. Gating hits would evict them each scan, permanently
// re-reading the exact content the memo exists to skip and undercounting
// the badge to just the early file.
func TestUntrackedStatsCachedTailSurvivesBudgetExhaustion(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeRepoFile(t, repo, name, "1\n2\n3\n")
		backdateRepoFile(t, repo, name)
	}
	if ins, files := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 9 || files != 3 {
		t.Fatalf("warm-up scan: got (%d, %d), want (9, 3)", ins, files)
	}

	// "0first.txt" sorts before a.txt in ls-files order; budget 2 is spent
	// entirely on its 2 bytes, so a/b/c are reached with budget exhausted.
	writeRepoFile(t, repo, "0first.txt", "x\n")
	ins, files := core.untrackedStats(repo, 2)
	if ins != 10 || files != 4 {
		t.Fatalf("exhausted scan: got (%d insertions, %d files), want (10, 4) - "+
			"cached tail must replay despite an exhausted budget", ins, files)
	}
	core.untrackedMu.Lock()
	entries := len(core.untrackedLines[repo].files)
	core.untrackedMu.Unlock()
	if entries != 3 {
		t.Fatalf("cache after exhausted scan: %d entries, want 3 - "+
			"budget exhaustion must not evict cached entries", entries)
	}
}

// TestUntrackedStatsFreshWriteNotCached pins the racy-mtime guard: a file
// whose mtime is within untrackedRacyWindow of the scan is counted but NOT
// memoized (a same-size rewrite in the same mtime tick would replay a stale
// count forever), and starts being memoized once its mtime quiesces.
func TestUntrackedStatsFreshWriteNotCached(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	writeRepoFile(t, repo, "f.txt", "1\n2\n")
	// Pin "now" just after the file's actual mtime so the assertion
	// cannot flake when a stalled runner puts >2s between the write and
	// the scan's own nowFn capture.
	info, err := os.Lstat(filepath.Join(repo, "f.txt"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	core.nowFn = func() time.Time { return info.ModTime().Add(100 * time.Millisecond) }
	if ins, _ := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 2 {
		t.Fatalf("fresh scan: got %d insertions, want 2", ins)
	}
	core.untrackedMu.Lock()
	entries := len(core.untrackedLines[repo].files)
	core.untrackedMu.Unlock()
	if entries != 0 {
		t.Fatalf("fresh file cached: %d entries, want 0 (mtime inside the racy window)", entries)
	}

	backdateRepoFile(t, repo, "f.txt")
	if ins, _ := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 2 {
		t.Fatalf("quiesced scan: got %d insertions, want 2", ins)
	}
	core.untrackedMu.Lock()
	entries = len(core.untrackedLines[repo].files)
	core.untrackedMu.Unlock()
	if entries != 1 {
		t.Fatalf("quiesced file not cached: %d entries, want 1", entries)
	}
}

// TestUntrackedStatsCacheTTLSweep proves a workspace that stops being
// scanned ages out of the cache instead of pinning its map for the process
// lifetime.
func TestUntrackedStatsCacheTTLSweep(t *testing.T) {
	repoA := testutil.InitGitRepo(t)
	repoB := testutil.InitGitRepo(t)
	core := NewCore()
	now := time.Now()
	core.nowFn = func() time.Time { return now }

	writeRepoFile(t, repoA, "a.txt", "1\n")
	writeRepoFile(t, repoB, "b.txt", "1\n")
	core.untrackedStats(repoA, maxUntrackedScanBytes)
	core.untrackedStats(repoB, maxUntrackedScanBytes)

	// Advance past the TTL and scan only repoB: the store's sweep must drop
	// repoA while keeping the just-refreshed repoB.
	now = now.Add(untrackedCacheTTL + time.Minute)
	core.untrackedStats(repoB, maxUntrackedScanBytes)

	core.untrackedMu.Lock()
	_, hasA := core.untrackedLines[repoA]
	_, hasB := core.untrackedLines[repoB]
	core.untrackedMu.Unlock()
	if hasA || !hasB {
		t.Fatalf("TTL sweep: hasA=%v hasB=%v, want stale A swept and fresh B kept", hasA, hasB)
	}
}

// TestCountAddedLinesMatchesPanelParse pins countAddedLines (the badge's
// untracked counter) to the diff panel's per-file count: the frontend's parse
// (countPatchAddsDels, mirroring patchFiles.ts) of git's own `--no-index --patch`
// output. numstat agreement is not enough - the panel counts the patch, not
// numstat - so this is the test that proves the badge matches what the user sees
// for each shape of untracked file. A content line starting with a single '+' is
// included because the patch prefixes it to "++...", and both sides must still
// count it (only the '+++' header is excluded).
func TestCountAddedLinesMatchesPanelParse(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	cases := []struct{ name, content string }{
		{"plain", "a\nb\nc\n"},
		{"no_trailing_newline", "x\ny"},
		{"binary", "\x00\x01\x02binary"},
		{"single_line", "only\n"},
		{"content_line_starting_with_plus", "+added\nplain\n"},
		{"empty", ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := fmt.Sprintf("f%d.txt", i)
			writeRepoFile(t, repo, rel, tc.content)
			panelIns, _ := countPatchAddsDels(gitStdout(t, repo, "diff", "--no-index",
				"--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv",
				"--", os.DevNull, rel))
			if got := countAddedLines([]byte(tc.content)); got != panelIns {
				t.Fatalf("%s: countAddedLines = %d, panel parse = %d - badge and panel diverge for %q",
					tc.name, got, panelIns, tc.content)
			}
		})
	}
}

// TestStatusUntrackedSymlinkNotFollowed guards the security/parity fix: an
// untracked symlink must be counted as git counts it - its 1-line target text -
// NOT by following the link and counting the target file's contents (which would
// read outside the workspace, could hang on a FIFO target, and diverge from the
// diff panel).
func TestStatusUntrackedSymlinkNotFollowed(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	// 5-line target; if the scan followed the link it would count these 5 for
	// the link too.
	writeRepoFile(t, repo, "target.txt", "1\n2\n3\n4\n5\n")
	if err := os.Symlink("target.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	// target.txt (5 lines) + link.txt (1 line of target-path text) = 6.
	// Following the link would instead give 5 + 5 = 10.
	if status.Insertions != 6 {
		t.Fatalf("Status.Insertions = %d, want 6 (target 5 + symlink-as-link-text 1); "+
			"a value of 10 means the symlink was followed", status.Insertions)
	}
	// Version-robust backstop: match git itself, whatever it reports per entry.
	if want := untrackedOracle(t, repo); status.Insertions != want {
		t.Fatalf("Status.Insertions = %d, want %d (git oracle)", status.Insertions, want)
	}
}

// TestStatusInsertionsNoHeadRepo covers the fresh-repo path: `git diff HEAD`
// has no HEAD to diff against (exits non-zero, empty stdout) so tracked churn is
// zero - like the panel - while untracked files are still counted, and Status
// must not error.
func TestStatusInsertionsNoHeadRepo(t *testing.T) {
	repo := t.TempDir()
	if err := testutil.RunGitAllowError(repo, "init", "-b", "main"); err != nil {
		testutil.RunGit(t, repo, "init")
	}
	testutil.RunGit(t, repo, "config", "user.email", "agent-overflow@example.com")
	testutil.RunGit(t, repo, "config", "user.name", "Agent Overflow")
	core := NewCore()

	writeRepoFile(t, repo, "a.txt", "one\ntwo\n") // 2 lines
	writeRepoFile(t, repo, "b.txt", "x\ny\nz\n")  // 3 lines

	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status on no-HEAD repo returned error: %v", err)
	}
	if !status.IsRepo {
		t.Fatal("expected IsRepo=true")
	}
	want := untrackedOracle(t, repo)
	if want == 0 {
		t.Fatal("fixture produced no untracked insertions")
	}
	if status.Insertions != want {
		t.Fatalf("Status.Insertions = %d, want %d (untracked-only, no tracked churn without HEAD)",
			status.Insertions, want)
	}
	if status.Deletions != 0 {
		t.Fatalf("Status.Deletions = %d, want 0", status.Deletions)
	}
	// No HEAD -> no tracked changes; the two untracked files are the whole count.
	if status.FileCount != 2 {
		t.Fatalf("Status.FileCount = %d, want 2 (a.txt + b.txt, untracked)", status.FileCount)
	}
}

// TestStatusInsertionsIncludeUntracked is the regression guard for the badge /
// panel divergence: Status.Insertions must equal the number the diff panel
// computes (tracked churn vs HEAD plus every untracked file counted as
// all-insertions), proven against git itself as an independent oracle on a
// fixture that exercises binary, no-trailing-newline, and nested-dir files.
func TestStatusInsertionsIncludeUntracked(t *testing.T) {
	repo := testutil.InitGitRepo(t) // README.txt = "hello\n", committed
	core := NewCore()

	// Tracked edit: rewrite the committed file so there is real churn (and a
	// deletion) to fold in alongside the untracked content.
	writeRepoFile(t, repo, "README.txt", "hello world\nsecond\nthird\n")

	// Untracked files of every shape the counter has to get right. sub/ holds
	// two files and is wholly untracked, so `git status` collapses it to one
	// porcelain entry - the dir-collapse case that the FileCount fix must not
	// undercount (ls-files enumerates both).
	writeRepoFile(t, repo, "new.txt", "a\nb\nc\n")                          // 3 lines
	writeRepoFile(t, repo, "tail.txt", "x\ny")                              // 2 lines, no trailing newline
	writeRepoFile(t, repo, "blob.bin", "\x00\x01\x02binary")                // binary -> 0 lines
	writeRepoFile(t, repo, filepath.Join("sub", "nested.txt"), "n1\nn2\n")  // 2 lines, nested dir
	writeRepoFile(t, repo, filepath.Join("sub", "nested2.txt"), "m1\nm2\n") // 2 lines, same wholly-untracked dir

	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	// Oracle: the same numbers the panel derives, straight from git.
	wantTrackedIns, wantTrackedDel := numstatTotals(gitStdout(t, repo,
		"diff", "--numstat", "--minimal", "--no-ext-diff", "--no-textconv", "HEAD", "--"))
	wantUntrackedIns := untrackedOracle(t, repo)

	if wantUntrackedIns == 0 {
		t.Fatal("fixture produced no untracked insertions; the new path is not being exercised")
	}
	wantIns := wantTrackedIns + wantUntrackedIns
	if status.Insertions != wantIns {
		t.Fatalf("Status.Insertions = %d, want %d (tracked %d + untracked %d)",
			status.Insertions, wantIns, wantTrackedIns, wantUntrackedIns)
	}
	if status.Deletions != wantTrackedDel {
		t.Fatalf("Status.Deletions = %d, want %d", status.Deletions, wantTrackedDel)
	}
	// The original bug was Status.Insertions == tracked-only; assert it now
	// genuinely exceeds that because untracked content is included.
	if status.Insertions <= wantTrackedIns {
		t.Fatalf("Status.Insertions = %d did not include untracked content (tracked = %d)",
			status.Insertions, wantTrackedIns)
	}

	// FileCount must match the panel too: tracked-changed files plus every
	// individual untracked file. The oracle counts untracked files via ls-files
	// (which enumerates sub/nested.txt and sub/nested2.txt separately) - the
	// previous porcelain-based count collapsed sub/ to one entry and undercounted.
	wantFiles := countLines(gitStdout(t, repo, "diff", "HEAD", "--name-only")) +
		len(splitNUL(gitStdout(t, repo, "ls-files", "--others", "--exclude-standard", "-z")))
	if wantFiles != 6 {
		t.Fatalf("fixture sanity: oracle wantFiles = %d, want 6 (1 tracked README + 5 untracked, sub/ counting as 2)", wantFiles)
	}
	if status.FileCount != wantFiles {
		t.Fatalf("Status.FileCount = %d, want %d (tracked-changed + individual untracked files)", status.FileCount, wantFiles)
	}

	// The definitive check for the original complaint (badge != panel): the badge
	// must equal what the panel actually shows. panelWorkspaceTotal parses the
	// same patch DiffWorkspaceVsHead produces using the frontend's algorithm - a
	// separate code path from the badge's numstat + countAddedLines - so this
	// closes the transitivity gap a numstat-only oracle would leave. The fixture
	// avoids content lines beginning with '+++'/'---' (the one case where the
	// frontend prefix parser undercounts and the badge is the more accurate).
	panelIns, panelDel := panelWorkspaceTotal(t, repo)
	if status.Insertions != panelIns || status.Deletions != panelDel {
		t.Fatalf("badge (+%d -%d) != panel (+%d -%d) - the badge must match the diff panel",
			status.Insertions, status.Deletions, panelIns, panelDel)
	}
}

// backdateRepoFile moves rel's mtime safely past untrackedRacyWindow so
// the memo's racy-mtime guard treats it as quiesced.
func backdateRepoFile(t *testing.T, repo, rel string) {
	t.Helper()
	old := time.Now().Add(-untrackedRacyWindow - 8*time.Second)
	if err := os.Chtimes(filepath.Join(repo, rel), old, old); err != nil {
		t.Fatalf("chtimes %s: %v", rel, err)
	}
}

func writeRepoFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
