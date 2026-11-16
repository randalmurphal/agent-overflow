package app

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/jstext"
	"agent-overflow/internal/jstext/jstexttest"
)

// wireRoundTrip marshals v as the transport does and decodes it into out,
// as a JSON client receives it.
func wireRoundTrip(t *testing.T, v, out any) {
	t.Helper()
	buf, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(buf, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

// checkKeyedReads reads an opened diff to its end several times over,
// each pass at its own random chunk sizes. Every chunk's key is the key
// of the patch text up to its end, and a re-read of each chunk answers
// the chunk and key it answered first.
func checkKeyedReads(t *testing.T, app *App, ctx context.Context, opened ReviewDiffOpened, rng *rand.Rand) {
	t.Helper()
	if opened.ID == "" || opened.Chunk.EOF {
		t.Fatal("the fixture must hold a handle past its first chunk")
	}
	if opened.Chunk.Key != jstext.EmptyKey.Extend(opened.Chunk.Data) {
		t.Fatalf("first chunk key %+v, want %+v", opened.Chunk.Key, jstext.EmptyKey.Extend(opened.Chunk.Data))
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
			chunk, err := app.ReadReviewDiff(ctx, opened.ID, offset, maxBytes)
			if err != nil {
				t.Fatalf("pass %d: ReadReviewDiff(%d): %v", pass, offset, err)
			}
			text += chunk.Data
			if want := jstext.EmptyKey.Extend(text); chunk.Key != want {
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
		if pass > 0 && text != patch {
			t.Fatalf("pass %d read a different patch", pass)
		}
		patch = text
		for _, s := range all {
			again, err := app.ReadReviewDiff(ctx, opened.ID, s.chunk.Offset, s.maxBytes)
			if err != nil || again != s.chunk {
				t.Fatalf("pass %d: re-read at %d: %+v, %v; want %+v", pass, s.chunk.Offset, again.Key, err, s.chunk.Key)
			}
		}
	}
	if midLine == 0 {
		t.Fatal("no chunk ended inside a line; the fixture does not exercise cuts between characters")
	}
}

// The git diff's keys are proven in gitdiff (TestDiffKeyIsTheTextsAtAnyChunking);
// an edits diff keys its chunks here.
func TestReviewDiffKeyIsTheWholeTextsAtAnyChunkingOfAnEditsDiff(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	rng := rand.New(rand.NewPCG(9, 10))
	threadID := seedEditPayloads(t, app, 1,
		jstexttest.ArbitraryText(rng, reviewDiffFirstChunkBytes, gitdiff.MinChunkBytes)+"\n\n",
		jstexttest.ArbitraryText(rng, 2*reviewDiffFirstChunkBytes, gitdiff.MinChunkBytes),
		"\n",
		jstexttest.ArbitraryText(rng, 1000, gitdiff.MinChunkBytes))
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
	threadID := seedEditPayloads(t, app, 1, jstexttest.ArbitraryText(rng, 3*reviewDiffFirstChunkBytes, gitdiff.MinChunkBytes))
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenTurnEditsDiff(ctx, threadID, 1)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenTurnEditsDiff: %+v, %v", opened.ID, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for worker := range 8 {
		maxBytes := gitdiff.MinChunkBytes * (1 + worker%3)
		wg.Go(func() {
			var text []byte
			for offset := int64(0); ; {
				chunk, err := app.ReadReviewDiff(ctx, opened.ID, offset, maxBytes)
				if err != nil {
					errs <- err
					return
				}
				text = append(text, chunk.Data...)
				if want := jstext.EmptyKey.Extend(string(text)); chunk.Key != want {
					errs <- errors.New("a concurrent read answered a wrong key")
					return
				}
				if chunk.EOF {
					return
				}
				offset = chunk.NextOffset
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
	if opened.Chunk.Key != jstext.EmptyKey.Extend(opened.Chunk.Data) {
		t.Fatalf("key %+v, want %+v", opened.Chunk.Key, jstext.EmptyKey.Extend(opened.Chunk.Data))
	}
	var wire struct {
		Chunk struct {
			Key map[string]int64 `json:"key"`
		} `json:"chunk"`
	}
	wireRoundTrip(t, opened, &wire)
	if want := jstext.EmptyKey.Extend(opened.Chunk.Data); wire.Chunk.Key["hash"] != int64(want.Hash) || wire.Chunk.Key["units"] != want.Units || len(wire.Chunk.Key) != 2 {
		t.Fatalf("wire key %v, want hash %d and units %d", wire.Chunk.Key, want.Hash, want.Units)
	}
}

// An open with no patch answers the key of no text.
func TestReviewDiffEmptyOpensAnswerTheEmptyKey(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	empty := jstext.Key{Hash: 2166136261, Units: 0}
	ctx, _ := reviewDiffConn(t)

	plain := testWorkspaceRef(t, app, t.TempDir())
	opened, err := app.OpenWorkspaceDiff(ctx, plain, false)
	if err != nil || opened.ID != "" || !opened.Chunk.EOF || opened.Chunk.Key != empty {
		t.Fatalf("a non-git workspace's diff: %+v, %v; want key %+v", opened, err, empty)
	}
	// The key of no text is not zero, so it is on the wire.
	var wire struct {
		Chunk struct {
			Key *jstext.Key `json:"key"`
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
