package agenthome

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/stringsx"
)

// HookSubcommand is the argv[1] sentinel that routes this binary to RunHook:
// the SessionStart hook a standing agent's sessions run, once per context
// file, at a start, after a compaction and after a clear.
const HookSubcommand = "__agent-home-context"

// maxHookInput bounds the hook input RunHook drains from stdin. Claude and
// Codex write a small JSON object; draining it keeps the provider's write
// from failing on a closed pipe, which Codex counts as a failed hook.
const maxHookInput = 1 << 20

// HookCommand is the shell command line that runs RunHook from exe for file
// of home. Both providers run it through a shell, so every word is quoted.
// exe and home must be absolute, since the hook runs in the thread's
// workspace, and valid UTF-8, since both providers read the command from
// JSON.
func HookCommand(exe, home string, file ContextFile) (string, error) {
	for _, path := range []string{exe, home} {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("%q is not an absolute path", path)
		}
		if !utf8.ValidString(path) {
			return "", fmt.Errorf("%q is not valid UTF-8, which the provider's hook settings cannot carry", path)
		}
	}
	return stringsx.ShellQuote(exe) + " " + HookSubcommand + " " + stringsx.ShellQuote(file.Name) + " " + stringsx.ShellQuote(home), nil
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

// RunHook is the entry point of HookSubcommand. args are the context file's
// name and the home. It prints the file as ReadContext delivers it, so the
// output always opens with the file's heading, and exits 0 when it printed:
// a file that cannot be read is delivered as a notice and its problem goes to
// stderr. A refused argument list exits 1 with nothing on stdout.
func RunHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if _, err := io.Copy(io.Discard, io.LimitReader(stdin, maxHookInput)); err != nil {
		fmt.Fprintf(stderr, "%s: read the hook input: %v\n", HookSubcommand, err)
	}
	if len(args) != 2 {
		fmt.Fprintf(stderr, "%s: want a context file and a home, got %d arguments\n", HookSubcommand, len(args))
		return 1
	}
	file, ok := ContextFileNamed(args[0])
	if !ok {
		fmt.Fprintf(stderr, "%s: %q is not a context file\n", HookSubcommand, args[0])
		return 1
	}
	home := args[1]
	if strings.TrimSpace(home) == "" {
		fmt.Fprintf(stderr, "%s: the home is empty\n", HookSubcommand)
		return 1
	}
	if !filepath.IsAbs(home) {
		// The provider runs the hook in the thread's workspace.
		fmt.Fprintf(stderr, "%s: the home %q is not an absolute path\n", HookSubcommand, home)
		return 1
	}
	text, problem := ReadContext(home, file)
	if problem != nil {
		fmt.Fprintf(stderr, "%s: %v\n", HookSubcommand, problem)
	}
	data, err := json.Marshal(hookOutput{HookSpecificOutput: hookSpecificOutput{HookEventName: "SessionStart", AdditionalContext: text}})
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
