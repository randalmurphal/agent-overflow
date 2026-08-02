// Package textgen runs short, structured-output text-generation tasks
// through a provider CLI (Claude or Codex). The CLI shell-out, scratch
// files, output capture, and JSON post-processing live here so callers
// only need to assemble the per-task argv, schema, and decoder.
package textgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

const (
	// ProcessOutputLimit caps captured stdout/stderr per CLI run. Keeps
	// a runaway provider from filling the host's memory while still
	// preserving enough for diagnostics.
	ProcessOutputLimit = 64 * 1024

	// JSONOutputLimit caps the size of structured-output files written
	// by `codex exec --output-last-message`. 256 KiB is generous for a
	// JSON object but small enough that a misbehaving CLI can't OOM us.
	JSONOutputLimit = 256 * 1024

	// DefaultCodexModel / DefaultClaudeModel are the per-provider model
	// defaults used when settings leaves them blank.
	DefaultCodexModel  = "gpt-5.6-sol"
	DefaultClaudeModel = "claude-haiku-4-5"
)

// PickAvailableProvider returns the preferred provider's name when its
// binary resolves on the local system, otherwise the alternate provider's
// name when ITS binary resolves, otherwise the preferred provider unchanged
// (caller surfaces the "binary not found" error downstream).
//
// `preferred` must be one of "claude" or "codex" (the values backing
// provider.Claude / provider.Codex). An unknown `preferred` is returned
// unchanged — the caller decides whether to error out.
//
// The two-provider universe is hardcoded here, matching the rest of the
// codebase (DefaultCodexModel / DefaultClaudeModel above, the explicit
// switches in app_text_generation.go). Adding a third provider would mean
// rewriting these explicit branches rather than threading a registry.
func PickAvailableProvider(
	preferred, claudeBinary, codexBinary string,
	lookPath func(string) error,
) string {
	if lookPath == nil {
		return preferred
	}
	available := func(bin string) bool {
		return bin != "" && lookPath(bin) == nil
	}
	switch preferred {
	case string(provider.Claude):
		if available(claudeBinary) {
			return string(provider.Claude)
		}
		if available(codexBinary) {
			return string(provider.Codex)
		}
	case string(provider.Codex):
		if available(codexBinary) {
			return string(provider.Codex)
		}
		if available(claudeBinary) {
			return string(provider.Claude)
		}
	}
	return preferred
}

// Config captures the resolved provider, binary path, model, reasoning
// effort, and a (mockable) CLI executor for a text-generation run.
// Callers assemble this once and pass it to RunCodex/RunClaude.
type Config struct {
	Provider string
	Binary   string
	Model    string
	// Effort is the reasoning tier, or empty for a model that advertises none
	// (see provider.ModelDeclaresNoReasoningEffort). RunCodex/RunClaude own the
	// flag and omit it when this is empty — callers must not append their own,
	// or the two runners would disagree about whether it was already there.
	Effort string
	// Env holds the user's custom environment for this provider (see
	// settings.ProviderEnvVars). Text generation drives the SAME CLI against
	// the SAME backend as a chat session, so a base URL or proxy that the
	// session needs is a base URL or proxy this run needs — without it, commit
	// message and thread-title generation would be the one surface that
	// silently kept talking to the vendor default. Applied over the process
	// environment by ExecCLI.
	Env  map[string]string
	Exec CLIExecutor
}

// CLIExecutor is the seam tests use to stub out Codex/Claude CLI
// invocations. Production wires it to ExecCLI, which shells out via
// exec.CommandContext.
type CLIExecutor func(ctx context.Context, spec CLISpec) (CLIResult, error)

// CLISpec captures everything needed to invoke a provider CLI for short
// text generation.
type CLISpec struct {
	Binary string
	Args   []string
	Cwd    string
	Stdin  string
	// Env are environment overrides applied over the current process
	// environment (nil inherits it unchanged), through the same
	// provider.BuildEnvironment rule every provider subprocess gets — one env
	// rule across every CLI Agent Overflow spawns, so an injected variable
	// cannot go missing on one of them.
	Env map[string]string
}

