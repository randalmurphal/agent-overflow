package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/transport"
)

// packedTestLine holds two-, three- and four-byte characters and the
// invalid shapes a patch of a non-UTF-8 file carries.
const packedTestLine = "+café — \U0001F600 lone \xe9 cut \xe2\x82 ff \xff surrogate \xed\xa0\x80 end\n"

// sparsePackedTestText is about size bytes of ASCII lines with a
// packedTestLine after every 40: sparse enough that gitdiff.PackText packs
// every chunk of it.
func sparsePackedTestText(size int) string {
	block := strings.Repeat("+a line of plain code, forty bytes long.\n", 40) + packedTestLine
	return strings.Repeat(block, size/len(block)+1)
}

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

// unpackChunkText decodes received packed text: each character is one
// byte of UTF-8.
func unpackChunkText(t *testing.T, packed string) string {
	t.Helper()
	out := make([]byte, 0, len(packed))
	for _, r := range packed {
		if r > 0xFF {
			t.Fatalf("packed text holds %U", r)
		}
		out = append(out, byte(r))
	}
	if !utf8.Valid(out) {
		t.Fatal("packed text unpacks to invalid UTF-8")
	}
	return string(out)
}

// comparePackedReads reads an open diff to its end with ReadReviewDiff and
// ReadReviewDiffPacked from each offset, and checks that the two answer
// the same chunk as a client receives them, each in the form
// gitdiff.PackText gives its bytes. wantPacked says whether the full-size
// chunks are packed (the last can be a tail of any shape). It returns the
// packed method's chunks.
func comparePackedReads(t *testing.T, app *App, ctx context.Context, opened ReviewDiffOpened, maxBytes int, wantPacked bool) []ReviewDiffPackedChunk {
	t.Helper()
	var packedChunks []ReviewDiffPackedChunk
	for offset := opened.Chunk.NextOffset; ; {
		plain, err := app.ReadReviewDiff(ctx, opened.ID, offset, maxBytes)
		if err != nil {
			t.Fatalf("ReadReviewDiff(%d): %v", offset, err)
		}
		packed, err := app.ReadReviewDiffPacked(ctx, opened.ID, offset, maxBytes)
		if err != nil {
			t.Fatalf("ReadReviewDiffPacked(%d): %v", offset, err)
		}
		if packed.Offset != plain.Offset || packed.NextOffset != plain.NextOffset || packed.EOF != plain.EOF {
			t.Fatalf("packed chunk at %d spans [%d,%d) eof=%v, plain [%d,%d) eof=%v", offset,
				packed.Offset, packed.NextOffset, packed.EOF, plain.Offset, plain.NextOffset, plain.EOF)
		}
		var gotPlain ReviewDiffChunk
		var gotPacked ReviewDiffPackedChunk
		wireRoundTrip(t, plain, &gotPlain)
		wireRoundTrip(t, packed, &gotPacked)
		// The key is of the text, whichever form carries it.
		if gotPacked.Key != gotPlain.Key || gotPlain.Key != plain.Key || plain.Key.Units == 0 {
			t.Fatalf("chunk at %d: packed key %+v, plain key %+v, sent %+v", offset, gotPacked.Key, gotPlain.Key, plain.Key)
		}
		if gotPacked.Wide == nil {
			t.Fatalf("chunk at %d: wide arrived null", offset)
		}
		data, wide, isPacked := gitdiff.PackText(plain.Data)
		if packed.Packed != isPacked || packed.Data != data || !reflect.DeepEqual(packed.Wide, wide) {
			t.Fatalf("chunk at %d: not gitdiff.PackText of the plain chunk's bytes", offset)
		}
		if !packed.EOF && gotPacked.Packed != wantPacked {
			t.Fatalf("chunk at %d: packed = %v, want %v", offset, gotPacked.Packed, wantPacked)
		}
		if gotPacked.Packed {
			if len(gotPacked.Wide) == 0 || unpackChunkText(t, gotPacked.Data) != gotPlain.Data {
				t.Fatalf("chunk at %d: packed text with %d marks does not unpack to the plain chunk's text", offset, len(gotPacked.Wide))
			}
		} else if len(gotPacked.Wide) != 0 || gotPacked.Data != gotPlain.Data {
			t.Fatalf("chunk at %d: an unpacked chunk with %d marks differs from the plain chunk", offset, len(gotPacked.Wide))
		}
		packedChunks = append(packedChunks, packed)
		if packed.EOF {
			break
		}
		offset = packed.NextOffset
	}
	if len(packedChunks) < 2 {
		t.Fatalf("read %d chunks; want several", len(packedChunks))
	}
	return packedChunks
}

