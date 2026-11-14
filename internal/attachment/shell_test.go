package attachment

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/store"
)

func writeCapture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capture.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSaveShellOutputAdoptsTheCaptureAsAFile(t *testing.T) {
	s, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	shell := store.AttachmentShell{Command: "git status", ExitCode: 1, DurationMs: 12}
	capture := writeCapture(t, "On branch main\n")
	record, err := s.SaveShellOutput("t1", shell, capture, 5)
	if err != nil {
		t.Fatal(err)
	}
	shell.Preview = "On branch main\n"
	if record.Kind != store.AttachmentKindFile || record.Shell != shell || record.Size != 15 ||
		record.Filename != ShellOutputFilename || record.CreatedAt != 5 {
		t.Fatalf("record: %+v", record)
	}
	stored, path, err := s.PathForThread("t1", record.ID)
	if err != nil || stored != record {
		t.Fatalf("stored row: %+v %v", stored, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "On branch main\n" {
		t.Fatalf("stored bytes: %q %v", data, err)
	}
	if _, err := os.Stat(capture); err != nil {
		t.Fatalf("the capture belongs to its owner and was removed: %v", err)
	}
	if _, _, err := s.ReadThreadBytes("t1", record.ID); !errors.Is(err, ErrNotAnImage) {
		t.Fatalf("shell output was served as an image: %v", err)
	}
}

func TestSaveShellOutputAcceptsEmptyOutputAndRefusesBadInput(t *testing.T) {
	s, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	shell := store.AttachmentShell{Command: "true"}
	record, err := s.SaveShellOutput("t1", shell, writeCapture(t, ""), 1)
	if err != nil || record.Size != 0 {
		t.Fatalf("empty output: %+v %v", record, err)
	}
	if _, err := s.SaveShellOutput("t1", store.AttachmentShell{}, writeCapture(t, "x"), 1); err == nil {
		t.Fatal("a capture with no command was stored")
	}
	if _, err := s.SaveShellOutput("", shell, writeCapture(t, "x"), 1); err == nil {
		t.Fatal("a capture with no thread was stored")
	}
	if _, err := s.SaveShellOutput("t1", shell, filepath.Join(t.TempDir(), "missing"), 1); err == nil {
		t.Fatal("a missing capture was stored")
	}
	if _, err := s.SaveShellOutput("t1", shell, t.TempDir(), 1); err == nil {
		t.Fatal("a directory was stored as output")
	}
	rows, err := s.List("t1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("refused saves left rows: %+v %v", rows, err)
	}
}

func TestCopyToThreadKeepsShellMetadata(t *testing.T) {
	s, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	seedThread(t, meta, "t2")
	shell := store.AttachmentShell{Command: "make test", ExitCode: 2, DurationMs: 9, Truncated: true}
	record, err := s.SaveShellOutput("t1", shell, writeCapture(t, "FAIL\n"), 1)
	if err != nil {
		t.Fatal(err)
	}
	shell.Preview = "FAIL\n"
	clone, err := s.CopyToThread("t1", "t2", record.ID, 2)
	if err != nil || clone.Shell != shell {
		t.Fatalf("clone: %+v %v", clone, err)
	}
	if _, chunk, err := s.ReadShellOutput("t2", clone.ID, 0, 0); err != nil || chunk.Data != "FAIL\n" {
		t.Fatalf("clone output: %+v %v", chunk, err)
	}
}

func TestReadShellOutputPagesOnRuneBoundariesAndIsShellOnly(t *testing.T) {
	s, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	seedThread(t, meta, "t2")
	content := strings.Repeat("héllo wörld €\n", 40)
	record, err := s.SaveShellOutput("t1", store.AttachmentShell{Command: "cat x"}, writeCapture(t, content), 1)
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	offset := int64(0)
	for range 1000 {
		_, chunk, err := s.ReadShellOutput("t1", record.ID, offset, 7)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(chunk.Data) || strings.ContainsRune(chunk.Data, utf8.RuneError) {
			t.Fatalf("chunk at %d splits a rune: %q", offset, chunk.Data)
		}
		if chunk.Offset != offset || chunk.Size != int64(len(content)) {
			t.Fatalf("chunk bounds: %+v", chunk)
		}
		got.WriteString(chunk.Data)
		if chunk.Next == chunk.Size {
			break
		}
		if chunk.Next <= offset {
			t.Fatalf("read did not advance past %d", offset)
		}
		offset = chunk.Next
	}
	if got.String() != content {
		t.Fatalf("paged output differs: %d bytes of %d", got.Len(), len(content))
	}
	if _, _, err := s.ReadShellOutput("t1", record.ID, int64(len(content))+1, 0); err == nil {
		t.Fatal("an offset past the end was accepted")
	}
	if _, _, err := s.ReadShellOutput("t2", record.ID, 0, 0); err == nil {
		t.Fatal("another thread read the output")
	}
	file, err := uploadBytes(s, "t1", "notes.txt", "text/plain", []byte("private file"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadShellOutput("t1", file.ID, 0, 0); !errors.Is(err, ErrNotShellOutput) {
		t.Fatalf("an uploaded file was read as shell output: %v", err)
	}
}

func TestShellPromptOutputInlinesSmallOutputAndStripsTerminalControl(t *testing.T) {
	path := writeCapture(t, "\x1b[31mred\x1b[0m\r\nbar 10%\rbar 100%\n\xff")
	got, err := ShellPromptOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "red\nbar 10%\nbar 100%\n�"; got != want {
		t.Fatalf("prompt output %q, want %q", got, want)
	}
}

func TestShellPromptOutputExcerptsLargeOutputAroundThePath(t *testing.T) {
	var content strings.Builder
	for i := 0; content.Len() <= ShellInlineLimitBytes*2; i++ {
		content.WriteString("line ")
		content.WriteString(strings.Repeat("é", 20))
		content.WriteString("\n")
	}
	body := "FIRST LINE\n" + content.String() + "LAST LINE\n"
	path := writeCapture(t, body)
	got, err := ShellPromptOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > ShellInlineLimitBytes {
		t.Fatalf("excerpt is %d bytes", len(got))
	}
	if !strings.HasPrefix(got, "FIRST LINE\n") || !strings.HasSuffix(got, "LAST LINE\n") {
		t.Fatalf("excerpt lost an end: %q … %q", got[:20], got[len(got)-20:])
	}
	if !strings.Contains(got, "of output omitted. The full output is saved at: "+path+" ...]") {
		t.Fatal("excerpt does not point at the full output")
	}
	if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
		t.Fatal("excerpt splits a rune")
	}
	for _, line := range strings.Split(got, "\n") {
		if line != "" && !strings.HasPrefix(line, "line ") && !strings.HasPrefix(line, "[... ") &&
			line != "FIRST LINE" && line != "LAST LINE" {
			t.Fatalf("excerpt cut a line: %q", line)
		}
	}

	exact := writeCapture(t, strings.Repeat("a", ShellInlineLimitBytes))
	if got, err := ShellPromptOutput(exact); err != nil || len(got) != ShellInlineLimitBytes {
		t.Fatalf("output at the limit was excerpted: %d bytes %v", len(got), err)
	}
}

func TestShellPreviewKeepsWholeLinesWithinItsBounds(t *testing.T) {
	long := strings.Repeat("x", ShellPreviewBytes*2)
	for name, tc := range map[string]struct{ in, want string }{
		"short output is whole": {"a\nb\n", "a\nb\n"},
		"no trailing newline":   {"a\nb", "a\nb"},
		"empty":                 {"", ""},
		"line bound":            {strings.Repeat("l\n", ShellPreviewLines+5), strings.Repeat("l\n", ShellPreviewLines)},
		"byte bound ends on a line": {
			"first\n" + long + "\n", "first\n",
		},
		"one long line ends on a rune": {
			strings.Repeat("é", ShellPreviewBytes), strings.Repeat("é", ShellPreviewBytes/2),
		},
		"an escape sequence is never cut": {
			strings.Repeat("y", ShellPreviewBytes-2) + "\x1b[31mred", strings.Repeat("y", ShellPreviewBytes-2),
		},
	} {
		got, err := shellPreview(writeCapture(t, tc.in))
		if err != nil || got != tc.want {
			t.Errorf("%s: got %d bytes %q, want %d bytes (err %v)", name, len(got), truncateForLog(got), len(tc.want), err)
		}
	}
}

func truncateForLog(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

func TestSaveShellOutputRecordsThePreview(t *testing.T) {
	s, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	record, err := s.SaveShellOutput("t1", store.AttachmentShell{Command: "seq 100"},
		writeCapture(t, strings.Repeat("row\n", 100)), 1)
	if err != nil || record.Shell.Preview != strings.Repeat("row\n", ShellPreviewLines) {
		t.Fatalf("preview: %q %v", record.Shell.Preview, err)
	}
	stored, _, err := s.PathForThread("t1", record.ID)
	if err != nil || stored.Shell.Preview != record.Shell.Preview {
		t.Fatalf("stored preview: %q %v", stored.Shell.Preview, err)
	}
}
