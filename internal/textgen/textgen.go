// Package textgen runs short, structured-output text-generation tasks
// through a provider CLI (Claude or Codex). The CLI shell-out, scratch
// files, output capture, and JSON post-processing live here so callers
// only need to assemble the per-task argv, schema, and decoder.
package textgen

import (
	"bytes"
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

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/stringsx"
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

	// DefaultAgyModel is the default Antigravity model for short structured
	// text generation runs.
	DefaultAgyModel = "gemini-4-hydrogen"
)

// PickAvailableProviderOrdered returns preferred when its binary resolves,
// otherwise the first provider in order whose binary resolves, otherwise
// preferred unchanged.
func PickAvailableProviderOrdered(
	preferred string,
	binaries map[string]string,
	order []string,
	lookPath func(string) error,
) string {
	if lookPath == nil {
		return preferred
	}
	available := func(name string) bool {
		bin := strings.TrimSpace(binaries[name])
		return bin != "" && lookPath(bin) == nil
	}
	if available(preferred) {
		return preferred
	}
	for _, candidate := range order {
		if candidate == preferred {
			continue
		}
		if available(candidate) {
			return candidate
		}
	}
	return preferred
}

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
// codebase (settings.TextGenerationDefaultFamily, the explicit switches in
// app_text_generation.go). Adding a third provider would mean
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
	Env map[string]string
	// ClaudeRetention answers the transcript sweep for a Claude run in
	// workspace. A run outlives the few seconds the CLI waits before its
	// sweep, so it carries what a session in that workspace does
	// (claude/retention.go). A Claude run refuses to start without it.
	ClaudeRetention func(workspace string) claude.Retention
	// AgyHome is the canonical Antigravity home an agy run reads its login
	// from (provideraccountapp.Manager.AgyCanonicalHome). An agy run refuses
	// to start without it.
	AgyHome agy.Home
	// Admit holds the CLI's whole run against state the caller read while
	// building this config (CLISpec.Admit).
	Admit Admission
	Exec  CLIExecutor
}

// CLIExecutor is the seam tests use to stub out Codex/Claude CLI
// invocations. Production wires it to ExecCLI with the app's reaper.
type CLIExecutor func(ctx context.Context, spec CLISpec) (CLIResult, error)

// CLISpec captures everything needed to invoke a provider CLI for short
// text generation.
type CLISpec struct {
	Binary string
	Args   []string
	Cwd    string
	Stdin  string
	// Env are environment overrides applied over the current process
	// environment (nil applies none), through the same
	// provider.BuildEnvironment rule every provider subprocess gets — one env
	// rule across every CLI Agent Overflow spawns, so an injected variable
	// cannot go missing on one of them, and so the AppImage launch artifacts
	// that rule scrubs are gone from this CLI's environment too.
	Env map[string]string
	// UnsetEnv names inherited variables removed from the child's
	// environment.
	UnsetEnv []string
	// Admit, when set, runs immediately before the process starts. The
	// process starts only if it returns nil, and release runs once no
	// process of the run's group is left, or once its start failed:
	// whatever Admit holds is held for the whole run.
	Admit Admission
}

// Admission admits one CLI run. ctx is the run's, so a wait inside the
// admission ends with the run's budget. stop ends the admitted run: the
// process is signalled as a cancelled run is, and the run returns cause.
type Admission func(ctx context.Context, stop func(cause error)) (release func(), err error)

