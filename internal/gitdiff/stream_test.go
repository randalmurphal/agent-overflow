package gitdiff

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// readDiff reads a whole diff in chunks of chunkBytes, checking that every
// chunk continues the previous one.
func readDiff(t *testing.T, diff *Diff, chunkBytes int) ([]Chunk, error) {
	t.Helper()
	var chunks []Chunk
	var offset int64
	for {
		chunk, err := diff.Read(context.Background(), offset, chunkBytes)
		if err != nil {
			return chunks, err
		}
		if chunk.Offset != offset || chunk.NextOffset != offset+int64(len(chunk.Data)) {
			t.Fatalf("chunk [%d,%d) with %d bytes does not continue offset %d",
				chunk.Offset, chunk.NextOffset, len(chunk.Data), offset)
		}
		chunks = append(chunks, chunk)
		offset = chunk.NextOffset
		if chunk.EOF {
			return chunks, nil
		}
	}
}

func joinChunks(chunks []Chunk) string {
	var b strings.Builder
	for _, chunk := range chunks {
		b.WriteString(chunk.Data)
	}
	return b.String()
}

// openAndRead opens a diff, reads it whole and closes it.
func openAndRead(t *testing.T, diff *Diff, err error) (string, error) {
	t.Helper()
	if err != nil {
		return "", err
	}
	defer closeDiff(t, diff)
	chunks, err := readDiff(t, diff, MaxChunkBytes)
	if err != nil {
		return "", err
	}
	return joinChunks(chunks), nil
}

func closeDiff(t *testing.T, diff *Diff) {
	t.Helper()
	if err := diff.Close(); err != nil {
		t.Errorf("close diff: %v", err)
	}
}

func worktreePatch(t *testing.T, repo string, opts Options) (string, error) {
	t.Helper()
	diff, err := OpenWorktreeDiff(context.Background(), repo, t.TempDir(), opts)
	return openAndRead(t, diff, err)
}

func branchBasePatch(t *testing.T, repo, base string, opts Options) (string, error) {
	t.Helper()
	diff, err := OpenBranchBaseDiff(context.Background(), repo, t.TempDir(), base, opts)
	return openAndRead(t, diff, err)
}

func commitPatch(t *testing.T, repo, sha string, opts Options) (string, error) {
	t.Helper()
	diff, err := OpenCommitDiff(context.Background(), repo, sha, opts)
	return openAndRead(t, diff, err)
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	stdout, _, _, err := runGit(context.Background(), repo, nil, false, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return stdout
}

// numberedLines returns count lines of width-padded text.
func numberedLines(prefix string, count int) string {
	var b strings.Builder
	for i := range count {
		b.WriteString(prefix)
		b.WriteString(strings.Repeat("x", i%40))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestDiffChunksReassembleTheWholePatchAtAnyChunkSize(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "big.txt", numberedLines("old ", 40000), "add big")
	commitFile(t, repo, "big.txt", numberedLines("new ", 40000), "rewrite big")
	commit := headSHA(t, repo)
	want := gitOutput(t, repo, Options{}.gitArgs("diff", commit+"^", commit, "--")...)
	if len(want) < 4*MinChunkBytes {
		t.Fatalf("fixture patch is only %d bytes; it must span several chunks", len(want))
	}

	for _, chunkBytes := range []int{MinChunkBytes, 100_000, MaxChunkBytes} {
		diff, err := OpenCommitDiff(context.Background(), repo, commit, Options{})
		if err != nil {
			t.Fatalf("OpenCommitDiff: %v", err)
		}
		chunks, err := readDiff(t, diff, chunkBytes)
		closeDiff(t, diff)
		if err != nil {
			t.Fatalf("read at %d: %v", chunkBytes, err)
		}
		if got := joinChunks(chunks); got != want {
			t.Fatalf("chunk size %d reassembled %d bytes, want the %d-byte patch", chunkBytes, len(got), len(want))
		}
		for i, chunk := range chunks {
			if len(chunk.Data) > chunkBytes {
				t.Fatalf("chunk %d holds %d bytes, over the %d asked for", i, len(chunk.Data), chunkBytes)
			}
			if !strings.HasSuffix(chunk.Data, "\n") {
				t.Fatalf("chunk %d does not end at a line boundary: %q", i, chunk.Data[max(0, len(chunk.Data)-20):])
			}
		}
	}
}

func TestDiffSplitsALongLineBetweenCharacters(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	// One line several chunks long, of two- and three-byte characters, so
	// a byte-count cut lands inside a character unless the reader backs off.
	line := strings.Repeat("é€", 3*MinChunkBytes/5) + "\n"
	writeFile(t, repo, "wide.txt", line)
	diff, err := OpenWorktreeDiff(context.Background(), repo, t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("OpenWorktreeDiff: %v", err)
	}
	defer closeDiff(t, diff)
	chunks, err := readDiff(t, diff, MinChunkBytes)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks; the line must span several", len(chunks))
	}
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk.Data) {
			t.Fatalf("chunk %d splits a character", i)
		}
	}
	if !strings.Contains(joinChunks(chunks), "+"+line) {
		t.Fatal("reassembled patch lost the long line")
	}
}