// CLIResult is the observable outcome of a provider-CLI invocation.
// ExitCode 0 indicates success.
type CLIResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ExecCLI shells out with the provided args, wires stdin from the
// prompt string, and captures stdout/stderr into strings. An
// exec.ExitError is normalised into a non-error return with the exit
// code so callers branch on it cleanly.
func ExecCLI(ctx context.Context, spec CLISpec) (CLIResult, error) {
	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Stdin = strings.NewReader(spec.Stdin)
	if len(spec.Env) > 0 {
		cmd.Env = provider.BuildEnvironment(spec.Env)
	}

	stdout := newCappedOutput(ProcessOutputLimit)
	stderr := newCappedOutput(ProcessOutputLimit)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := CLIResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

type cappedOutput struct {
	builder   strings.Builder
	limit     int
	truncated bool
}

func newCappedOutput(limit int) cappedOutput {
	return cappedOutput{limit: limit}
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	remaining := w.limit - w.builder.Len()
	if remaining > 0 {
		toWrite := len(p)
		if toWrite > remaining {
			toWrite = remaining
		}
		_, _ = w.builder.Write(p[:toWrite])
	}
	if len(p) > remaining {
		w.truncated = true
	}
	return len(p), nil
}

func (w cappedOutput) String() string {
	if !w.truncated {
		return w.builder.String()
	}
	return w.builder.String() + "\n[truncated]"
}

// CreateScratchFiles writes a JSON schema to a temp file and creates an
// empty output file for CLIs that return structured output through a
// file path (Codex's --output-last-message). Returns the two paths plus
// a cleanup callback that removes the temp dir.
func CreateScratchFiles(schema string) (schemaPath, outputPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "agent-overflow-textgen-*")
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup = func() {
		_ = os.RemoveAll(dir)
	}

	schemaPath = filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schemaPath, []byte(schema), 0o600); err != nil {
		cleanup()
		return "", "", func() {}, err
	}

	outputPath = filepath.Join(dir, "output.json")
	if err := os.WriteFile(outputPath, []byte(""), 0o600); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	return schemaPath, outputPath, cleanup, nil
}

// ReadOutputFile reads at most JSONOutputLimit bytes from the path.
// Files larger than the limit produce an error so a runaway CLI cannot
// stream gigabytes through the JSON decoder.
func ReadOutputFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	limited, err := io.ReadAll(io.LimitReader(file, JSONOutputLimit+1))
	if err != nil {
		return nil, err
	}
	if len(limited) > JSONOutputLimit {
		return nil, fmt.Errorf("output exceeds %d bytes", JSONOutputLimit)
	}
	return limited, nil
}

// TranslateCLINotFound turns an exec.ErrNotFound or a context timeout
// into a user-friendly error. Everything else passes through so callers
// see the raw cause.
func TranslateCLINotFound(cliName string, timeout time.Duration, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%s CLI not found on PATH", cliName)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s CLI timed out after %s", cliName, timeout)
	}
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		return fmt.Errorf("%s CLI not found: %s", cliName, pathErr.Path)
	}
	return err
}

// FirstNonEmptyMessage picks the first non-blank candidate after
// trimming. stderr wins over stdout when both are populated, but an
// empty stderr falls through to whatever else we have.
func FirstNonEmptyMessage(candidates ...string) string {
	for _, c := range candidates {
		if t := strings.TrimSpace(c); t != "" {
			return t
		}
	}
	return ""
}

