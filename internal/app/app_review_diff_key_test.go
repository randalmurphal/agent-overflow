package app

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/fnv1a"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/transport"
)

// textKeyOf is the key of text as one patch, folded independently of any
// chunking.
func textKeyOf(text string) TextKey {
	hash, units := fnv1a.FoldUTF16(fnv1a.OffsetBasis, text)
	return TextKey{Hash: hash, Units: int64(units)}
}

// arbitraryPatchText is about size bytes drawn from characters of every
// UTF-8 length and the invalid shapes a non-UTF-8 file carries, with lines
// from empty to longer than a chunk. It holds no NUL, so git diffs it as
// text.
func arbitraryPatchText(rng *rand.Rand, size int) string {
	inLine := []string{"a", "plain ascii ", "é", "—", "\U0001F600", "\xff", "\xc3", "\xe2\x82", "\xf0\x9f\x98", "\xed\xa0\x80", "\x80", "\r"}
	var b strings.Builder
	for b.Len() < size {
		switch rng.IntN(512) {
		case 0:
			// A line longer than a chunk, so reads cut it between
			// characters (gitdiff.ChunkCut).
			for end := b.Len() + gitdiff.MinChunkBytes + rng.IntN(gitdiff.MinChunkBytes); b.Len() < end; {
				b.WriteString(inLine[rng.IntN(len(inLine))])
			}
		case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16:
			b.WriteByte('\n')
		default:
			b.WriteString(inLine[rng.IntN(len(inLine))])
		}
	}
	return b.String()
}

// checkKeyedReads reads an opened diff to its end several times over,
// each pass at its own random chunk sizes, with the plain and the packed
// method. Every chunk's key is the key of the patch text up to its end,
// the two methods answer the same key, and a re-read of each chunk
// answers the key it answered first. It returns the whole patch.
func checkKeyedReads(t *testing.T, app *App, ctx context.Context, opened ReviewDiffOpened, rng *rand.Rand) string {
	t.Helper()
	if opened.ID == "" || opened.Chunk.EOF {
		t.Fatal("the fixture must hold a handle past its first chunk")
	}
	if opened.Chunk.Key != textKeyOf(opened.Chunk.Data) {
		t.Fatalf("first chunk key %+v, want %+v", opened.Chunk.Key, textKeyOf(opened.Chunk.Data))
	}
	type served struct {
		chunk    ReviewDiffChunk
		maxBytes int
	}
	var patch string
	midLine := 0
	for pass := range 3 {
		var all []served
		text := opened.Chunk.Data
		for offset := opened.Chunk.NextOffset; ; {
			maxBytes := gitdiff.MinChunkBytes + rng.IntN(4*gitdiff.MinChunkBytes)
			var chunk ReviewDiffChunk
			if rng.IntN(2) == 0 {
				var err error
				if chunk, err = app.ReadReviewDiff(ctx, opened.ID, offset, maxBytes); err != nil {
					t.Fatalf("pass %d: ReadReviewDiff(%d): %v", pass, offset, err)
				}
			} else {
				packed, err := app.ReadReviewDiffPacked(ctx, opened.ID, offset, maxBytes)
				if err != nil {
					t.Fatalf("pass %d: ReadReviewDiffPacked(%d): %v", pass, offset, err)
				}
				chunk = ReviewDiffChunk{Data: unpackedForTest(t, packed), Offset: packed.Offset, NextOffset: packed.NextOffset, EOF: packed.EOF, Key: packed.Key}
			}
			text += chunk.Data
			if want := textKeyOf(text); chunk.Key != want {
				t.Fatalf("pass %d: chunk [%d,%d) key %+v, want %+v", pass, chunk.Offset, chunk.NextOffset, chunk.Key, want)
			}
			all = append(all, served{chunk, maxBytes})
			if !chunk.EOF && !strings.HasSuffix(chunk.Data, "\n") {
				midLine++
			}
			if chunk.EOF {
				break
			}
			offset = chunk.NextOffset
		}
		// A packed chunk's text is its sanitized bytes, so passes compare
		// as a client would.
		if pass > 0 && textKeyOf(text) != textKeyOf(patch) {
			t.Fatalf("pass %d read a different patch", pass)
		}
		patch = text
		for _, s := range all {
			plain, err := app.ReadReviewDiff(ctx, opened.ID, s.chunk.Offset, s.maxBytes)
			if err != nil || plain.Key != s.chunk.Key || plain.NextOffset != s.chunk.NextOffset {
				t.Fatalf("pass %d: plain re-read at %d: %+v, %v; want key %+v", pass, s.chunk.Offset, plain.Key, err, s.chunk.Key)
			}
			packed, err := app.ReadReviewDiffPacked(ctx, opened.ID, s.chunk.Offset, s.maxBytes)
			if err != nil || packed.Key != s.chunk.Key {
				t.Fatalf("pass %d: packed re-read at %d: %+v, %v; want key %+v", pass, s.chunk.Offset, packed.Key, err, s.chunk.Key)
			}
		}
	}
	if midLine == 0 {
		t.Fatal("no chunk ended inside a line; the fixture does not exercise cuts between characters")
	}
	return patch
}

