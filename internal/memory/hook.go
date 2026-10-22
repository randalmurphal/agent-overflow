package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"runtime/debug"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/stringsx"
)

// HookSubcommand is the argv[1] sentinel that routes this binary to RunHook,
// the hook that delivers a session's memory: whole at a start, after a
// compaction and after a clear (AtSessionStart), and what changed at each
// prompt (AtPrompt).
const HookSubcommand = "__memory-context"

// HookEvent is the provider event a hook command runs at.
type HookEvent string

const (
	// AtSessionStart delivers the whole memory and records it.
	AtSessionStart HookEvent = "session-start"
	// AtPrompt delivers what changed since the record, the whole memory
	// when there is none.
	AtPrompt HookEvent = "prompt"
)

// hookEventName is the hookEventName of each event's output, which both
// providers require to match the event they ran the hook for.
var hookEventName = map[HookEvent]string{AtSessionStart: "SessionStart", AtPrompt: "UserPromptSubmit"}

// maxHookInput bounds the hook input RunHook reads from stdin. Claude and
// Codex write a JSON object; draining it keeps the provider's write from
// failing on a closed pipe, which Codex counts as a failed hook. A prompt's
// input carries the prompt, so the bound is generous.
const maxHookInput = 8 << 20

// HookCommand is the shell command line that runs RunHook from exe at event
// for plan. Both providers run it through a shell, so every word is quoted.
// exe must be absolute, since the hook runs in the session's workspace, and
// valid UTF-8, since both providers read the command from JSON. AtPrompt
// needs the plan's records folder.
func HookCommand(exe string, event HookEvent, plan Plan) (string, error) {
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("%q is not an absolute path", exe)
	}
	if !utf8.ValidString(exe) {
		return "", fmt.Errorf("%q is not valid UTF-8, which the provider's hook settings cannot carry", exe)
	}
	if err := plan.validateFor(event); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode the memory plan: %w", err)
	}
	return stringsx.ShellQuote(exe) + " " + HookSubcommand + " " + string(event) + " " + stringsx.ShellQuote(string(encoded)), nil
}

// hookOutput is the output both providers read: Claude Code and Codex take
// hookSpecificOutput.additionalContext as context for the model
// (docs/references/claude-wire.md, codex-wire.md).
type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// hookInput is what RunHook reads of the provider's input: the provider
// session the record is kept for, and the subagent whose turn ran the hook,
// which Codex names for a thread a turn spawned.
type hookInput struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
}

// RunHook is the entry point of HookSubcommand. Its arguments are the event
// and the plan as HookCommand encodes them. AtSessionStart prints the
// delivery Build makes, stamped, and records it for the provider session
// its input names; AtPrompt prints what changed since that session's
// record, nothing when nothing did and the stamped delivery when there is
// no record, then records what it printed. A subagent's run prints and
// records nothing: memory is the thread's, and a subagent never received
// it. A folder or file that cannot be read is a notice in the delivery; it
// and a record that cannot be read or written go to stderr, and the run
// still exits 0. A refused argument, or a panic, exits 1 with nothing on
// stdout: never 2, which both providers read as a refusal of the prompt.
func RunHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runHookAt(args, stdin, stdout, stderr, time.Now())
}

// runHookAt is RunHook at the time now.
func runHookAt(args []string, stdin io.Reader, stdout, stderr io.Writer, now time.Time) (code int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(stderr, "%s: %v\n%s", HookSubcommand, recovered, debug.Stack())
			code = 1
		}
	}()
	input, inputErr := io.ReadAll(io.LimitReader(stdin, maxHookInput))
	if _, err := io.Copy(io.Discard, stdin); err != nil && inputErr == nil {
		inputErr = err
	}
	if inputErr != nil {
		fmt.Fprintf(stderr, "%s: read the hook input: %v\n", HookSubcommand, inputErr)
	}
	if len(args) != 2 {
		fmt.Fprintf(stderr, "%s: want the event and the memory plan, got %d arguments\n", HookSubcommand, len(args))
		return 1
	}
	event := HookEvent(args[0])
	if _, ok := hookEventName[event]; !ok {
		fmt.Fprintf(stderr, "%s: %q is not a hook event\n", HookSubcommand, args[0])
		return 1
	}
	var plan Plan
	decoder := json.NewDecoder(bytes.NewReader([]byte(args[1])))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		fmt.Fprintf(stderr, "%s: read the memory plan: %v\n", HookSubcommand, err)
		return 1
	}
	if err := plan.validateFor(event); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", HookSubcommand, err)
		return 1
	}
	var session hookInput
	if err := json.Unmarshal(input, &session); err != nil {
		fmt.Fprintf(stderr, "%s: the hook input names no session, so nothing is recorded: %v\n", HookSubcommand, err)
		session = hookInput{}
	} else if session.AgentID != "" {
		return 0
	} else if session.SessionID == "" {
		fmt.Fprintf(stderr, "%s: the hook input names no session, so nothing is recorded\n", HookSubcommand)
	}
	d := readDelivery(plan, now)
	for _, problem := range d.errs {
		fmt.Fprintf(stderr, "%s: %v\n", HookSubcommand, problem)
	}
	sections := d.sections()
	context := ""
	if event == AtPrompt && session.SessionID != "" {
		prior, err := readRecord(plan.Records, session.SessionID)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v; the whole memory is delivered\n", HookSubcommand, err)
		}
		if prior != nil {
			if context = changes(prior.Sections, sections); context == "" {
				return 0
			}
		}
	}
	if context == "" {
		context = Stamp(d.render(sections), now)
	}
	data, err := json.Marshal(hookOutput{HookSpecificOutput: hookSpecificOutput{HookEventName: hookEventName[event], AdditionalContext: context}})
	if err != nil {
		fmt.Fprintf(stderr, "%s: encode the output: %v\n", HookSubcommand, err)
		return 1
	}
	if _, err := stdout.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(stderr, "%s: write the output: %v\n", HookSubcommand, err)
		return 1
	}
	if plan.Records != "" && session.SessionID != "" {
		if err := writeRecord(plan.Records, session.SessionID, sections, now); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", HookSubcommand, err)
		}
	}
	return 0
}