func TestChunkCut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		window string
		want   int
	}{
		{"after the last newline", "ab\ncd\nef", 6},
		{"whole window ending in a newline", "ab\n", 3},
		{"no newline, complete characters", "abcé", 5},
		{"no newline, two-byte character cut", "abc\xc3", 3},
		{"no newline, three-byte character cut after one byte", "ab\xe2\x82", 2},
		{"no newline, invalid bytes are kept", "ab\x80\x80\x80\x80", 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChunkCut([]byte(tc.window)); got != tc.want {
				t.Fatalf("ChunkCut(%q) = %d, want %d", tc.window, got, tc.want)
			}
		})
	}
}

func TestDiffRereadsReturnTheSnapshotAfterTheWorktreeAndHeadMove(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "big.txt", numberedLines("old ", 20000), "add big")
	writeFile(t, repo, "big.txt", numberedLines("new ", 20000))
	snapshotRoot := t.TempDir()
	diff, err := OpenWorktreeDiff(context.Background(), repo, snapshotRoot, Options{})
	if err != nil {
		t.Fatalf("OpenWorktreeDiff: %v", err)
	}
	defer closeDiff(t, diff)
	first, err := readDiff(t, diff, MinChunkBytes)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first) < 3 {
		t.Fatalf("got %d chunks; the patch must span several", len(first))
	}

	// Everything the snapshot was taken from changes: the file, and HEAD.
	writeFile(t, repo, "big.txt", "rewritten\n")
	testutil.RunGit(t, repo, "add", "big.txt")
	testutil.RunGit(t, repo, "commit", "-q", "-m", "move HEAD")

	for _, i := range []int{len(first) - 1, 1, 0} {
		chunk, err := diff.Read(context.Background(), first[i].Offset, len(first[i].Data))
		if err != nil {
			t.Fatalf("re-read chunk %d: %v", i, err)
		}
		if chunk != first[i] {
			t.Fatalf("re-read chunk %d differs from the first read", i)
		}
	}
}

func TestDiffRereadRefusesBytesThatNoLongerMatch(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "big.txt", numberedLines("old ", 20000), "add big")
	// Every 20th line changes: many hunks, several chunks.
	lines := strings.SplitAfter(numberedLines("old ", 20000), "\n")
	for i := 0; i < len(lines); i += 20 {
		lines[i] = "changed\n"
	}
	writeFile(t, repo, "big.txt", strings.Join(lines, ""))
	diff, err := OpenWorktreeDiff(context.Background(), repo, t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("OpenWorktreeDiff: %v", err)
	}
	defer closeDiff(t, diff)
	first, err := readDiff(t, diff, MinChunkBytes)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first) < 3 {
		t.Fatalf("got %d chunks; the patch must span several", len(first))
	}
	// The endpoints are fixed, so only configuration can change the bytes:
	// more context lines reshape the one hunk.
	testutil.RunGit(t, repo, "config", "diff.context", "9")
	last := first[len(first)-1]
	if _, err := diff.Read(context.Background(), last.Offset, len(last.Data)); !errors.Is(err, ErrDiffChanged) {
		t.Fatalf("re-read after the patch changed: err = %v, want ErrDiffChanged", err)
	}
}

func TestDiffReadRefusesAnOffsetNoReadEndedAt(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	writeFile(t, repo, "README.txt", "hello\nedited\n")
	diff, err := OpenWorktreeDiff(context.Background(), repo, t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("OpenWorktreeDiff: %v", err)
	}
	defer closeDiff(t, diff)
	if _, err := diff.Read(context.Background(), 3, MinChunkBytes); err == nil {
		t.Fatal("a read at an offset no chunk ended at must fail")
	}
	chunk, err := diff.Read(context.Background(), 0, MinChunkBytes)
	if err != nil || !chunk.EOF {
		t.Fatalf("read: %+v, %v", chunk, err)
	}
	end, err := diff.Read(context.Background(), chunk.NextOffset, MinChunkBytes)
	if err != nil || !end.EOF || end.Data != "" {
		t.Fatalf("read at the end: %+v, %v; want an empty EOF chunk", end, err)
	}
}