// CLIResult is the observable outcome of a provider-CLI invocation.
// ExitCode 0 indicates success.
type CLIResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ExecCLI runs the CLI as its own process group, watched by reaper, with the
// prompt on stdin, and captures stdout and stderr into strings. An
// exec.ExitError is normalised into a non-error return with the exit code so
// callers branch on it cleanly.
//
// The run is the whole group: a cancel, a deadline or the admission's stop
// signals every process in it, and the run returns, releasing its
// admission, only once none is left. SIGTERM comes first and SIGKILL only
// after stopGrace, never exec's instant SIGKILL to the leader alone. This CLI
// runs a real turn in the provider's canonical home, so it can be driving the
// CLI's own OAuth refresh when the budget expires or the user cancels;
// Anthropic retires the previous refresh token the moment the token endpoint
// answers, so a SIGKILL landing between that answer and the CLI's write to
// disk destroys the login with no copy anywhere. SIGTERM lets it finish the
// write and exit. A wrapper script or a helper the CLI started is stopped
// with it, so nothing of the run keeps using the credential after the
// release.
func ExecCLI(ctx context.Context, reaper procutil.Reaper, spec CLISpec) (CLIResult, error) {
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	stdout := newCappedOutput(ProcessOutputLimit)
	stderr := newCappedOutput(ProcessOutputLimit)
	var outcome procutil.GroupOutcome
	err := runAdmitted(ctx, spec.Admit, stop, func() error {
		var err error
		outcome, err = procutil.RunGroup(ctx, reaper, procutil.GroupCommand{
			Dir: spec.Cwd,
			// Unconditional: with no overrides this is the inherited
			// environment scrubbed of the AppImage launch artifacts, which a
			// provider CLI must not resolve its own runtime against.
			Env:            provider.BuildEnvironment(spec.Env, spec.UnsetEnv...),
			Argv:           append([]string{spec.Binary}, spec.Args...),
			Stdin:          strings.NewReader(spec.Stdin),
			Output:         &stdout,
			Stderr:         &stderr,
			TerminateGrace: stopGrace,
		})
		return err
	})
	result := CLIResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	survived := errors.Is(err, procutil.ErrGroupSurvived)
	if ctx.Err() != nil {
		// The caller's cancel or deadline, or the admission's stop.
		if survived {
			return result, errors.Join(context.Cause(ctx), err)
		}
		return result, context.Cause(ctx)
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && !survived {
		result.ExitCode = exitErr.ExitCode()
		err = nil
	}
	if err != nil {
		return result, err
	}
	if err := outcome.OutputErr(); err != nil {
		return result, fmt.Errorf("%s output did not arrive whole: %w", filepath.Base(spec.Binary), err)
	}
	return result, nil
}

// stopGrace is how long a stopped run's group gets between SIGTERM and
// SIGKILL.
const stopGrace = 2 * time.Second

// runAdmitted runs run once admit allows it, releasing what admit holds once
// run has returned, which is once the process group is gone, or once its
// start failed. stop cancels ctx, the context run runs under.
func runAdmitted(ctx context.Context, admit Admission, stop func(error), run func() error) error {
	if admit != nil {
		release, err := admit(ctx, stop)
		if err != nil {
			return err
		}
		defer release()
	}
	return run()
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
		return cliTimeoutError{cliName: cliName, timeout: timeout}
	}
	if _, ok := errors.AsType[*os.PathError](err); ok {
		// Deliberately NOT naming pathErr.Path: this message reaches the
		// AudienceAny thread:title_generation channel through RedactError,
		// and the path is the user's configured absolute binary path
		// (security review 2026-08-25, finding 2). cliName already names
		// which CLI to check.
		return fmt.Errorf("%s CLI not found or not executable", cliName)
	}
	return err
}

// cliTimeoutError is the timeout translation above. It is a type rather
// than a `fmt.Errorf` because the sentinel has to survive the rewrite:
// runTextGenWithFallback decides whether to try the alternate provider
// from errors.Is(err, context.Canceled) vs DeadlineExceeded, and a
// flattened message severs that. Wrapping with %w instead would append
// "(context deadline exceeded)" to a message that already says which CLI
// timed out and after how long.
type cliTimeoutError struct {
	cliName string
	timeout time.Duration
}

func (e cliTimeoutError) Error() string {
	return fmt.Sprintf("%s CLI timed out after %s", e.cliName, e.timeout)
}

func (e cliTimeoutError) Unwrap() error { return context.DeadlineExceeded }

// RedactError renders a text-generation error for a log line. A CLI
// failure collapses to a stable opaque string because THIS package
// builds those messages out of the subprocess's own output
// ("codex CLI failed: <stderr>"), which can carry the prompt, the
// workspace, or the environment the run was given. Everything else
// passes through — including the timeout, whose message names the CLI
// and the budget it had and repeats nothing the subprocess wrote.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	// A wrapped *os.PathError names an absolute host path (a scratch/temp
	// file, an output file, a configured binary path), and this string
	// reaches the AudienceAny thread:title_generation channel — collapse it
	// by TYPE so a new error shape carrying a path cannot silently opt out
	// (security review 2026-08-25, finding 2). The op survives for
	// diagnosability; the path never does.
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		return fmt.Sprintf("provider file access failed (%s)", pathErr.Op)
	}
	message := err.Error()
	if strings.Contains(message, "CLI failed") {
		return "provider CLI failed"
	}
	return message
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
		// AO owns Codex's home: the run authenticates from the one every
		// other Codex launch uses.
		UnsetEnv: []string{"CODEX_HOME"},
		Admit:    cfg.Admit,
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
	var retention claude.Retention
	if cfg.ClaudeRetention != nil {
		retention = cfg.ClaudeRetention(workspace)
	}
	line, err := claude.NewCommandLine(retention, "", args...)
	if err != nil {
		return nil, err
	}

	result, err := cfg.Exec(ctx, CLISpec{
		Binary: cfg.Binary,
		Args:   line.Args(),
		Cwd:    workspace,
		Stdin:  stdin,
		Env:    line.Env(cfg.Env),
		// The run uses the config home every other Claude launch uses, the
		// one its retention period was resolved against.
		UnsetEnv: line.UnsetEnv(),
		Admit:    cfg.Admit,
	})
	if err != nil {
		return nil, TranslateCLINotFound("claude", timeout, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("claude CLI failed: %s", FirstNonEmptyMessage(result.Stderr, result.Stdout, "exit code "+fmt.Sprint(result.ExitCode)))
	}
	return []byte(result.Stdout), nil
}

