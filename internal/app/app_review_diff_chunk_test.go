package app

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/transport"
)

// The worst case for a chunk's frame: every byte a six-byte escape.
func TestReviewDiffChunkAtTheCeilingFitsTheReadLimit(t *testing.T) {
	t.Parallel()
	raw := strings.Repeat("\x01", gitdiff.MaxChunkBytes)
	buf, err := json.Marshal(ReviewDiffChunk{Data: raw, Offset: 1 << 40, NextOffset: 1<<40 + gitdiff.MaxChunkBytes})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(buf) < 6*gitdiff.MaxChunkBytes {
		t.Fatalf("a %d-byte chunk is a %d-byte result: not the worst case", len(raw), len(buf))
	}
	// Room for the reply envelope around the result.
	if len(buf)+4096 > transport.DefaultReadLimit {
		t.Fatalf("a %d-byte chunk is a %d-byte result, past the %d-byte read limit", len(raw), len(buf), transport.DefaultReadLimit)
	}
}
