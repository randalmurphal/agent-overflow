package gitdiff

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/jstext"
	"agent-overflow/internal/jstext/jstexttest"
	"agent-overflow/internal/testutil"
)

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
		{"no newline, four-byte character cut after three bytes", "ab\xf0\x9f\x98", 2},
		{"no newline, a complete four-byte character", "ab\xf0\x9f\x98\x80", 6},
		{"no newline, an invalid lead byte is kept", "ab\xff", 3},
		{"no newline, an unfinished character after an invalid byte", "ab\xff\xe2", 3},
		{"a newline before an unfinished character", "\n\xe2\x82", 1},
		// Windows of 1 to 4 bytes: one that is a single unfinished
		// character is cut whole, so every cut makes progress.
		{"one byte", "a", 1},
		{"one unfinished byte", "\xc3", 1},
		{"one continuation byte", "\x80", 1},
		{"two bytes, one unfinished character", "\xe2\x82", 2},
		{"two bytes, an unfinished character after one", "a\xc3", 1},
		{"three bytes, one unfinished character", "\xf0\x9f\x98", 3},
		{"three bytes, an unfinished character after one", "a\xe2\x82", 1},
		{"three continuation bytes", "\x80\x80\x80", 3},
		{"four bytes, one character", "\xf0\x9f\x98\x80", 4},
		{"four bytes, an unfinished character after one", "a\xf0\x9f\x98", 1},
		{"four bytes, two characters", "éé", 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChunkCut([]byte(tc.window)); got != tc.want {
				t.Fatalf("ChunkCut(%q) = %d, want %d", tc.window, got, tc.want)
			}
		})
	}
}

// The frontend's fake backend (frontend/src/test/helpers/streamedPatch.ts)
// cuts its reads by the same vectors.
const chunkCutVectorsPath = "../../frontend/src/test/fixtures/chunkCutVectors.json"

// Where a read of Window bytes from Offset ends: the rest when it fits,
// else ChunkCut of the window, as diffStream.next cuts it.
func TestChunkCutMatchesTheSharedVectors(t *testing.T) {
	t.Parallel()
	buf, err := os.ReadFile(chunkCutVectorsPath)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vectors []struct {
		Name   string `json:"name"`
		RawHex string `json:"rawHex"`
		Offset int    `json:"offset"`
		Window int    `json:"window"`
		Cut    int    `json:"cut"`
	}
	if err := json.Unmarshal(buf, &vectors); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range vectors {
		raw, err := hex.DecodeString(v.RawHex)
		if err != nil {
			t.Fatalf("%s: rawHex: %v", v.Name, err)
		}
		cut := len(raw)
		if len(raw)-v.Offset > v.Window {
			cut = v.Offset + ChunkCut(raw[v.Offset:v.Offset+v.Window])
		}
		if cut != v.Cut {
			t.Errorf("%s: a read of %d bytes from %d of %q ends at %d, the vector says %d", v.Name, v.Window, v.Offset, raw, cut, v.Cut)
		}
	}
}

// A cut never falls inside a valid UTF-8 sequence, so the text a client
// receives for consecutive chunks, and its key, is the whole patch's.
func TestChunkCutKeepsTheTextOfTheWholePatch(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(5, 6))
	alphabet := []string{"a", "\n", "é", "€", "\U0001F600", "\xff", "\xc3", "\xe2\x82", "\xf0\x9f\x98", "\xed\xa0\x80", "\x80"}
	for round := range 20000 {
		var b strings.Builder
		for n := 2 + rng.IntN(16); n > 0; n-- {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		patch := b.String()
		if len(patch) <= utf8.UTFMax+1 {
			continue
		}
		end := utf8.UTFMax + 1 + rng.IntN(len(patch)-utf8.UTFMax)
		cut := ChunkCut([]byte(patch[:end]))
		if jstext.EmptyKey.Extend(patch[:cut]).Extend(patch[cut:]) != jstext.EmptyKey.Extend(patch) {
			t.Fatalf("round %d: %q cut at %d of a %d-byte window changes the text", round, patch, cut, end)
		}
	}
}

// Every chunk's key is the key of the patch text up to its end, read at
// any chunk sizes, and a re-read of a chunk answers the key it answered
// first. A long line is cut inside a character's window, so the chunks
// cover cuts that back off.
func TestDiffKeyIsTheTextsAtAnyChunking(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	rng := rand.New(rand.NewPCG(7, 8))
	writeFile(t, repo, "arbitrary.txt", jstexttest.ArbitraryText(rng, 12*MinChunkBytes, MinChunkBytes))
	diff, err := diffOf(Options{})(WorktreeSides(context.Background(), repo, t.TempDir()))
	if err != nil {
		t.Fatalf("WorktreeSides: %v", err)
	}
	defer closeDiff(t, diff)
	type served struct {
		chunk    Chunk
		maxBytes int
	}
	var whole jstext.Key
	backedOff := 0
	for pass := range 3 {
		var all []served
		key := jstext.EmptyKey
		for offset := int64(0); ; {
			maxBytes := MinChunkBytes + rng.IntN(2*MinChunkBytes)
			chunk, err := diff.Read(context.Background(), offset, maxBytes)
			if err != nil {
				t.Fatalf("pass %d: read at %d: %v", pass, offset, err)
			}
			if key = key.Extend(chunk.Data); chunk.Key != key {
				t.Fatalf("pass %d: chunk [%d,%d) key %+v, want %+v", pass, chunk.Offset, chunk.NextOffset, chunk.Key, key)
			}
			if !chunk.EOF && !strings.HasSuffix(chunk.Data, "\n") && len(chunk.Data) < maxBytes {
				backedOff++
			}
			all = append(all, served{chunk, maxBytes})
			if chunk.EOF {
				break
			}
			offset = chunk.NextOffset
		}
		if pass > 0 && key != whole {
			t.Fatalf("pass %d keys the patch %+v, the first %+v", pass, key, whole)
		}
		whole = key
		for _, s := range all {
			again, err := diff.Read(context.Background(), s.chunk.Offset, s.maxBytes)
			if err != nil || again.Key != s.chunk.Key || again.NextOffset != s.chunk.NextOffset {
				t.Fatalf("pass %d: re-read at %d: %+v, %v; want key %+v", pass, s.chunk.Offset, again.Key, err, s.chunk.Key)
			}
		}
	}
	patch, err := worktreePatch(t, repo, Options{})
	if err != nil {
		t.Fatalf("worktreePatch: %v", err)
	}
	if want := jstext.EmptyKey.Extend(patch); whole != want {
		t.Fatalf("the last chunk's key %+v, the whole patch's %+v", whole, want)
	}
	if backedOff == 0 {
		t.Fatal("no chunk was cut back inside a line; the fixture does not exercise cuts between characters")
	}
}
