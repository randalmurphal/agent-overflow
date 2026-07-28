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

func TestLimitPromptSection_NoopBelowBudget(t *testing.T) {
	in := "short diff"
	if got := LimitPromptSection(in, 1000); got != in {
		t.Errorf("expected no truncation, got %q", got)
	}
}

func TestLimitPromptSection_AppendsTruncatedMarker(t *testing.T) {
	in := strings.Repeat("x", 10_000)
	got := LimitPromptSection(in, 100)
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("expected [truncated] marker, got %q", got[len(got)-40:])
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 100)) {
		t.Error("expected first maxChars bytes preserved")
	}
}

// A textgen prompt is self-contained, so the Claude invocation must run
// isolated from the workspace's customizations: --safe-mode keeps hooks,
// plugins, MCP servers, and CLAUDE.md out of a commit-message run, and
// --no-session-persistence keeps the run out of the workspace's resume
// list. (--bare would be wrong here: it disables OAuth, which subscription
// users authenticate with.)
func TestRunClaude_IsolatesFromWorkspaceCustomizations(t *testing.T) {
	var got CLISpec
	cfg := Config{
		Binary: "claude",
		Model:  "claude-haiku-4-5",
		Exec: func(_ context.Context, spec CLISpec) (CLIResult, error) {
			got = spec
			return CLIResult{Stdout: `{"structured_output":{}}`}, nil
		},
	}

	if _, err := RunClaude(
		context.Background(),
		cfg,
		"/workspace",
		`{"type":"object"}`,
		[]string{"--extra"},
		"prompt",
		time.Minute,
	); err != nil {
		t.Fatalf("RunClaude: %v", err)
	}

	for _, want := range []string{
		"-p", "--json-schema", "--safe-mode", "--no-session-persistence", "--extra",
	} {
		found := false
		for _, arg := range got.Args {
			if arg == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("claude args missing %q: %v", want, got.Args)
		}
	}
	if got.Cwd != "/workspace" || got.Stdin != "prompt" {
		t.Errorf("spec cwd/stdin = %q/%q, want /workspace/prompt", got.Cwd, got.Stdin)
	}
}

// The Codex counterpart of the isolation contract: --ignore-user-config
// keeps a textgen run from booting every MCP server in ~/.codex/config.toml
// (codex exec starts a real thread, so they would all spawn), and
// --ephemeral keeps it out of persisted session history. Auth still reads
// auth.json from CODEX_HOME.
func TestRunCodex_IsolatesFromUserConfig(t *testing.T) {
	var got CLISpec
	cfg := Config{
		Binary: "codex",
		Model:  "gpt-5.6-sol",
		Effort: "low",
		Exec: func(_ context.Context, spec CLISpec) (CLIResult, error) {
			got = spec
			for i, arg := range spec.Args {
				if arg == "--output-last-message" && i+1 < len(spec.Args) {
					if err := os.WriteFile(spec.Args[i+1], []byte(`{}`), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			return CLIResult{}, nil
		},
	}

	if _, err := RunCodex(
		context.Background(),
		cfg,
		"/workspace",
		`{"type":"object"}`,
		[]string{"--extra"},
		"prompt",
		time.Minute,
	); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}

	for _, want := range []string{
		"exec", "--ephemeral", "--ignore-user-config", "--skip-git-repo-check", "--extra",
	} {
		found := false
		for _, arg := range got.Args {
			if arg == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("codex args missing %q: %v", want, got.Args)
		}
	}
	if got.Args[len(got.Args)-1] != "-" {
		t.Errorf("codex args must end with the stdin sentinel; got %v", got.Args)
	}
}

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
		`"  hello   world  "`: "hello world",
		"first\nsecond":       "first",
		"  `quoted`  ":        "quoted",
		"":                    "",
		"already clean":       "already clean",
		`'leading single'`:    "leading single",
		"multi\nline\nthird":  "multi",
	}
	for in, want := range cases {
		if got := NormalizeStructuredOutputLine(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickAvailableProvider_PreferredAvailable(t *testing.T) {
	lookPath := func(bin string) error {
		if bin == "codex" || bin == "claude" {
			return nil
		}
		return exec.ErrNotFound
	}
	if got := PickAvailableProvider("codex", "claude", "codex", lookPath); got != "codex" {
		t.Fatalf("both available, prefer codex: got %q, want codex", got)
	}
	if got := PickAvailableProvider("claude", "claude", "codex", lookPath); got != "claude" {
		t.Fatalf("both available, prefer claude: got %q, want claude", got)
	}
}

func TestPickAvailableProvider_PreferredMissingFallsBack(t *testing.T) {
	onlyClaude := func(bin string) error {
		if bin == "claude" {
			return nil
		}
		return exec.ErrNotFound
	}
	if got := PickAvailableProvider("codex", "claude", "codex", onlyClaude); got != "claude" {
		t.Fatalf("only claude installed but prefer codex: got %q, want claude", got)
	}

	onlyCodex := func(bin string) error {
		if bin == "codex" {
			return nil
		}
		return exec.ErrNotFound
	}
	if got := PickAvailableProvider("claude", "claude", "codex", onlyCodex); got != "codex" {
		t.Fatalf("only codex installed but prefer claude: got %q, want codex", got)
	}
}

func TestPickAvailableProvider_NeitherAvailableReturnsPreferred(t *testing.T) {
	none := func(string) error { return exec.ErrNotFound }
	if got := PickAvailableProvider("codex", "claude", "codex", none); got != "codex" {
		t.Fatalf("neither installed, prefer codex: got %q, want codex (so caller surfaces the codex error)", got)
	}
	if got := PickAvailableProvider("claude", "claude", "codex", none); got != "claude" {
		t.Fatalf("neither installed, prefer claude: got %q, want claude", got)
	}
}

func TestPickAvailableProvider_EmptyBinaryPathTreatedAsUnavailable(t *testing.T) {
	always := func(string) error { return nil }
	// Empty codexBinary means we can't probe it — must fall back to claude.
	if got := PickAvailableProvider("codex", "claude", "", always); got != "claude" {
		t.Fatalf("empty codex binary, claude available: got %q, want claude", got)
	}
}

func TestPickAvailableProvider_UnknownPreferredReturnedVerbatim(t *testing.T) {
	always := func(string) error { return nil }
	if got := PickAvailableProvider("nonsense", "claude", "codex", always); got != "nonsense" {
		t.Fatalf("unknown preferred should pass through: got %q", got)
	}
}

func TestPickAvailableProvider_NilLookPathReturnsPreferred(t *testing.T) {
	// Defensive: production callers always supply lookPath, but a nil
	// callback shouldn't panic.
	if got := PickAvailableProvider("codex", "claude", "codex", nil); got != "codex" {
		t.Fatalf("nil lookPath: got %q, want codex (pass-through)", got)
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
