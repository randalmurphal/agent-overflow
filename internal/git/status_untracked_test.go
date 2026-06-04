package git

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
// binary), the scan-budget cap, a file that vanished mid-scan, and the
// security-critical symlink case (counted as its link text, never followed).
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

	// Regular text file: every newline is a line; bytesRead is the full size.
	if ins, read := countUntrackedFileLines(write("text.txt", "a\nb\nc\n"), maxUntrackedScanBytes); ins != 3 || read != 6 {
		t.Fatalf("text file: got (%d, %d), want (3, 6)", ins, read)
	}
	// No trailing newline still counts the final partial line.
	if ins, read := countUntrackedFileLines(write("tail.txt", "x\ny"), maxUntrackedScanBytes); ins != 2 || read != 3 {
		t.Fatalf("no-trailing-newline: got (%d, %d), want (2, 3)", ins, read)
	}
	// Binary content (NUL in the probe window) -> 0 lines, but bytes are read.
	if ins, read := countUntrackedFileLines(write("blob.bin", "\x00\x01\x02"), maxUntrackedScanBytes); ins != 0 || read != 3 {
		t.Fatalf("binary file: got (%d, %d), want (0, 3)", ins, read)
	}
	// A file that vanished mid-scan (Lstat fails) degrades to (0, 0).
	if ins, read := countUntrackedFileLines(filepath.Join(dir, "gone.txt"), maxUntrackedScanBytes); ins != 0 || read != 0 {
		t.Fatalf("missing file: got (%d, %d), want (0, 0)", ins, read)
	}
	// Budget cap: only `budget` bytes are read, so trailing lines beyond it are
	// not tallied and bytesRead never exceeds the budget. "a\n" -> 1 line, 2 bytes.
	if ins, read := countUntrackedFileLines(write("capped.txt", "a\nb\nc\n"), 2); ins != 1 || read != 2 {
		t.Fatalf("budget cap: got (%d, %d), want (1, 2)", ins, read)
	}

	// A symlink is counted as its target *text* (one line), never followed -
	// the security/parity fix. "text.txt" is an 8-char link with no newline.
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink("text.txt", link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if ins, read := countUntrackedFileLines(link, maxUntrackedScanBytes); ins != 1 || read != 0 {
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
	// makes the result independent of ls-files iteration order.
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeRepoFile(t, repo, name, "1\n2\n3\n")
	}

	// Full budget: every file is read and tallied - 3 files x 3 lines.
	if ins, files := core.untrackedStats(repo, maxUntrackedScanBytes); ins != 9 || files != 3 {
		t.Fatalf("full budget: got (%d insertions, %d files), want (9, 3)", ins, files)
	}

	// Tiny budget: the first file's 1-byte read spends the whole budget, so files
	// 2 and 3 hit the budget<=0 short-circuit - counted but not tallied. The file
	// count must survive; only the line tally stops.
	if ins, files := core.untrackedStats(repo, 1); ins != 1 || files != 3 {
		t.Fatalf("budget exhausted: got (%d insertions, %d files), want (1, 3) - "+
			"file count must survive budget exhaustion", ins, files)
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