// RunCodex drives `codex exec --ephemeral` with a JSON schema scratch
// file and reads the structured response back from the output file.
// Returns the raw output bytes for the caller to decode.
//
// The prompt is self-contained, so `--ignore-user-config` (codex 0.122+)
// skips ~/.codex/config.toml — without it every run starts a real thread
// and boots all of the user's configured MCP servers. Auth still reads
// auth.json from CODEX_HOME, and `--ephemeral` already keeps the run
// out of persisted session history.
//
// extraArgs are inserted between the standard --output-* flags and the
// trailing "-" stdin sentinel — use them for task-specific flags such
// as repeated `--image PATH`.
func RunCodex(
	ctx context.Context,
	cfg Config,
	workspace string,
	schemaJSON string,
	extraArgs []string,
	stdin string,
	timeout time.Duration,
) ([]byte, error) {
	schemaPath, outputPath, cleanup, err := CreateScratchFiles(schemaJSON)
	if err != nil {
		return nil, fmt.Errorf("codex: scratch files: %w", err)
	}
	defer cleanup()

	args := []string{
		"exec",
		"--ephemeral",
		"--ignore-user-config",
		"--skip-git-repo-check",
		"-s", "read-only",
		"--model", cfg.Model,
		"--output-schema", schemaPath,
		"--output-last-message", outputPath,
	}
	if cfg.Effort != "" {
		args = append(args, "--config", fmt.Sprintf("model_reasoning_effort=%q", cfg.Effort))
	}
	args = append(args, extraArgs...)
	args = append(args, "-")

	result, err := cfg.Exec(ctx, CLISpec{
		Binary: cfg.Binary,
		Args:   args,
		Cwd:    workspace,
		Stdin:  stdin,
		Env:    cfg.Env,
	})
	if err != nil {
		return nil, TranslateCLINotFound("codex", timeout, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("codex CLI failed: %s", FirstNonEmptyMessage(result.Stderr, result.Stdout, "exit code "+fmt.Sprint(result.ExitCode)))
	}
	return ReadOutputFile(outputPath)
}

// RunClaude drives `claude -p` with a JSON schema. Returns the raw
// stdout for the caller to decode via DecodeClaudeStructuredLastLine.
//
// The prompt is self-contained, so `--safe-mode` (CLI 2.1.169+) skips the
// workspace's hooks, plugins, MCP servers, and CLAUDE.md — none of which
// should fire for a commit-message or title generation — while OAuth
// inference works normally. `--no-session-persistence` keeps these runs
// out of the workspace's ~/.claude/projects resume list, where each one
// otherwise lands as a resumable transcript.
//
// extraArgs are appended after the standard flags; use them for
// task-specific flags like --effort or --dangerously-skip-permissions.
func RunClaude(
	ctx context.Context,
	cfg Config,
	workspace string,
	schemaJSON string,
	extraArgs []string,
	stdin string,
	timeout time.Duration,
) ([]byte, error) {
	args := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", schemaJSON,
		"--safe-mode",
		"--no-session-persistence",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	args = append(args, extraArgs...)

	result, err := cfg.Exec(ctx, CLISpec{
		Binary: cfg.Binary,
		Args:   args,
		Cwd:    workspace,
		Stdin:  stdin,
		Env:    cfg.Env,
	})
	if err != nil {
		return nil, TranslateCLINotFound("claude", timeout, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("claude CLI failed: %s", FirstNonEmptyMessage(result.Stderr, result.Stdout, "exit code "+fmt.Sprint(result.ExitCode)))
	}
	return []byte(result.Stdout), nil
}

// DecodeClaudeStructuredLastLine pulls the structured_output envelope
// from the last non-empty line of Claude's `-p --output-format json`
// stdout. Claude emits one JSON object per line; the structured
// payload lives on the last line. Returns an error for empty input or
// malformed JSON.
func DecodeClaudeStructuredLastLine[T any](stdout []byte) (T, error) {
	var zero T
	trimmed := strings.TrimSpace(string(stdout))
	if trimmed == "" {
		return zero, fmt.Errorf("claude returned empty output")
	}

	lines := strings.Split(trimmed, "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(lines[i])
		if candidate != "" {
			last = candidate
			break
		}
	}
	if last == "" {
		return zero, fmt.Errorf("claude returned no JSON output")
	}

	var envelope struct {
		StructuredOutput T `json:"structured_output"`
	}
	if err := json.Unmarshal([]byte(last), &envelope); err != nil {
		return zero, fmt.Errorf("decode claude structured output: %w", err)
	}
	return envelope.StructuredOutput, nil
}

// NormalizeStructuredOutputLine applies the trim/normalize logic
// shared by commit subjects and thread titles: take the first line,
// strip surrounding quotes and whitespace, collapse internal whitespace
// runs.
func NormalizeStructuredOutputLine(raw string) string {
	out := raw
	if line, _, ok := strings.Cut(out, "\n"); ok {
		out = line
	}
	out = strings.TrimSpace(out)
	out = strings.Trim(out, `'"`+"`")
	out = strings.TrimSpace(out)
	return strings.Join(strings.Fields(out), " ")
}

// CapRunesWithEllipsis truncates a string at maxRunes runes, leaving
// room for a 3-char "..." suffix in the t3-code style (72-char subject
// = 69 runes + "...").
func CapRunesWithEllipsis(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return strings.TrimSpace(string(runes[:maxRunes-3])) + "..."
}

// LimitPromptSection applies a prompt-layer byte cap with a
// `\n\n[truncated]` marker. Shared by every prompt builder that
// composes context for a structured-output CLI run, so the marker
// stays identical across commit-message, thread-title, etc.
func LimitPromptSection(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n\n[truncated]"
}