// RunAgy drives `agy --output-format json --json-schema ... --print=<prompt>`
// for one-shot structured output. `agy`'s `-p`/`--print` flag takes the prompt
// as its value rather than reading stdin; passing `--print=<prompt>` avoids
// flag-parsing ambiguity for prompts starting with `-`.
// `--disable-slash-commands` prevents prompt text starting with `/` from
// expanding as a slash command or skill, and AGY_CLI_DISABLE_AUTO_UPDATE=1
// keeps background self-updates off during the run.
func RunAgy(
	ctx context.Context,
	cfg Config,
	workspace string,
	schemaJSON string,
	extraArgs []string,
	stdin string,
	timeout time.Duration,
) ([]byte, error) {
	if strings.TrimSpace(stdin) == "" {
		return nil, fmt.Errorf("agy: empty prompt")
	}
	if cfg.AgyHome.Isolated() {
		return nil, errors.New("agy: text generation runs on the canonical home only")
	}
	launch, err := cfg.AgyHome.Launch(cfg.Env)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--output-format", "json",
		"--json-schema", schemaJSON,
		"--disable-slash-commands",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	args = append(args, extraArgs...)
	args = append(args, "--print="+stdin)

	result, err := cfg.Exec(ctx, CLISpec{
		Binary:   cfg.Binary,
		Args:     args,
		Cwd:      workspace,
		Env:      launch.Env,
		UnsetEnv: launch.Unset,
		Admit:    cfg.Admit,
	})
	if err != nil {
		return nil, TranslateCLINotFound("agy", timeout, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("agy CLI failed: %s", FirstNonEmptyMessage(result.Stderr, result.Stdout, "exit code "+fmt.Sprint(result.ExitCode)))
	}
	return []byte(result.Stdout), nil
}

// DecodeAgyStructured pulls a structured payload of type T out of `agy -p
// --output-format json --json-schema` stdout. Handles wrapped envelopes
// (`structured_output`, `result`, `response`, `text`) as well as direct JSON
// payloads on the final non-empty JSON line.
func DecodeAgyStructured[T any](stdout []byte) (T, error) {
	var zero T
	trimmed := strings.TrimSpace(string(stdout))
	if trimmed == "" {
		return zero, fmt.Errorf("agy returned empty output")
	}

	candidate := trimmed
	if !json.Valid([]byte(candidate)) {
		lines := strings.Split(trimmed, "\n")
		found := ""
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line != "" && json.Valid([]byte(line)) {
				found = line
				break
			}
		}
		if found == "" {
			return zero, fmt.Errorf("agy returned no valid JSON output")
		}
		candidate = found
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(candidate), &envelope); err != nil {
		return zero, fmt.Errorf("decode agy structured output: %w", err)
	}
	if errRaw, hasErr := envelope["error"]; hasErr && len(errRaw) > 0 && string(bytes.TrimSpace(errRaw)) != "null" && string(bytes.TrimSpace(errRaw)) != `""` {
		return zero, fmt.Errorf("agy returned error envelope: %s", strings.TrimSpace(string(errRaw)))
	}
	if statusRaw, hasStatus := envelope["status"]; hasStatus {
		var status string
		if json.Unmarshal(statusRaw, &status) == nil && strings.EqualFold(strings.TrimSpace(status), "error") {
			return zero, fmt.Errorf("agy returned error status envelope: %s", candidate)
		}
	}
	if raw, ok := envelope["structured_output"]; ok && len(raw) > 0 && string(raw) != "null" {
		var out T
		if err := json.Unmarshal(raw, &out); err != nil {
			return zero, fmt.Errorf("decode agy structured_output: %w", err)
		}
		return out, nil
	}
	for _, key := range []string{"result", "response", "text"} {
		raw, ok := envelope[key]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var out T
		if err := json.Unmarshal(raw, &out); err == nil {
			return out, nil
		}
		var asString string
		if err := json.Unmarshal(raw, &asString); err == nil {
			cleaned := stripJSONCodeFence(asString)
			if err := json.Unmarshal([]byte(cleaned), &out); err == nil {
				return out, nil
			}
		}
	}
	var direct T
	if err := json.Unmarshal([]byte(candidate), &direct); err != nil {
		return zero, fmt.Errorf("decode agy structured output: %w", err)
	}
	return direct, nil
}

func stripJSONCodeFence(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
		trimmed = trimmed[nl+1:]
	}
	trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed), "```")
	return strings.TrimSpace(trimmed)
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
//
// The cut backs off to a rune boundary: the budgets are byte counts, but
// a torn UTF-8 sequence handed to a model is not a rounding error.
func LimitPromptSection(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return stringsx.ClipRunes(s, maxChars) + "\n\n[truncated]"
}
