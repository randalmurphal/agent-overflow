package uitrace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendWritesJSONLines(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	path, err := tr.Append([]string{
		`{"label":"chat.state","data":{"threadId":"thread-1"}}`,
		`{"label":"chat.dom"}`,
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if path != tr.Path() {
		t.Fatalf("Append path = %q, want %q", path, tr.Path())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(got) != 2 {
		t.Fatalf("line count = %d, want 2; data=%q", len(got), data)
	}
	if got[0] != `{"label":"chat.state","data":{"threadId":"thread-1"}}` {
		t.Fatalf("first line = %q", got[0])
	}
}

func TestAppendEmptyBatchIsNoOp(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	path, err := tr.Append(nil)
	if err != nil {
		t.Fatalf("Append(nil) returned error: %v", err)
	}
	if path != tr.Path() {
		t.Fatalf("Append(nil) path = %q, want %q", path, tr.Path())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no trace file on empty batch, stat err = %v", err)
	}
}

func TestAppendRejectsInvalidJSON(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	_, err = tr.Append([]string{`{"label":`})
	if err == nil {
		t.Fatal("Append returned nil error")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("error = %q, want invalid JSON message", err)
	}
}

func TestAppendRejectsOversizedLine(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	line := `{"value":"` + strings.Repeat("x", MaxLineBytes) + `"}`

	_, err = tr.Append([]string{line})
	if err == nil {
		t.Fatal("Append returned nil error")
	}
	if !strings.Contains(err.Error(), "max") {
		t.Fatalf("error = %q, want max size message", err)
	}
}

func TestAppendRejectsOversizedBatch(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	line := `{"value":"` + strings.Repeat("x", 3000) + `"}`
	lines := make([]string, 0, MaxBatchLines)
	for i := 0; i < MaxBatchLines; i++ {
		lines = append(lines, line)
	}

	_, err = tr.Append(lines)
	if err == nil {
		t.Fatal("Append returned nil error")
	}
	if !strings.Contains(err.Error(), "batch") {
		t.Fatalf("error = %q, want batch size message", err)
	}
}

func TestAppendRejectsTooManyLines(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	lines := make([]string, MaxBatchLines+1)
	for i := range lines {
		lines[i] = `{"x":1}`
	}

	_, err = tr.Append(lines)
	if err == nil {
		t.Fatal("Append returned nil error")
	}
	if !strings.Contains(err.Error(), "max") {
		t.Fatalf("error = %q, want max lines message", err)
	}
}

func TestAppendSkipsBlankLines(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	path, err := tr.Append([]string{"", "   ", `{"a":1}`, "\r\n"})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	if strings.TrimSpace(string(data)) != `{"a":1}` {
		t.Fatalf("content = %q, want single JSON line", data)
	}
}

func TestAppendStripsTrailingNewlines(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	path, err := tr.Append([]string{`{"a":1}` + "\r\n"})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	if string(data) != `{"a":1}`+"\n" {
		t.Fatalf("content = %q, want single normalized line", data)
	}
}

func TestAppendRotatesAtMaxFileBytes(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	// Pre-seed the trace file at MaxFileBytes so any non-empty append
	// pushes size+pending past the cap and forces rotation.
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bulk := strings.Repeat("a", MaxFileBytes)
	if err := os.WriteFile(tr.Path(), []byte(bulk), 0644); err != nil {
		t.Fatalf("seed trace file: %v", err)
	}

	if _, err := tr.Append([]string{`{"a":1}`}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	rotated, err := os.ReadFile(tr.Path() + ".1")
	if err != nil {
		t.Fatalf("read rotated file: %v", err)
	}
	if len(rotated) != len(bulk) {
		t.Fatalf("rotated size = %d, want %d", len(rotated), len(bulk))
	}

	current, err := os.ReadFile(tr.Path())
	if err != nil {
		t.Fatalf("read current file: %v", err)
	}
	if string(current) != `{"a":1}`+"\n" {
		t.Fatalf("current file = %q, want single appended line", current)
	}
}

func TestAppendReplacesPreviousRotation(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, DirName), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(tr.Path()+".1", []byte("stale"), 0644); err != nil {
		t.Fatalf("seed stale rotation: %v", err)
	}
	bulk := strings.Repeat("b", MaxFileBytes)
	if err := os.WriteFile(tr.Path(), []byte(bulk), 0644); err != nil {
		t.Fatalf("seed trace file: %v", err)
	}

	if _, err := tr.Append([]string{`{"a":1}`}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	rotated, err := os.ReadFile(tr.Path() + ".1")
	if err != nil {
		t.Fatalf("read rotated file: %v", err)
	}
	if string(rotated) == "stale" {
		t.Fatal("stale rotation was not replaced")
	}
}

func TestNewRequiresConfigDir(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") returned nil error")
	}
}

func TestPathLayout(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	want := filepath.Join(dir, DirName, FileName)
	if tr.Path() != want {
		t.Fatalf("Path() = %q, want %q", tr.Path(), want)
	}
}