// unpackedForTest is a packed chunk's text: its sanitized UTF-8 when
// packed, which folds as its raw bytes do, and its data otherwise.
func unpackedForTest(t *testing.T, chunk ReviewDiffPackedChunk) string {
	t.Helper()
	if !chunk.Packed {
		return chunk.Data
	}
	return unpackChunkText(t, chunk.Data)
}

func TestReviewDiffKeyIsTheWholeTextsAtAnyChunkingOfAGitDiff(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	repo := initMainGitRepo(t)
	rng := rand.New(rand.NewPCG(7, 8))
	content := arbitraryPatchText(rng, 2*reviewDiffFirstChunkBytes)
	if err := os.WriteFile(filepath.Join(repo, "arbitrary.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, testWorkspaceRef(t, app, repo), false)
	if err != nil {
		t.Fatalf("OpenWorkspaceDiff: %v", err)
	}
	patch := checkKeyedReads(t, app, ctx, opened, rng)
	if !strings.Contains(patch, "arbitrary.txt") || len(patch) < len(content) {
		t.Fatal("the patch does not hold the file")
	}
	// The text a JSON client receives for the whole patch at once keys
	// the same.
	var delivered string
	wireRoundTrip(t, patch, &delivered)
	if textKeyOf(delivered) != textKeyOf(patch) {
		t.Fatal("the whole patch's delivered text keys differently")
	}
}

func TestReviewDiffKeyIsTheWholeTextsAtAnyChunkingOfAnEditsDiff(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	rng := rand.New(rand.NewPCG(9, 10))
	threadID := seedEditPayloads(t, app, 1,
		arbitraryPatchText(rng, reviewDiffFirstChunkBytes)+"\n\n",
		arbitraryPatchText(rng, 2*reviewDiffFirstChunkBytes),
		"\n",
		arbitraryPatchText(rng, 1000))
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenTurnEditsDiff(ctx, threadID, 1)
	if err != nil {
		t.Fatalf("OpenTurnEditsDiff: %v", err)
	}
	checkKeyedReads(t, app, ctx, opened, rng)
}

// Reads of one handle run concurrently, each walk from 0 recording keys
// the others look up.
func TestReviewDiffKeysServeConcurrentReads(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	rng := rand.New(rand.NewPCG(11, 12))
	threadID := seedEditPayloads(t, app, 1, arbitraryPatchText(rng, 3*reviewDiffFirstChunkBytes))
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenTurnEditsDiff(ctx, threadID, 1)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenTurnEditsDiff: %+v, %v", opened.ID, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for worker := range 8 {
		maxBytes := gitdiff.MinChunkBytes * (1 + worker%3)
		packed := worker%2 == 0
		wg.Go(func() {
			text := ""
			for offset := int64(0); ; {
				var data string
				var key TextKey
				var next int64
				var eof bool
				if packed {
					chunk, err := app.ReadReviewDiffPacked(ctx, opened.ID, offset, maxBytes)
					if err != nil {
						errs <- err
						return
					}
					if chunk.Packed {
						out := make([]byte, 0, len(chunk.Data))
						for _, r := range chunk.Data {
							out = append(out, byte(r))
						}
						chunk.Data = string(out)
					}
					data, key, next, eof = chunk.Data, chunk.Key, chunk.NextOffset, chunk.EOF
				} else {
					chunk, err := app.ReadReviewDiff(ctx, opened.ID, offset, maxBytes)
					if err != nil {
						errs <- err
						return
					}
					data, key, next, eof = chunk.Data, chunk.Key, chunk.NextOffset, chunk.EOF
				}
				text += data
				if want := textKeyOf(text); key != want {
					errs <- errors.New("a concurrent read answered a wrong key")
					return
				}
				if eof {
					return
				}
				offset = next
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestReviewDiffOpenThatFitsItsFirstChunkAnswersItsKey(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	repo := initMainGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\ncafé \U0001F600 \xff\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, testWorkspaceRef(t, app, repo), false)
	if err != nil {
		t.Fatalf("OpenWorkspaceDiff: %v", err)
	}
	if opened.ID != "" || !opened.Chunk.EOF || opened.Chunk.Data == "" {
		t.Fatalf("opened = %+v; want the whole patch and no handle", opened)
	}
	if opened.Chunk.Key != textKeyOf(opened.Chunk.Data) {
		t.Fatalf("key %+v, want %+v", opened.Chunk.Key, textKeyOf(opened.Chunk.Data))
	}
	var wire struct {
		Chunk struct {
			Key map[string]int64 `json:"key"`
		} `json:"chunk"`
	}
	wireRoundTrip(t, opened, &wire)
	if want := textKeyOf(opened.Chunk.Data); wire.Chunk.Key["hash"] != int64(want.Hash) || wire.Chunk.Key["units"] != want.Units || len(wire.Chunk.Key) != 2 {
		t.Fatalf("wire key %v, want hash %d and units %d", wire.Chunk.Key, want.Hash, want.Units)
	}
}

// An open with no patch answers the key of no text.
func TestReviewDiffEmptyOpensAnswerTheEmptyKey(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	empty := TextKey{Hash: 2166136261, Units: 0}
	ctx, _ := reviewDiffConn(t)

	plain := testWorkspaceRef(t, app, t.TempDir())
	opened, err := app.OpenWorkspaceDiff(ctx, plain, false)
	if err != nil || opened.ID != "" || !opened.Chunk.EOF || opened.Chunk.Key != empty {
		t.Fatalf("a non-git workspace's diff: %+v, %v; want key %+v", opened, err, empty)
	}
	// The key of no text is not zero, so it is on the wire.
	var wire struct {
		Chunk struct {
			Key *TextKey `json:"key"`
		} `json:"chunk"`
	}
	wireRoundTrip(t, opened, &wire)
	if wire.Chunk.Key == nil || *wire.Chunk.Key != empty {
		t.Fatalf("wire key %+v, want %+v", wire.Chunk.Key, empty)
	}
	opened, err = app.OpenBranchBaseDiff(ctx, plain, "main", false)
	if err != nil || opened.ID != "" || !opened.Chunk.EOF || opened.Chunk.Key != empty {
		t.Fatalf("a non-git workspace's branch diff: %+v, %v; want key %+v", opened, err, empty)
	}

	repo := initMainGitRepo(t)
	opened, err = app.OpenWorkspaceDiff(ctx, testWorkspaceRef(t, app, repo), false)
	if err != nil || opened.Chunk.Data != "" || !opened.Chunk.EOF || opened.Chunk.Key != empty {
		t.Fatalf("a clean workspace's diff: %+v, %v; want key %+v", opened, err, empty)
	}
}

// fakeReviewDiff answers whatever read returns, accepting any offset.
type fakeReviewDiff struct {
	read func(offset int64) gitdiff.Chunk
}

func (d *fakeReviewDiff) Read(_ context.Context, offset int64, _ int) (gitdiff.Chunk, error) {
	return d.read(offset), nil
}

func (d *fakeReviewDiff) Close() error { return nil }

// A chunk whose start has no recorded key, or whose text contradicts what
// was served, fails the read instead of answering a wrong key.
func TestReviewDiffReadFailsWithoutTheKeyItsStartNeeds(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	ctx, _ := reviewDiffConn(t)
	patch := strings.Repeat("+line\n", 100)
	changed := false
	diff := &fakeReviewDiff{read: func(offset int64) gitdiff.Chunk {
		end := min(offset+60, int64(len(patch)))
		data := patch[offset:end]
		if changed {
			data = strings.ToUpper(data)
		}
		return gitdiff.Chunk{Data: data, Offset: offset, NextOffset: end, EOF: end == int64(len(patch))}
	}}
	id, err := app.holdReviewDiff(newOpenReviewDiff(ctx, diff, transport.ScopeFilesRead))
	if err != nil {
		t.Fatalf("holdReviewDiff: %v", err)
	}
	for _, offset := range []int64{0, 60} {
		if _, err := app.ReadReviewDiff(ctx, id, offset, 0); err != nil {
			t.Fatalf("read at %d: %v", offset, err)
		}
	}
	if _, err := app.ReadReviewDiff(ctx, id, 30, 0); !errors.Is(err, errReviewDiffKey) {
		t.Fatalf("read at an offset nothing ended at: err = %v, want errReviewDiffKey", err)
	}
	if _, err := app.ReadReviewDiffPacked(ctx, id, 30, 0); !errors.Is(err, errReviewDiffKey) {
		t.Fatalf("packed read at an offset nothing ended at: err = %v, want errReviewDiffKey", err)
	}
	// The same span read again with other text: the start key at 60 is
	// what was served, the end key contradicts the one recorded at 120.
	changed = true
	if _, err := app.ReadReviewDiff(ctx, id, 60, 0); !errors.Is(err, errReviewDiffKey) {
		t.Fatalf("a re-read whose text differs: err = %v, want errReviewDiffKey", err)
	}

	misplaced := &fakeReviewDiff{read: func(offset int64) gitdiff.Chunk {
		return gitdiff.Chunk{Data: "+line\n", Offset: offset + 6, NextOffset: offset + 12}
	}}
	if _, err := app.holdOpenedReviewDiff(ctx, "open", transport.ScopeFilesRead, misplaced); !errors.Is(err, errReviewDiffKey) {
		t.Fatalf("a chunk that starts elsewhere than read: err = %v, want errReviewDiffKey", err)
	}
	short := &fakeReviewDiff{read: func(offset int64) gitdiff.Chunk {
		return gitdiff.Chunk{Data: "+line\n", Offset: offset, NextOffset: offset + 7}
	}}
	if _, err := app.holdOpenedReviewDiff(ctx, "open", transport.ScopeFilesRead, short); !errors.Is(err, errReviewDiffKey) {
		t.Fatalf("a chunk whose span is not its bytes: err = %v, want errReviewDiffKey", err)
	}
}
