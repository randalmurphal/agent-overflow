package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func note(t *testing.T, kind, text string, at int64) Note {
	t.Helper()
	built, err := NewNote(Draft{Kind: kind, Text: text},
		Provenance{RunID: "run", PhaseID: "implement", Attempt: 1, Wave: 2}, at)
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func TestDirRefusesAnIdThatIsNotOnePathSegment(t *testing.T) {
	for _, id := range []string{"", "  ", ".", "..", "a/b", "../escape", string(filepath.Separator)} {
		if _, err := Dir(t.TempDir(), id); err == nil {
			t.Errorf("Dir accepted root run id %q", id)
		}
	}
	root := t.TempDir()
	dir, err := Dir(root, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, DirName, "run-1"); dir != want {
		t.Fatalf("Dir = %q, want %q", dir, want)
	}
	notes, err := NotesPath(root, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, NotesFileName); notes != want {
		t.Fatalf("NotesPath = %q, want %q", notes, want)
	}
	if _, err := Dir("", "run-1"); err == nil {
		t.Fatal("Dir accepted an empty config root")
	}
}

// The tree is created lazily by the first append. A campaign that records
// nothing leaves no directory behind.
func TestAppendCreatesTheTreeAndAppendsOneLinePerNote(t *testing.T) {
	path, err := NotesPath(t.TempDir(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("the tree exists before anything was recorded")
	}
	for index, text := range []string{"first", "second", "third"} {
		if err := Append(path, note(t, KindLearning, text, int64(index))); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != 3 {
		t.Fatalf("log holds %d newlines, want one per note:\n%s", lines, raw)
	}
	notes, skipped, err := ReadNotes(path)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("ReadNotes = %v, %v", skipped, err)
	}
	if len(notes) != 3 || notes[0].Text != "first" || notes[2].Text != "third" {
		t.Fatalf("notes = %+v, want them oldest first", notes)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != filePerm {
		t.Fatalf("log mode = %v, want %v", info.Mode().Perm(), filePerm)
	}
}

func TestReadNotesTreatsAMissingLogAsEmpty(t *testing.T) {
	path, err := NotesPath(t.TempDir(), "never-wrote")
	if err != nil {
		t.Fatal(err)
	}
	notes, skipped, err := ReadNotes(path)
	if err != nil || len(notes) != 0 || len(skipped) != 0 {
		t.Fatalf("ReadNotes on a missing log = %v, %v, %v", notes, skipped, err)
	}
}

// A crash between a note's bytes reaching the file and the write completing
// leaves a torn FINAL line. The accumulated memory of a whole campaign must
// survive that, so the line is skipped and reported — never fatal.
func TestReadNotesToleratesATornFinalLine(t *testing.T) {
	path, err := NotesPath(t.TempDir(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	for index, text := range []string{"kept one", "kept two"} {
		if err := Append(path, note(t, KindWarning, text, int64(index))); err != nil {
			t.Fatal(err)
		}
	}
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	torn, err := EncodeNote(note(t, KindPattern, "lost to the crash", 9))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(whole, torn[:len(torn)/2]...), filePerm); err != nil {
		t.Fatal(err)
	}
	notes, skipped, err := ReadNotes(path)
	if err != nil {
		t.Fatalf("a torn line was fatal: %v", err)
	}
	if len(notes) != 2 || notes[1].Text != "kept two" {
		t.Fatalf("notes = %+v, want the two whole ones", notes)
	}
	if len(skipped) != 1 || skipped[0].Line != 3 {
		t.Fatalf("skipped = %+v, want line 3 reported", skipped)
	}
	// Reporting must not leak the corrupt bytes: they can be anything.
	if strings.Contains(skipped[0].Reason, "lost to the crash") {
		t.Fatalf("skip reason carries the line's content: %s", skipped[0].Reason)
	}
	// The log stays appendable after the tear: the next note lands on its own
	// line rather than being welded onto the torn one.
	if err := Append(path, note(t, KindHandoff, "after the crash", 10)); err != nil {
		t.Fatal(err)
	}
	notes, skipped, err = ReadNotes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 3 || notes[2].Text != "after the crash" || len(skipped) != 1 {
		t.Fatalf("after re-append: notes = %+v skipped = %+v", notes, skipped)
	}
}

func TestDecodeNotesSkipsWhatIsNotANote(t *testing.T) {
	log := strings.Join([]string{
		`{"kind":"warning","text":"real","provenance":{"runId":"r","wave":0},"at":1}`,
		``,
		`not json at all`,
		`{"kind":"ruling","text":"a kind this build does not know","at":2}`,
		`{"kind":"pattern","text":"   ","at":3}`,
		`{"kind":"handoff","text":"also real","provenance":{"runId":"r","wave":1},"at":4}`,
	}, "\n") + "\n"
	notes, skipped, err := DecodeNotes(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 || notes[0].Text != "real" || notes[1].Text != "also real" {
		t.Fatalf("notes = %+v", notes)
	}
	// The blank line is not a skip — it is not a line anyone wrote.
	if len(skipped) != 3 {
		t.Fatalf("skipped = %+v, want the three unusable lines", skipped)
	}
	for _, entry := range skipped {
		if entry.Line == 2 {
			t.Fatal("a blank line was reported as unreadable")
		}
	}
}

func TestDecodeNotesRefusesAnUnboundedLineRatherThanTruncatingTheLog(t *testing.T) {
	// bufio stops at an over-long line, which would silently lose every note
	// after it. That is a read failure, not a skip.
	log := `{"kind":"warning","text":"` + strings.Repeat("x", MaxLineBytes) + `","at":1}` + "\n"
	if _, _, err := DecodeNotes(strings.NewReader(log)); err == nil {
		t.Fatal("an over-long line was accepted")
	}
}

// The encoder must not HTML-escape: a note about `<-chan` should read back as
// itself in the file a human greps.
func TestEncodeNoteDoesNotEscapeMarkup(t *testing.T) {
	line, err := EncodeNote(note(t, KindLearning, "a <-chan closes on cancel & drains", 1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), "<-chan closes on cancel & drains") {
		t.Fatalf("encoded line escaped markup: %s", line)
	}
}
