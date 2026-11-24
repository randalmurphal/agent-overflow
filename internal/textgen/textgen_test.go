package textgen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/provider/claude"
)

// userHome answers the transcript sweep for a test run with no period.
func userHome(string) claude.Retention { return claude.UserHomeRetention(0) }

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

// TestLimitPromptSection_CutsOnARuneBoundary: the budget is bytes, but a
// torn UTF-8 sequence in a prompt is not a rounding error — every prompt
// builder budgets its data sections through this function.
func TestLimitPromptSection_CutsOnARuneBoundary(t *testing.T) {
	in := strings.Repeat("é", 100) // two bytes per rune
	got := LimitPromptSection(in, 101)
	if !utf8.ValidString(got) {
		t.Fatalf("LimitPromptSection tore a rune: %q", got)
	}
	body := strings.TrimSuffix(got, "\n\n[truncated]")
	if len(body) != 100 {
		t.Fatalf("body = %d bytes, want the cut backed off to 100", len(body))
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
		Binary:          "claude",
		Model:           "claude-haiku-4-5",
		ClaudeRetention: userHome,
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
	res, err := ExecCLI(ctx, procutiltest.Reaper(), CLISpec{
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
	_, err := ExecCLI(ctx, procutiltest.Reaper(), CLISpec{
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

// A path error's message travels the same wire path as RedactError's output,
// and pathErr.Path is the user's configured absolute binary path, so the
// message must not name it.
func TestTranslateCLINotFound_Messages(t *testing.T) {
	for _, tc := range []struct {
		name, cli string
		err       error
		want      string
	}{
		{"not found", "codex", exec.ErrNotFound, "codex CLI not found on PATH"},
		{"timeout", "claude", context.DeadlineExceeded, "claude CLI timed out after 1s"},
		{"path error omits the path", "claude", &os.PathError{
			Op:   "fork/exec",
			Path: "/home/developer/.local/bin/claude",
			Err:  errors.New("permission denied"),
		}, "claude CLI not found or not executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := TranslateCLINotFound(tc.cli, time.Second, tc.err); err == nil || err.Error() != tc.want {
				t.Fatalf("TranslateCLINotFound = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestTranslateCLINotFound_TimeoutKeepsDeadlineSentinel is the reason the
// timeout translation is a type: runTextGenWithFallback decides whether
// to try the alternate provider from the sentinel, and a wrapped CLI
// error whose deadline was flattened into prose reads as an ordinary
// failure.
func TestTranslateCLINotFound_TimeoutKeepsDeadlineSentinel(t *testing.T) {
	err := TranslateCLINotFound("codex", 3*time.Minute, fmt.Errorf("run: %w", context.DeadlineExceeded))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(%v, context.DeadlineExceeded) = false", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("timeout must not read as a cancellation: %v", err)
	}
	if got := err.Error(); got != "codex CLI timed out after 3m0s" {
		t.Fatalf("message = %q, want no wrapping noise", got)
	}
}

// RedactError's output reaches the AudienceAny thread:title_generation
// channel. The timeout names the CLI and the budget it had and repeats
// nothing the subprocess wrote, so collapsing it would only lose
// information. A wrapped *os.PathError names an absolute host path
// (scratch/temp file, output file); the collapse is typed rather than
// substring-matched so a new error shape carrying a path cannot silently
// opt out (security review 2026-08-25, finding 2).
func TestRedactError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"replaces a CLI failure", errors.New("codex CLI failed: exit status 1"), "provider CLI failed"},
		{"passes other errors through", errors.New("decode claude structured output: unexpected EOF"), "decode claude structured output: unexpected EOF"},
		{"keeps the timeout message", TranslateCLINotFound("codex", 3*time.Minute, context.DeadlineExceeded), "codex CLI timed out after 3m0s"},
		{"collapses path errors", fmt.Errorf("codex: scratch files: %w", &os.PathError{
			Op:   "open",
			Path: "/home/developer/.cache/agent-overflow/scratch-1234.json",
			Err:  errors.New("permission denied"),
		}), "provider file access failed (open)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactError(tc.err); got != tc.want {
				t.Fatalf("RedactError(%q) = %q, want %q", tc.err, got, tc.want)
			}
		})
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

func TestDecodeClaudeStructuredLastLine_RefusesUnreadableInput(t *testing.T) {
	type payload struct{}
	for name, in := range map[string]string{"empty input": "  \n\n", "malformed JSON": "not-json"} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeClaudeStructuredLastLine[payload]([]byte(in)); err == nil {
				t.Fatalf("decode(%q) returned no error", in)
			}
		})
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

// pickBetween is PickAvailableProviderOrdered over claude and codex at the
// given binaries, falling back in that order.
func pickBetween(preferred, claudeBinary, codexBinary string, lookPath func(string) error) string {
	return PickAvailableProviderOrdered(preferred, map[string]string{"claude": claudeBinary, "codex": codexBinary}, []string{"claude", "codex"}, lookPath)
}

// When neither is installed the preferred provider comes back, so the caller
// surfaces its error. An empty binary path cannot be probed and counts as
// not installed.
func TestPickAvailableProvider_FallsBackInOrder(t *testing.T) {
	for _, tc := range []struct {
		name, preferred, codexBinary string
		installed                    []string
		want                         string
	}{
		{"both available, prefer codex", "codex", "codex", []string{"claude", "codex"}, "codex"},
		{"both available, prefer claude", "claude", "codex", []string{"claude", "codex"}, "claude"},
		{"only claude installed, prefer codex", "codex", "codex", []string{"claude"}, "claude"},
		{"only codex installed, prefer claude", "claude", "codex", []string{"codex"}, "codex"},
		{"neither installed, prefer codex", "codex", "codex", nil, "codex"},
		{"neither installed, prefer claude", "claude", "codex", nil, "claude"},
		// The lookup accepts the empty name, so only the guard refuses it.
		{"empty codex binary, prefer codex", "codex", "", []string{"claude", "codex", ""}, "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookPath := func(bin string) error {
				if slices.Contains(tc.installed, bin) {
					return nil
				}
				return exec.ErrNotFound
			}
			if got := pickBetween(tc.preferred, "claude", tc.codexBinary, lookPath); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPickAvailableProvider_NilLookPathReturnsPreferred(t *testing.T) {
	// Defensive: production callers always supply lookPath, but a nil
	// callback shouldn't panic.
	if got := pickBetween("codex", "claude", "codex", nil); got != "codex" {
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

// The effort flag belongs to the runners, not the callers: Config.Effort is
// filled for every text-generation flow, so a caller that forgot to append it
// (thread titles and workflow digests both did) silently ran at the CLI's
// default tier while commit messages ran at the configured one.
func TestRunClaude_RendersEffortFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		effort string
		want   []string
	}{
		{name: "tiered model sends the flag", effort: "low", want: []string{"--effort", "low"}},
		{name: "model without tiers omits it", effort: "", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got CLISpec
			cfg := Config{
				Binary:          "claude",
				Model:           "claude-sonnet-4-6",
				Effort:          tc.effort,
				ClaudeRetention: userHome,
				Exec: func(_ context.Context, spec CLISpec) (CLIResult, error) {
					got = spec
					return CLIResult{Stdout: `{"structured_output":{}}`}, nil
				},
			}
			if _, err := RunClaude(
				context.Background(), cfg, "/workspace", `{"type":"object"}`, nil, "prompt", time.Minute,
			); err != nil {
				t.Fatalf("RunClaude: %v", err)
			}
			assertFlagValue(t, got.Args, "--effort", tc.want)
		})
	}
}

// A text-generation run lives long enough to run the CLI's transcript sweep,
// so it carries the retention a session in its workspace does; none
// configured sends no flag. It also drops an inherited config-home override,
// so the home it sweeps is the one the period was resolved against.
func TestRunClaude_CarriesCleanupPeriodDays(t *testing.T) {
	// Answers only for the workspace the run is in.
	forWorkspace := func(days int) func(string) claude.Retention {
		return func(workspace string) claude.Retention {
			if workspace != "/workspace" {
				return claude.UserHomeRetention(0)
			}
			return claude.UserHomeRetention(days)
		}
	}
	for _, tc := range []struct {
		name string
		days func(string) claude.Retention
		want []string
	}{
		{name: "configured", days: forWorkspace(90), want: []string{"--settings", `{"cleanupPeriodDays":90}`}},
		{name: "none for this workspace", days: forWorkspace(0), want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got CLISpec
			cfg := Config{
				Binary:          "claude",
				ClaudeRetention: tc.days,
				Exec: func(_ context.Context, spec CLISpec) (CLIResult, error) {
					got = spec
					return CLIResult{Stdout: `{"structured_output":{}}`}, nil
				},
			}
			if _, err := RunClaude(
				context.Background(), cfg, "/workspace", `{"type":"object"}`, nil, "prompt", time.Minute,
			); err != nil {
				t.Fatalf("RunClaude: %v", err)
			}
			assertFlagValue(t, got.Args, "--settings", tc.want)
			for _, env := range []provider.ReservedEnv{provider.ClaudeConfigDirEnv, provider.ClaudeSecureStorageConfigDirEnv} {
				if !slices.Contains(got.Env.Clears, env) {
					t.Fatalf("clears = %v, want them to drop %s", got.Env.Clears, env.Name())
				}
			}
		})
	}
}

// A Claude run with no retention answer starts nothing.
func TestRunClaude_RefusesAnUnansweredRetention(t *testing.T) {
	cfg := Config{
		Binary: "claude",
		Exec: func(context.Context, CLISpec) (CLIResult, error) {
			t.Fatal("a run with no retention answer started the CLI")
			return CLIResult{}, nil
		},
	}
	if _, err := RunClaude(context.Background(), cfg, "/workspace", `{"type":"object"}`, nil, "prompt", time.Minute); err == nil {
		t.Fatal("RunClaude accepted a Config with no ClaudeRetention")
	}
}

// A Codex run authenticates from the home every other Codex launch uses.
func TestRunCodex_DropsAnInheritedCodexHome(t *testing.T) {
	var got CLISpec
	cfg := Config{
		Binary: "codex",
		Model:  "gpt-5",
		Exec: func(_ context.Context, spec CLISpec) (CLIResult, error) {
			got = spec
			return CLIResult{}, nil
		},
	}
	if _, err := RunCodex(context.Background(), cfg, t.TempDir(), `{"type":"object"}`, nil, "prompt", time.Minute); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	if !slices.Contains(got.Env.Clears, provider.CodexHomeEnv) {
		t.Fatalf("clears = %v, want them to drop CODEX_HOME", got.Env.Clears)
	}
}

// A refused admission starts no process, and its error is the run's.
func TestExecCLI_RefusedAdmissionStartsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	refused := errors.New("refused")
	_, err := ExecCLI(context.Background(), procutiltest.Reaper(), CLISpec{
		Binary: "/bin/sh",
		Args:   []string{"-c", `: > "$0"`, marker},
		Admit:  func(context.Context, func(error)) (func(), error) { return nil, refused },
	})
	if !errors.Is(err, refused) {
		t.Fatalf("ExecCLI = %v, want the admission's refusal", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the process ran after a refused admission (stat: %v)", statErr)
	}
}

// What an admission holds is released once the process has exited, not once
// it has started: the process here starts with a pause and ends by writing a
// file, and the release looks for that file.
func TestExecCLI_HoldsTheAdmissionUntilTheProcessExits(t *testing.T) {
	exited := filepath.Join(t.TempDir(), "exited")
	released := 0
	result, err := ExecCLI(context.Background(), procutiltest.Reaper(), CLISpec{
		Binary: "/bin/sh",
		Args:   []string{"-c", `sleep 0.2; echo ran; : > "$0"`, exited},
		Admit: func(context.Context, func(error)) (func(), error) {
			return func() {
				released++
				if _, err := os.Stat(exited); err != nil {
					t.Errorf("released before the process exited: %v", err)
				}
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("ExecCLI: %v", err)
	}
	if released != 1 || strings.TrimSpace(result.Stdout) != "ran" {
		t.Fatalf("released %d times, stdout %q; want one release and the run's output", released, result.Stdout)
	}
}

// A process that fails to start releases its admission all the same.
func TestExecCLI_ReleasesTheAdmissionWhenTheStartFails(t *testing.T) {
	released := 0
	_, err := ExecCLI(context.Background(), procutiltest.Reaper(), CLISpec{
		Binary: filepath.Join(t.TempDir(), "missing"),
		Admit: func(context.Context, func(error)) (func(), error) {
			return func() { released++ }, nil
		},
	})
	if err == nil {
		t.Fatal("ExecCLI started a binary that does not exist")
	}
	if released != 1 {
		t.Fatalf("released %d times, want one", released)
	}
}

// The admission's stop ends a running process, and the run returns the cause
// the stop gave, after the process has exited.
func TestExecCLI_TheAdmissionsStopEndsTheRunWithItsCause(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	moved := errors.New("moved")
	stopped := make(chan func(error), 1)
	released := make(chan struct{})
	// The bound only ends a failing run: a stop that does not reach the
	// process leaves it running this long.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ExecCLI(ctx, procutiltest.Reaper(), CLISpec{
			Binary: "/bin/sh",
			Args:   []string{"-c", `: > "$0"; while :; do sleep 0.05; done`, started},
			Admit: func(_ context.Context, stop func(error)) (func(), error) {
				stopped <- stop
				return func() { close(released) }, nil
			},
		})
		done <- err
	}()
	stop := <-stopped
	waitForFile(t, started)
	stop(moved)
	err := <-done
	if !errors.Is(err, moved) {
		t.Fatalf("ExecCLI = %v, want the stop's cause", err)
	}
	select {
	case <-released:
	default:
		t.Fatal("the stopped run returned without releasing its admission")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Both runners hand the config's admission to the executor.
func TestRunners_PassTheAdmissionToTheExecutor(t *testing.T) {
	admitted := 0
	cfg := Config{
		Binary:          "cli",
		Model:           "m",
		ClaudeRetention: userHome,
		Admit: func(context.Context, func(error)) (func(), error) {
			admitted++
			return func() {}, nil
		},
		Exec: func(ctx context.Context, spec CLISpec) (CLIResult, error) {
			if spec.Admit == nil {
				return CLIResult{}, errors.New("no admission in the spec")
			}
			release, err := spec.Admit(ctx, func(error) {})
			if err != nil {
				return CLIResult{}, err
			}
			release()
			return CLIResult{Stdout: "{}"}, nil
		},
	}
	if _, err := RunClaude(context.Background(), cfg, t.TempDir(), "{}", nil, "", time.Second); err != nil {
		t.Fatalf("RunClaude: %v", err)
	}
	if _, err := RunCodex(context.Background(), cfg, t.TempDir(), "{}", nil, "", time.Second); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	if admitted != 2 {
		t.Fatalf("admitted %d starts, want one per runner", admitted)
	}
}

// ExecCLI removes the variables a spec clears from the child's environment.
func TestExecCLI_AppliesTheSpecsClears(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/alternate")
	t.Setenv("AO_TEXTGEN_KEPT", "kept")
	result, err := ExecCLI(context.Background(), procutiltest.Reaper(), CLISpec{
		Binary: "/bin/sh",
		Args:   []string{"-c", `printf '%s|%s' "${CLAUDE_CONFIG_DIR-unset}" "${AO_TEXTGEN_KEPT-unset}"`},
		Env:    provider.EnvInput{Clears: []provider.ReservedEnv{provider.ClaudeConfigDirEnv}},
	})
	if err != nil {
		t.Fatalf("ExecCLI: %v", err)
	}
	if result.Stdout != "unset|kept" {
		t.Fatalf("child environment = %q, want the named variable dropped and the rest inherited", result.Stdout)
	}
}

// The Codex counterpart. An empty effort must drop the whole `--config
// model_reasoning_effort=...` pair rather than send an empty override, which
// codex would read as a malformed value rather than "use your default".
func TestRunCodex_RendersEffortFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		effort string
		want   []string
	}{
		{name: "tiered model sends the override", effort: "high", want: []string{"--config", `model_reasoning_effort="high"`}},
		{name: "model without tiers omits it", effort: "", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got CLISpec
			cfg := Config{
				Binary: "codex",
				Model:  "gpt-5.6-sol",
				Effort: tc.effort,
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
				context.Background(), cfg, "/workspace", `{"type":"object"}`, nil, "prompt", time.Minute,
			); err != nil {
				t.Fatalf("RunCodex: %v", err)
			}
			assertFlagValue(t, got.Args, "--config", tc.want)
		})
	}
}

// assertFlagValue checks that flag is absent from args when want is nil, and
// otherwise that args carries want as a contiguous pair.
func assertFlagValue(t *testing.T, args []string, flag string, want []string) {
	t.Helper()
	for i, arg := range args {
		if arg != flag {
			continue
		}
		if want == nil {
			t.Fatalf("args must omit %s; got %v", flag, args)
		}
		if i+1 >= len(args) || args[i+1] != want[1] {
			t.Fatalf("%s value = %v, want %q; got %v", flag, args[i+1:], want[1], args)
		}
		return
	}
	if want != nil {
		t.Fatalf("args missing %v; got %v", want, args)
	}
}

func TestRunAgyAndDecodeStructured(t *testing.T) {
	var got CLISpec
	// The store is decided as the run starts: the config is built while the
	// login is in the file, and the run starts after it moved to the
	// Keychain.
	fileStorage := true
	cfg := Config{
		Provider: "agy",
		Binary:   "agy",
		Model:    DefaultAgyModel,
		Effort:   "low",
		AgyHome: func(context.Context) (agy.Home, error) {
			return agy.CanonicalHome("", fileStorage)
		},
		Exec: func(_ context.Context, spec CLISpec) (CLIResult, error) {
			got = spec
			return CLIResult{Stdout: `{"result":"{\"title\":\"Add Antigravity textgen\"}"}`}, nil
		},
	}

	fileStorage = false
	out, err := RunAgy(context.Background(), cfg, "/workspace", `{"type":"object"}`, nil, "prompt", time.Minute)
	if err != nil {
		t.Fatalf("RunAgy: %v", err)
	}
	for _, want := range []string{"--output-format", "json", "--json-schema", "--disable-slash-commands", "--model", DefaultAgyModel, "--effort", "low", "--print=prompt"} {
		if !slices.Contains(got.Args, want) {
			t.Errorf("agy args missing %q: %v", want, got.Args)
		}
	}
	if got.Stdin != "" {
		t.Errorf("agy stdin = %q, want empty (prompt rides --print=...)", got.Stdin)
	}
	if got.Env.Pins[provider.AgyDisableAutoUpdateEnv] != "1" {
		t.Errorf("AGY_CLI_DISABLE_AUTO_UPDATE = %q, want 1", got.Env.Pins[provider.AgyDisableAutoUpdateEnv])
	}
	// The run reads the login from the store its home names: the Keychain
	// here, so an inherited file-storage flag is cleared.
	if !slices.Contains(got.Env.Clears, provider.AgyForceFileStorageEnv) {
		t.Errorf("agy clears = %v, want the file-storage flag cleared", got.Env.Clears)
	}
	isolated, err := agy.IsolatedHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	named, err := agy.CanonicalHome(filepath.Join(t.TempDir(), ".gemini", "antigravity-cli"), true)
	if err != nil {
		t.Fatal(err)
	}
	for name, home := range map[string]agy.HomeSource{
		"no source":                   nil,
		"no home":                     agy.Home{}.Source(),
		"an isolated home":            isolated.Source(),
		"a home naming its directory": named.Source(),
	} {
		refused := cfg
		refused.AgyHome = home
		if _, err := RunAgy(context.Background(), refused, "/workspace", `{"type":"object"}`, nil, "prompt", time.Minute); err == nil {
			t.Errorf("RunAgy ran with %s", name)
		}
	}

	type titlePayload struct {
		Title string `json:"title"`
	}
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"result_json_string", string(out), "Add Antigravity textgen"},
		{"structured_output", `{"structured_output":{"title":"Envelope title"}}`, "Envelope title"},
		{"direct_json", `{"title":"Direct title"}`, "Direct title"},
		{"fenced_response", `{"response":"` + "```json\\n{\\\"title\\\":\\\"Fenced title\\\"}\\n```" + `"}`, "Fenced title"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := DecodeAgyStructured[titlePayload]([]byte(tc.raw))
			if err != nil {
				t.Fatalf("DecodeAgyStructured: %v", err)
			}
			if parsed.Title != tc.want {
				t.Fatalf("Title = %q, want %q", parsed.Title, tc.want)
			}
		})
	}
}
