package main

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
	"agent-overflow/internal/settings"
)

const (
	textGenerationProcessOutputLimit = 64 * 1024
	textGenerationJSONOutputLimit    = 256 * 1024
)

type textGenerationConfig struct {
	Provider string
	Binary   string
	Model    string
	Effort   string
	Exec     textGenerationCLIExecutor
}

// textGenerationCLIExecutor is the seam tests use to stub out Codex/Claude
// CLI invocations. Production wires it to execTextGenerationCLI, which shells
// out via exec.CommandContext.
type textGenerationCLIExecutor func(ctx context.Context, spec textGenerationCLISpec) (textGenerationCLIResult, error)

// textGenerationCLISpec captures everything needed to invoke a provider CLI
// for short text generation.
type textGenerationCLISpec struct {
	// Binary is the resolved path (absolute, or a name to look up on PATH)
	// of the provider CLI.
	Binary string
	// Args are the positional arguments passed to the binary.
	Args []string
	// Cwd is the working directory.
	Cwd string
	// Stdin is the prompt piped to the CLI.
	Stdin string
}

// textGenerationCLIResult captures the observable outcome of a provider-CLI
// invocation. ExitCode 0 indicates success.
type textGenerationCLIResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (a *App) resolveTextGenerationConfig() textGenerationConfig {
	s := a.currentSettings()
	providerKind := strings.TrimSpace(s.TextGenerationProvider)
	if providerKind == "" {
		providerKind = settings.DefaultSettings.TextGenerationProvider
	}

	exec := a.textGenerationExecutor
	if exec == nil {
		exec = execTextGenerationCLI
	}

	cfg := textGenerationConfig{
		Provider: providerKind,
		Effort:   strings.TrimSpace(s.TextGenerationReasoningEffort),
		Exec:     exec,
	}

	switch providerKind {
	case string(provider.Codex):
		cfg.Binary = a.providerBinaryPath(string(provider.Codex))
		cfg.Model = strings.TrimSpace(s.TextGenerationModel)
		if cfg.Model == "" {
			cfg.Model = defaultTextGenerationCodexModel
		}
		if cfg.Effort == "" {
			cfg.Effort = settings.DefaultSettings.TextGenerationReasoningEffort
		}
	case string(provider.Claude):
		cfg.Binary = a.providerBinaryPath(string(provider.Claude))
		cfg.Model = strings.TrimSpace(s.TextGenerationModel)
		if cfg.Model == "" {
			cfg.Model = defaultTextGenerationClaudeModel
		}
	}

	return cfg
}

