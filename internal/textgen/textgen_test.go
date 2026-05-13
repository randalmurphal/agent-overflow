package textgen

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecCLI_StreamsExitCodeWithoutError(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := ExecCLI(ctx, CLISpec{
		Binary: "sh",
		Args:   []string{"-c", "echo hello; exit 3"},
	})
	if err != nil {
		t.Fatalf("ExecCLI returned error for non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("stdout = %q, want hello", res.Stdout)
	}
}

func TestExecCLI_TimeoutSurfacesCtxErr(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := ExecCLI(ctx, CLISpec{
		Binary: "sh",
		Args:   []string{"-c", "sleep 1"},
	})
	if err == nil {
		t.Fatalf("expected context error on timeout")
	}
}

func TestCappedOutput_TruncatesAndMarks(t *testing.T) {
	w := newCappedOutput(5)
	_, _ = w.Write([]byte("hello world"))
	got := w.String()
	if !strings.HasPrefix(got, "hello") {
		t.Fatalf("expected hello prefix, got %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncated marker, got %q", got)
	}
}

func TestCappedOutput_WithinLimitNoMarker(t *testing.T) {
	w := newCappedOutput(100)
	_, _ = w.Write([]byte("hi"))
	if w.String() != "hi" {
		t.Fatalf("unexpected output: %q", w.String())
	}
}

func TestCreateScratchFiles_WritesSchemaAndCleansUp(t *testing.T) {
	schemaPath, outputPath, cleanup, err := CreateScratchFiles(`{"x":1}`)
	if err != nil {
		t.Fatalf("CreateScratchFiles: %v", err)
	}
	defer cleanup()
	b, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if string(b) != `{"x":1}` {
		t.Fatalf("schema bytes = %q", string(b))
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("output path missing: %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(schemaPath)); !os.IsNotExist(err) {
		t.Fatalf("expected dir removed, err=%v", err)
	}
}

func TestReadOutputFile_RejectsOverlarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.json")
	payload := make([]byte, JSONOutputLimit+1)
	for i := range payload {
		payload[i] = 'a'
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ReadOutputFile(path); err == nil {
		t.Fatalf("expected error for oversize output")
	}
}

func TestReadOutputFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadOutputFile(path)
	if err != nil {
		t.Fatalf("ReadOutputFile: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("unexpected payload: %s", string(got))
	}
}

func TestTranslateCLINotFound_NotFoundMessage(t *testing.T) {
	err := TranslateCLINotFound("codex", time.Second, exec.ErrNotFound)
	if err == nil || !strings.Contains(err.Error(), "codex CLI not found on PATH") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTranslateCLINotFound_TimeoutMessage(t *testing.T) {
	err := TranslateCLINotFound("claude", time.Second, context.DeadlineExceeded)
	if err == nil || !strings.Contains(err.Error(), "claude CLI timed out after 1s") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTranslateCLINotFound_PathErrorMessage(t *testing.T) {
	err := TranslateCLINotFound("codex", time.Second, &os.PathError{Op: "stat", Path: "/no/such"})
	if err == nil || !strings.Contains(err.Error(), "/no/such") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTranslateCLINotFound_PassesUnknownThrough(t *testing.T) {
	orig := errors.New("boom")
	if err := TranslateCLINotFound("codex", time.Second, orig); err != orig {
		t.Fatalf("expected pass-through, got %v", err)
	}
}

func TestFirstNonEmptyMessage(t *testing.T) {
	if FirstNonEmptyMessage("", "  ", "third") != "third" {
		t.Fatalf("third should win")
	}
	if FirstNonEmptyMessage("first", "second") != "first" {
		t.Fatalf("first should win")
	}
	if FirstNonEmptyMessage() != "" {
		t.Fatalf("empty input should yield empty string")
	}
}

func TestDecodeClaudeStructuredLastLine_TakesLastNonEmpty(t *testing.T) {
	in := []byte("ignored\n{\"structured_output\":{\"v\":7}}\n\n")
	type payload struct {
		V int `json:"v"`
	}
	got, err := DecodeClaudeStructuredLastLine[payload](in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.V != 7 {
		t.Fatalf("v = %d, want 7", got.V)
	}
}

func TestDecodeClaudeStructuredLastLine_EmptyInput(t *testing.T) {
	type payload struct{}
	if _, err := DecodeClaudeStructuredLastLine[payload]([]byte("  \n\n")); err == nil {
		t.Fatalf("expected error for empty input")
	}
}

func TestDecodeClaudeStructuredLastLine_MalformedJSON(t *testing.T) {
	type payload struct{}
	if _, err := DecodeClaudeStructuredLastLine[payload]([]byte("not-json")); err == nil {
		t.Fatalf("expected decode error")
	}
}

func TestNormalizeStructuredOutputLine(t *testing.T) {
	cases := map[string]string{
		`"  hello   world  "`:         "hello world",
		"first\nsecond":                "first",
		"  `quoted`  ":                 "quoted",
		"":                             "",
		"already clean":                "already clean",
		`'leading single'`:             "leading single",
		"multi\nline\nthird":           "multi",
	}
	for in, want := range cases {
		if got := NormalizeStructuredOutputLine(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapRunesWithEllipsis(t *testing.T) {
	if got := CapRunesWithEllipsis("short", 10); got != "short" {
		t.Fatalf("short unchanged: %q", got)
	}
	if got := CapRunesWithEllipsis("0123456789abcdef", 10); got != "0123456..." {
		t.Fatalf("cap = %q, want 0123456...", got)
	}
	if got := CapRunesWithEllipsis("café latte", 7); got != "café..." {
		t.Fatalf("rune-aware cap = %q, want café...", got)
	}
}
