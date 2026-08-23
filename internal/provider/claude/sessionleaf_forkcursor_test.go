package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func forkCursorFromTranscript(t *testing.T, transcript, pin string) ForkResumeCursor {
	t.Helper()
	tracker, branch, err := scanSessionTrackerAndBranch(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionTrackerAndBranch: %v", err)
	}
	return forkResumeCursorFromScan(tracker.stateForColdResume(), branch, pin)
}

// A pin that survives the resume filters is used verbatim — even when
// the source streamed on and deeper surviving rows follow it.
func TestForkResumeCursorPinSurvivesVerbatim(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"start"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"reply 1"}]}}`,
		// The source kept streaming after the fork click.
		`{"type":"user","uuid":"u2","parentUuid":"a1","message":{"role":"user","content":"later prompt"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u2","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"later reply"}]}}`,
	}, "\n")

	got := forkCursorFromTranscript(t, transcript, "a1")
	if !got.PinOnDisk || got.Cursor != "a1" {
		t.Fatalf("cursor = %+v, want {Cursor:a1 PinOnDisk:true}", got)
	}
}

// A pin the CLI's filters would drop (dangling client tool_use — the
// row a mid-turn capture tends to land on) is repaired BACKWARD to the
// deepest surviving row at or before the pin's file position. Deeper
// survivors that follow the pin in the file belong to the source's
// future and must never be chosen.
func TestForkResumeCursorDanglingToolUsePinRepairsBackwardOnly(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"start"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"reply 1"}]}}`,
		// The pin: a tool_use whose result never landed.
		`{"type":"assistant","uuid":"a2","parentUuid":"a1","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{}}]}}`,
		// The source streamed on PAST the pin without resolving it (an
		// interrupt row, then a fresh reply) — both survive the filters
		// but sit after the pin in file order.
		`{"type":"user","uuid":"u2","parentUuid":"a2","message":{"role":"user","content":"[Request interrupted by user]"}}`,
		`{"type":"assistant","uuid":"a3","parentUuid":"u2","message":{"id":"m3","role":"assistant","content":[{"type":"text","text":"post-interrupt reply"}]}}`,
	}, "\n")

	got := forkCursorFromTranscript(t, transcript, "a2")
	if !got.PinOnDisk {
		t.Fatalf("PinOnDisk = false for a pin physically in the file: %+v", got)
	}
	if got.Cursor != "a1" {
		t.Fatalf("cursor = %q, want a1 (deepest survivor at or before the pin; a3/u2 are after it)", got.Cursor)
	}
}

// A pin absent from the file is the stdout-to-disk append gap:
// PinOnDisk=false so the caller can wait it out, with Cursor holding
// the deepest survivor of the file as it stands (the exhaustion
// fallback — always EARLIER than the pin).
func TestForkResumeCursorMissingPinReportsAppendGap(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"start"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"reply 1"}]}}`,
	}, "\n")

	got := forkCursorFromTranscript(t, transcript, "a2-not-yet-flushed")
	if got.PinOnDisk {
		t.Fatalf("PinOnDisk = true for a uuid the file does not hold: %+v", got)
	}
	if got.Cursor != "a1" {
		t.Fatalf("cursor = %q, want a1 (deepest on-disk survivor)", got.Cursor)
	}
}

// Rows with unresolved server-side tools are excluded from cursor
// candidacy — the CLI's filters keep them but the API rejects a resume
// onto one (same screen repairLeafForActiveBranch applies).
func TestForkResumeCursorExcludesUnresolvedServerToolRows(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"advise"}}`,
		`{"type":"assistant","uuid":"a-advisor","parentUuid":"u1","message":{"id":"m-adv","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}`,
	}, "\n")

	got := forkCursorFromTranscript(t, transcript, "a-advisor")
	if !got.PinOnDisk {
		t.Fatalf("PinOnDisk = false for a uuid physically in the file: %+v", got)
	}
	if got.Cursor != "u1" {
		t.Fatalf("cursor = %q, want u1 (a-advisor has an unresolved server tool)", got.Cursor)
	}
}

// A file whose only on-branch row at or before the pin is
// filter-dropped yields no cursor at all — the caller's loud-failure
// case.
func TestForkResumeCursorNoSurvivorYieldsEmpty(t *testing.T) {
	transcript := `{"type":"assistant","uuid":"a-dangling","parentUuid":null,"message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{}}]}}`

	got := forkCursorFromTranscript(t, transcript, "a-dangling")
	if !got.PinOnDisk || got.Cursor != "" {
		t.Fatalf("cursor = %+v, want {Cursor:\"\" PinOnDisk:true}", got)
	}
}

// ResolveForkResumeCursor end to end: locate + scan over a real file
// layout, missing-pin as data (not an error), real I/O fault as error.
func TestResolveForkResumeCursorReadsSessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("eval workspace symlinks: %v", err)
	}
	slug := strings.ReplaceAll(canonical, string(filepath.Separator), "-")
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"start"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"reply 1"}]}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(projectDir, "sess-forkpin.jsonl"), []byte(transcript+"\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	got, err := ResolveForkResumeCursor("sess-forkpin", workspace, "a1")
	if err != nil {
		t.Fatalf("ResolveForkResumeCursor(pin on disk): %v", err)
	}
	if !got.PinOnDisk || got.Cursor != "a1" {
		t.Fatalf("cursor = %+v, want {Cursor:a1 PinOnDisk:true}", got)
	}

	got, err = ResolveForkResumeCursor("sess-forkpin", workspace, "a2-not-flushed")
	if err != nil {
		t.Fatalf("ResolveForkResumeCursor(pin missing): %v", err)
	}
	if got.PinOnDisk || got.Cursor != "a1" {
		t.Fatalf("cursor = %+v, want {Cursor:a1 PinOnDisk:false}", got)
	}

	if _, err := ResolveForkResumeCursor("sess-nonexistent", workspace, "a1"); err == nil {
		t.Fatal("ResolveForkResumeCursor(missing session file) = nil error, want the locate failure")
	}

	if _, err := ResolveForkResumeCursor("sess-forkpin", workspace, "   "); err == nil {
		t.Fatal("ResolveForkResumeCursor(blank pin) = nil error, want a refusal")
	}
}