func TestReadReviewDiffPackedServesTheSameChunksAsReadReviewDiff(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	repo := initMainGitRepo(t)
	content := sparsePackedTestText(3 * reviewDiffFirstChunkBytes)
	if err := os.WriteFile(filepath.Join(repo, "mixed.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenWorkspaceDiff: %+v, %v", opened.ID, err)
	}

	otherCtx, _ := reviewDiffConn(t)
	if _, err := app.ReadReviewDiffPacked(otherCtx, opened.ID, opened.Chunk.NextOffset, 1<<20); !errors.Is(err, ErrReviewDiffNotFound) {
		t.Fatalf("read on another connection: err = %v, want ErrReviewDiffNotFound", err)
	}

	chunks := comparePackedReads(t, app, ctx, opened, 256<<10, true)
	again, err := app.ReadReviewDiffPacked(ctx, opened.ID, chunks[1].Offset, 256<<10)
	if err != nil || !reflect.DeepEqual(again, chunks[1]) {
		t.Fatalf("re-read at %d: %v; want the chunk served first", chunks[1].Offset, err)
	}

	if err := app.ReleaseReviewDiff(ctx, opened.ID); err != nil {
		t.Fatalf("ReleaseReviewDiff: %v", err)
	}
	if _, err := app.ReadReviewDiffPacked(ctx, opened.ID, 0, 1<<20); !errors.Is(err, ErrReviewDiffNotFound) {
		t.Fatalf("read after release: err = %v, want ErrReviewDiffNotFound", err)
	}
}

// Text dense in characters past ASCII is served as ReadReviewDiff serves
// it: a client would decode every line of it.
func TestReadReviewDiffPackedLeavesDenseTextUnpacked(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	repo := initMainGitRepo(t)
	content := strings.Repeat(packedTestLine, 3*reviewDiffFirstChunkBytes/len(packedTestLine))
	if err := os.WriteFile(filepath.Join(repo, "dense.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenWorkspaceDiff: %+v, %v", opened.ID, err)
	}
	comparePackedReads(t, app, ctx, opened, 256<<10, false)
}

func TestReadReviewDiffPackedReadsAnEditsDiff(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	var b strings.Builder
	b.WriteString("diff --git a/m.txt b/m.txt\nnew file mode 100644\n--- /dev/null\n+++ b/m.txt\n")
	text := sparsePackedTestText(3 * reviewDiffFirstChunkBytes)
	fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", strings.Count(text, "\n"))
	b.WriteString(text)
	threadID := seedEditPayloads(t, app, 2, b.String())
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenTurnEditsDiff(ctx, threadID, 2)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenTurnEditsDiff: %+v, %v", opened.ID, err)
	}
	chunks := comparePackedReads(t, app, ctx, opened, 256<<10, true)
	again, err := app.ReadReviewDiffPacked(ctx, opened.ID, chunks[1].Offset, 256<<10)
	if err != nil || !reflect.DeepEqual(again, chunks[1]) {
		t.Fatalf("re-read at %d: %v; want the chunk served first", chunks[1].Offset, err)
	}
	if err := app.ReleaseReviewDiff(ctx, opened.ID); err != nil {
		t.Fatalf("ReleaseReviewDiff: %v", err)
	}
	if _, err := app.ReadReviewDiffPacked(ctx, opened.ID, opened.Chunk.NextOffset, 1<<20); !errors.Is(err, ErrReviewDiffNotFound) {
		t.Fatalf("read after release: err = %v, want ErrReviewDiffNotFound", err)
	}
}