// execTextGenerationCLI shells out with the provided args, wires stdin from the
// prompt string, and captures stdout/stderr into strings. exec.ExitError is
// normalised into a non-error return with the exit code so callers can branch
// on it.
func execTextGenerationCLI(ctx context.Context, spec textGenerationCLISpec) (textGenerationCLIResult, error) {
	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Stdin = strings.NewReader(spec.Stdin)

	stdout := newCappedTextGenerationOutput(textGenerationProcessOutputLimit)
	stderr := newCappedTextGenerationOutput(textGenerationProcessOutputLimit)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := textGenerationCLIResult{
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

type cappedTextGenerationOutput struct {
	builder   strings.Builder
	limit     int
	truncated bool
}

func newCappedTextGenerationOutput(limit int) cappedTextGenerationOutput {
	return cappedTextGenerationOutput{limit: limit}
}

func (w *cappedTextGenerationOutput) Write(p []byte) (int, error) {
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

func (w cappedTextGenerationOutput) String() string {
	if !w.truncated {
		return w.builder.String()
	}
	return w.builder.String() + "\n[truncated]"
}

// createTextGenerationScratchFiles writes a JSON schema to a temp file and
// creates an empty output file for CLIs that return structured output through a
// file path.
func createTextGenerationScratchFiles(schema string) (schemaPath, outputPath string, cleanup func(), err error) {
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

func readTextGenerationOutputFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	limited, err := io.ReadAll(io.LimitReader(file, textGenerationJSONOutputLimit+1))
	if err != nil {
		return nil, err
	}
	if len(limited) > textGenerationJSONOutputLimit {
		return nil, fmt.Errorf("output exceeds %d bytes", textGenerationJSONOutputLimit)
	}
	return limited, nil
}

// translateCLINotFound turns an exec.ErrNotFound or a context timeout into a
// user-friendly error. Everything else passes through so callers see the raw
// cause.
func translateCLINotFound(cliName string, timeout time.Duration, err error) error {
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

// firstNonEmptyMessage picks the best human-readable error detail from a set of
// candidates. stderr wins over stdout, but an empty stderr falls through to
// whatever else we have.
func firstNonEmptyMessage(candidates ...string) string {
	for _, c := range candidates {
		if t := strings.TrimSpace(c); t != "" {
			return t
		}
	}
	return ""
}

// runCodexTextGeneration drives `codex exec --ephemeral` with a JSON
// schema scratch file and reads the structured response back from the
// output file. Returns the raw output bytes for the caller to decode.
//
// extraArgs are inserted between the standard --output-* flags and the
// trailing "-" stdin sentinel — use them for task-specific flags such as
// repeated `--image PATH`.
func (a *App) runCodexTextGeneration(
	ctx context.Context,
	cfg textGenerationConfig,
	workspace string,
	schemaJSON string,
	extraArgs []string,
	stdin string,
	timeout time.Duration,
) ([]byte, error) {
	schemaPath, outputPath, cleanup, err := createTextGenerationScratchFiles(schemaJSON)
	if err != nil {
		return nil, fmt.Errorf("codex: scratch files: %w", err)
	}
	defer cleanup()

	args := []string{
		"exec",
		"--ephemeral",
		"--skip-git-repo-check",
		"-s", "read-only",
		"--model", cfg.Model,
		"--config", fmt.Sprintf("model_reasoning_effort=%q", cfg.Effort),
		"--output-schema", schemaPath,
		"--output-last-message", outputPath,
	}
	args = append(args, extraArgs...)
	args = append(args, "-")

	result, err := cfg.Exec(ctx, textGenerationCLISpec{
		Binary: cfg.Binary,
		Args:   args,
		Cwd:    workspace,
		Stdin:  stdin,
	})
	if err != nil {
		return nil, translateCLINotFound("codex", timeout, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("codex CLI failed: %s", firstNonEmptyMessage(result.Stderr, result.Stdout, "exit code "+fmt.Sprint(result.ExitCode)))
	}
	return readTextGenerationOutputFile(outputPath)
}

// runClaudeTextGeneration drives `claude -p` with a JSON schema. Returns
// the raw stdout for the caller to decode via decodeClaudeStructuredLastLine.
//
// extraArgs are appended after the standard flags; use them for task-specific
// flags like --effort or --dangerously-skip-permissions.
func (a *App) runClaudeTextGeneration(
	ctx context.Context,
	cfg textGenerationConfig,
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
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, extraArgs...)

	result, err := cfg.Exec(ctx, textGenerationCLISpec{
		Binary: cfg.Binary,
		Args:   args,
		Cwd:    workspace,
		Stdin:  stdin,
	})
	if err != nil {
		return nil, translateCLINotFound("claude", timeout, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("claude CLI failed: %s", firstNonEmptyMessage(result.Stderr, result.Stdout, "exit code "+fmt.Sprint(result.ExitCode)))
	}
	return []byte(result.Stdout), nil
}

// decodeClaudeStructuredLastLine pulls the structured_output envelope from the
// last non-empty line of Claude's `-p --output-format json` stdout. Claude
// emits one JSON object per line; the structured payload lives on the last
// line. Returns an error for empty input or malformed JSON.
func decodeClaudeStructuredLastLine[T any](stdout []byte) (T, error) {
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

// normalizeStructuredOutputLine applies the trim/normalize logic shared by
// commit subjects and thread titles: take the first line, strip surrounding
// quotes and whitespace, collapse internal whitespace runs.
func normalizeStructuredOutputLine(raw string) string {
	out := raw
	if line, _, ok := strings.Cut(out, "\n"); ok {
		out = line
	}
	out = strings.TrimSpace(out)
	out = strings.Trim(out, `'"`+"`")
	out = strings.TrimSpace(out)
	return strings.Join(strings.Fields(out), " ")
}

// capRunesWithEllipsis truncates a string at maxRunes runes, leaving room for
// a 3-char "..." suffix in the t3-code style (72-char subject = 69 runes + "...").
func capRunesWithEllipsis(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return strings.TrimSpace(string(runes[:maxRunes-3])) + "..."
}
