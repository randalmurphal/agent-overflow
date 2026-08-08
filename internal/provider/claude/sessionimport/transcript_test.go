package sessionimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// transcript_test.go — pass 1's own rules: what it skips, what it refuses,
// and that neither costs the rest of the file.

func TestScanSkipsAnOversizedLineAndKeepsReadingTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")

	// A tool result larger than the line cap. The old reader ran this
	// through bufio.Scanner, whose over-long token is TERMINAL: one runaway
	// record failed the whole session with a raw "token too long".
	huge := strings.Repeat("x", maxTranscriptLineBytes+1024)
	writeJSONL(t, path,
		userRow("u1", "", "start", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			toolUseBlock("toolu_1", "Bash", map[string]any{"command": "cat big"}),
		}, "2026-01-01T00:00:01.000Z"),
		toolResultRow("r1", "a1", "toolu_1", huge, "2026-01-01T00:00:02.000Z"),
		userRow("u2", "r1", "carry on", "2026-01-01T00:00:03.000Z"),
	)

	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession over an oversized record: %v", err)
	}
	defer loaded.Close()
	if !hasWarning(loaded.Warnings, WarnOversizedLine) {
		t.Errorf("warnings = %+v, want %s", loaded.Warnings, WarnOversizedLine)
	}
	// The oversized row is gone, so the row that named it as a parent roots
	// a branch of its own — a hole, not an abandoned file. What must be
	// true is that every row AFTER the oversized one was still read.
	seen := map[string]bool{}
	events := 0
	for i := range loaded.Branches {
		branch, err := loaded.ConvertBranch(i)
		if err != nil {
			t.Fatalf("ConvertBranch(%d): %v", i, err)
		}
		events += len(branch.Events)
		for _, row := range branch.Chain {
			seen[row.UUID] = true
		}
	}
	for _, uuid := range []string{"u1", "a1", "u2"} {
		if !seen[uuid] {
			t.Errorf("row %s was lost; an oversized record must cost only itself", uuid)
		}
	}
	if seen["r1"] {
		t.Error("the oversized record was admitted; it is past the line cap")
	}
	if events == 0 {
		t.Error("an oversized record must not cost the session its other rows")
	}
}

func TestLoadSessionRefusesAnAbsurdlyLargeTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionA+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Sparse: the refusal is on the stat, so nothing is ever read.
	if err := f.Truncate(maxTranscriptBytes + 1); err != nil {
		f.Close()
		t.Skipf("filesystem will not hold a %d-byte sparse file: %v", maxTranscriptBytes+1, err)
	}
	f.Close()

	loaded, err := LoadSession(path)
	if err == nil {
		loaded.Close()
		t.Fatal("LoadSession over a 1 GB file: want a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "skipped") || !strings.Contains(err.Error(), path) {
		t.Errorf("refusal = %q, want user-facing prose naming the file", err)
	}
}

func TestScanRecordsByteRangesPassTwoCanReadBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	writeJSONL(t, path,
		"}}{ not json",
		userRow("u1", "", "start", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("answer")}, "2026-01-01T00:00:01.000Z"),
	)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	defer loaded.Close()
	for _, row := range loaded.Branches[0].Chain {
		line := string(raw[row.Offset : row.Offset+int64(row.Length)])
		if !strings.Contains(line, `"uuid":"`+row.UUID+`"`) {
			t.Errorf("row %s points at %q, which is not its own line", row.UUID, line)
		}
	}
}