func TestDiffIdleStreamEndsAndTheNextReadResumes(t *testing.T) {
	previous := streamIdleTimeout
	streamIdleTimeout = 20 * time.Millisecond
	t.Cleanup(func() { streamIdleTimeout = previous })

	repo := testutil.InitGitRepo(t)
	writeFile(t, repo, "big.txt", numberedLines("line ", 20000))
	diff, err := OpenWorktreeDiff(context.Background(), repo, t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("OpenWorktreeDiff: %v", err)
	}
	defer closeDiff(t, diff)
	first, err := diff.Read(context.Background(), 0, MinChunkBytes)
	if err != nil || first.EOF {
		t.Fatalf("first read: %+v, %v", first, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		diff.mu.Lock()
		live := diff.live
		diff.mu.Unlock()
		if live == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle git process was never stopped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rest, err := readDiffFrom(t, diff, first.NextOffset, MinChunkBytes)
	if err != nil {
		t.Fatalf("read after idle stop: %v", err)
	}
	want, err := worktreePatch(t, repo, Options{})
	if err != nil {
		t.Fatalf("worktreePatch: %v", err)
	}
	if first.Data+rest != want {
		t.Fatal("the read resumed after the idle stop does not continue the patch")
	}
}

func readDiffFrom(t *testing.T, diff *Diff, offset int64, chunkBytes int) (string, error) {
	t.Helper()
	var b strings.Builder
	for {
		chunk, err := diff.Read(context.Background(), offset, chunkBytes)
		if err != nil {
			return "", err
		}
		b.WriteString(chunk.Data)
		offset = chunk.NextOffset
		if chunk.EOF {
			return b.String(), nil
		}
	}
}

// fakeGit puts a `git` executable running script first on PATH.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake git is a shell script")
	}
	dir := t.TempDir()
	mockexec.WriteIn(t, dir, "git", "#!/bin/sh\n"+script+"\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDiffCloseInterruptsAReadWaitingOnGit(t *testing.T) {
	fakeGit(t, "exec sleep 30")
	diff := newDiff(t.TempDir(), nil, nil, []string{"diff"})
	done := make(chan error, 1)
	go func() {
		_, err := diff.Read(context.Background(), 0, MinChunkBytes)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if err := diff.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Close waited %v for git", elapsed)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrDiffClosed) {
			t.Fatalf("interrupted read: err = %v, want ErrDiffClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read never returned")
	}
}

func TestDiffReadStopsWhenItsCallerGoesAway(t *testing.T) {
	fakeGit(t, "exec sleep 30")
	diff := newDiff(t.TempDir(), nil, nil, []string{"diff"})
	defer closeDiff(t, diff)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := diff.Read(ctx, 0, MinChunkBytes); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read: err = %v, want the caller's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("read waited %v after its caller left", elapsed)
	}
}

func TestDiffReportsAGitFailureInsteadOfAShortPatch(t *testing.T) {
	fakeGit(t, "printf 'diff --git a/x b/x\\n'; echo 'fatal: bad object' >&2; exit 128")
	diff := newDiff(t.TempDir(), nil, nil, []string{"diff"})
	defer closeDiff(t, diff)
	_, err := diff.Read(context.Background(), 0, MinChunkBytes)
	if err == nil || !strings.Contains(err.Error(), "exit=128") || !strings.Contains(err.Error(), "bad object") {
		t.Fatalf("read: err = %v, want git's exit status and stderr", err)
	}
}

func TestWorktreeDiffCloseRemovesItsSnapshot(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	writeFile(t, repo, "new.txt", "brand new\n")
	root := t.TempDir()
	diff, err := OpenWorktreeDiff(context.Background(), repo, root, Options{})
	if err != nil {
		t.Fatalf("OpenWorktreeDiff: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("snapshot root holds %d entries while open, want 1", len(entries))
	}
	closeDiff(t, diff)
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("snapshot root holds %d entries after Close, want 0", len(entries))
	}
	if _, err := diff.Read(context.Background(), 0, MinChunkBytes); !errors.Is(err, ErrDiffClosed) {
		t.Fatalf("read after Close: err = %v, want ErrDiffClosed", err)
	}
}