// The packed read keeps ReadReviewDiff's scope, closed-diff and shutdown
// rules.
func TestReadReviewDiffPackedKeepsTheReadRules(t *testing.T) {
	t.Parallel()
	app := identityApp(t)
	threadID := seedEditPayloads(t, app, 1, generatedPatch("big.go", 60000))
	session := pairSessionWithScopes(t, app, "thumb-threads", []identity.Scope{identity.ScopeThreadsRead})
	ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{SessionID: session.ID})
	t.Cleanup(state.RunCleanups)
	opened, err := app.OpenTurnEditsDiff(ctx, threadID, 1)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenTurnEditsDiff() = %+v, %v", opened.ID, err)
	}
	if _, err := app.ReadReviewDiffPacked(ctx, opened.ID, opened.Chunk.NextOffset, 1<<20); err != nil {
		t.Fatalf("a session holding threads:read reading an edits diff: %v", err)
	}

	diff, err := newEditsDiff(ctx, app.store, threadID, opened.PayloadIDs)
	if err != nil {
		t.Fatalf("newEditsDiff() error = %v", err)
	}
	id, err := app.holdReviewDiff(newOpenReviewDiff(ctx, diff, transport.ScopeFilesRead))
	if err != nil {
		t.Fatalf("holdReviewDiff() error = %v", err)
	}
	_, err = app.ReadReviewDiffPacked(ctx, id, 0, 1<<20)
	wantScopeRefusal(t, err, transport.ScopeFilesRead)

	app.shuttingDown.Store(true)
	if _, err := app.ReadReviewDiffPacked(ctx, opened.ID, opened.Chunk.NextOffset, 1<<20); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("read during shutdown: err = %v, want ErrShuttingDown", err)
	}
}

func TestReadReviewDiffPackedOfAClosedDiffIsNotOpen(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	ref, _ := largeWorkspace(t, app)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenWorkspaceDiff: %+v, %v", opened.ID, err)
	}
	open, err := app.ownedReviewDiff(ctx, opened.ID)
	if err != nil {
		t.Fatalf("ownedReviewDiff: %v", err)
	}
	if err := open.diff.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := app.ReadReviewDiffPacked(ctx, opened.ID, opened.Chunk.NextOffset, 1<<20); !errors.Is(err, ErrReviewDiffNotFound) {
		t.Fatalf("read of a closed diff: err = %v, want ErrReviewDiffNotFound", err)
	}
}

// The worst case for a packed chunk's frame: every byte a six-byte escape
// (control characters, and an invalid byte's three packed characters), and
// as many wide lines, so wide marks, as PackText's rule packs.
func TestReviewDiffPackedChunkAtTheCeilingFitsTheReadLimit(t *testing.T) {
	t.Parallel()
	unit := "\xff\n" + strings.Repeat("\x01", 43) + "\n"
	raw := strings.Repeat(unit, gitdiff.MaxChunkBytes/len(unit))
	data, wide, packed := gitdiff.PackText(raw)
	if !packed || len(wide) != gitdiff.MaxChunkBytes/len(unit) {
		t.Fatalf("packed = %v with %d marks; want the densest text PackText packs", packed, len(wide))
	}
	if _, _, denser := gitdiff.PackText(strings.Repeat("\xff\n"+strings.Repeat("\x01", 42)+"\n", 1000)); denser {
		t.Fatal("text with more wide lines was packed: the case above is not the worst")
	}
	buf, err := json.Marshal(ReviewDiffPackedChunk{Data: data, Packed: true, Wide: wide, Offset: 1 << 40, NextOffset: 1<<40 + gitdiff.MaxChunkBytes})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Room for the reply envelope around the result.
	if len(buf)+4096 > transport.DefaultReadLimit {
		t.Fatalf("a %d-byte chunk packs to a %d-byte result, past the %d-byte read limit", len(raw), len(buf), transport.DefaultReadLimit)
	}
}
