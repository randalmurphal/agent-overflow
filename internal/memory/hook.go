package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/stringsx"
)

// HookSubcommand is the argv[1] sentinel that routes this binary to RunHook:
// the SessionStart hook that delivers a session's memory at a start, after a
// compaction and after a clear, and at a resume when the thread's model does
// not hold the delivery as it reads now.
const HookSubcommand = "__memory-context"

// maxHookInput bounds the hook input RunHook drains from stdin. Claude and
// Codex write a small JSON object; draining it keeps the provider's write
// from failing on a closed pipe, which Codex counts as a failed hook.
const maxHookInput = 1 << 20

// HookCommand is the shell command line that runs RunHook from exe for plan.
// Both providers run it through a shell, so every word is quoted. exe must be
// absolute, since the hook runs in the session's workspace, and valid UTF-8,
// since both providers read the command from JSON.
func HookCommand(exe string, plan Plan) (string, error) {
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("%q is not an absolute path", exe)
	}
	if !utf8.ValidString(exe) {
		return "", fmt.Errorf("%q is not valid UTF-8, which the provider's hook settings cannot carry", exe)
	}
	if err := plan.validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode the memory plan: %w", err)
	}
	return stringsx.ShellQuote(exe) + " " + HookSubcommand + " " + stringsx.ShellQuote(string(encoded)), nil
}

// hookOutput is the SessionStart output both providers read: Claude Code and
// Codex take hookSpecificOutput.additionalContext as context for the model
// (docs/references/claude-wire.md, codex-wire.md).
type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// RunHook is the entry point of HookSubcommand. Its one argument is the plan
// as HookCommand encodes it. It prints the delivery Build makes, stamped
// (Stamp), so the output always opens with Heading and the stamp, and exits 0
// when it printed: a folder or file that cannot be read is a notice in the
// delivery and its problem goes to stderr. A refused argument exits 1 with
// nothing on stdout.
func RunHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runHookAt(args, stdin, stdout, stderr, time.Now())
}

// runHookAt is RunHook at the time now.
func runHookAt(args []string, stdin io.Reader, stdout, stderr io.Writer, now time.Time) int {
	if _, err := io.Copy(io.Discard, io.LimitReader(stdin, maxHookInput)); err != nil {
		fmt.Fprintf(stderr, "%s: read the hook input: %v\n", HookSubcommand, err)
	}
	if len(args) != 1 {
		fmt.Fprintf(stderr, "%s: want the memory plan, got %d arguments\n", HookSubcommand, len(args))
		return 1
	}
	var plan Plan
	decoder := json.NewDecoder(bytes.NewReader([]byte(args[0])))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		fmt.Fprintf(stderr, "%s: read the memory plan: %v\n", HookSubcommand, err)
		return 1
	}
	if err := plan.validate(); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", HookSubcommand, err)
		return 1
	}
	text, problems := Build(plan, now)
	for _, problem := range problems {
		fmt.Fprintf(stderr, "%s: %v\n", HookSubcommand, problem)
	}
	data, err := json.Marshal(hookOutput{HookSpecificOutput: hookSpecificOutput{HookEventName: "SessionStart", AdditionalContext: Stamp(text, now)}})
	if err != nil {
		fmt.Fprintf(stderr, "%s: encode the output: %v\n", HookSubcommand, err)
		return 1
	}
	if _, err := stdout.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(stderr, "%s: write the output: %v\n", HookSubcommand, err)
		return 1
	}
	return 0
}