// A patch far larger than anything stored for it: a small compressed blob
// that expands into tens of megabytes of added lines. Reading it holds no
// more than a few chunks in memory, and its snapshot on disk stays the
// size of the compressed object. It does not run in parallel because the
// heap bound reads process-wide MemStats.
func TestDiffStreamsACompressiblePatchWithoutHoldingIt(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	const lines = 3_000_000
	writeFile(t, repo, "zeros.txt", strings.Repeat("0000000000\n", lines))
	root := t.TempDir()
	diff, err := OpenWorktreeDiff(context.Background(), repo, root, Options{})
	if err != nil {
		t.Fatalf("OpenWorktreeDiff: %v", err)
	}
	defer closeDiff(t, diff)

	var snapshotBytes int64
	_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if info, err := entry.Info(); err == nil {
				snapshotBytes += info.Size()
			}
		}
		return nil
	})

	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	baseline := stats.HeapAlloc
	var peak uint64
	var total int64
	var offset int64
	// Small reads keep the reader's own garbage well under the patch size,
	// so a reader that holds the patch cannot hide inside the bound.
	const readBytes = 1 << 20
	for {
		chunk, err := diff.Read(context.Background(), offset, readBytes)
		if err != nil {
			t.Fatalf("read at %d: %v", offset, err)
		}
		total += int64(len(chunk.Data))
		offset = chunk.NextOffset
		runtime.ReadMemStats(&stats)
		peak = max(peak, stats.HeapAlloc)
		if chunk.EOF {
			break
		}
	}
	if total < lines*12 {
		t.Fatalf("read %d bytes, want the whole %d-line patch", total, lines)
	}
	if grew := peak - min(peak, baseline); grew > 16<<20 || grew > uint64(total)/2 {
		t.Fatalf("heap grew by %d bytes reading a %d-byte patch; want a few chunks at most", grew, total)
	}
	if snapshotBytes > total/100 {
		t.Fatalf("snapshot holds %d bytes for a %d-byte patch; want the compressed object only", snapshotBytes, total)
	}
}

func TestPatchShapeIgnoresDiffConfiguration(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	commitFile(t, repo, "docs/old-name.txt", numberedLines("stable ", 50), "add file")
	testutil.RunGit(t, repo, "mv", "docs/old-name.txt", "docs/new-name.txt")
	for key, value := range map[string]string{
		"diff.renames":        "false",
		"diff.noprefix":       "true",
		"diff.mnemonicPrefix": "true",
		"diff.relative":       "true",
	} {
		testutil.RunGit(t, repo, "config", key, value)
	}
	patch, err := worktreePatch(t, repo, Options{})
	if err != nil {
		t.Fatalf("worktreePatch: %v", err)
	}
	for _, want := range []string{
		"diff --git a/docs/old-name.txt b/docs/new-name.txt",
		"rename from docs/old-name.txt",
		"rename to docs/new-name.txt",
	} {
		if !strings.Contains(patch, want) {
			t.Fatalf("patch lacks %q:\n%s", want, patch)
		}
	}
}

func TestOpenMergeBaseDiffShowsOnlyTheHeadSideOfTheMergeBase(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "checkout", "-q", "-b", "feature")
	commitFile(t, repo, "feature.txt", "feature work\n", "feature")
	head := headSHA(t, repo)
	testutil.RunGit(t, repo, "checkout", "-q", "main")
	commitFile(t, repo, "main.txt", "moved on\n", "main moves")

	diff, err := OpenMergeBaseDiff(context.Background(), repo, "main", head, Options{})
	patch, err := openAndRead(t, diff, err)
	if err != nil {
		t.Fatalf("OpenMergeBaseDiff: %v", err)
	}
	if !strings.Contains(patch, "+feature work") || strings.Contains(patch, "moved on") {
		t.Fatalf("want only the feature side of the merge base:\n%s", patch)
	}
	if _, err := OpenMergeBaseDiff(context.Background(), repo, "main", "HEAD", Options{}); err == nil {
		t.Fatal("a head that is not an OID must be refused")
	}
	if _, err := OpenMergeBaseDiff(context.Background(), repo, "--output=x", head, Options{}); err == nil {
		t.Fatal("a flag-shaped base must be refused")
	}
}
