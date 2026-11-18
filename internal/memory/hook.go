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
// the hook that delivers a part of a session's memory: whole at a start,
// after a compaction and after a clear (AtSessionStart), and what changed
// at each prompt (AtPrompt).
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
// for part of plan's delivery. Both providers run it through a shell, so
// every word is quoted. exe must be absolute, since the hook runs in the
// session's workspace, and valid UTF-8, since both providers read the
// command from JSON. AtPrompt needs the plan's records folder.
func HookCommand(exe string, event HookEvent, part Part, plan Plan) (string, error) {
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("%q is not an absolute path", exe)
	}
	if !utf8.ValidString(exe) {
		return "", fmt.Errorf("%q is not valid UTF-8, which the provider's hook settings cannot carry", exe)
	}
	if err := plan.validateFor(event, part); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode the memory plan: %w", err)
	}
	return stringsx.ShellQuote(exe) + " " + HookSubcommand + " " + string(event) + " " + string(part) + " " + stringsx.ShellQuote(string(encoded)), nil
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

// RunHook is the entry point of HookSubcommand. Its arguments are the
// event, the part and the plan as HookCommand encodes them. AtSessionStart
// prints the part as Build makes it, stamped, and records it for the
// provider session its input names; AtPrompt prints what changed in the
// part since that session's record of it, nothing when nothing did and the
// stamped part when there is no record, then records what it printed. Each
// part keeps a record of its own, so the runs of a delivery's parts, which
// a provider may run at once, never write the same file; the user part's
// run also names as gone the sections of a part the session's record holds
// and the plan no longer has. A subagent's run prints and records nothing: memory is the thread's, and a subagent never received
// it. A folder or file that cannot be read is a notice in the delivery; it
// and a record that cannot be read or written go to stderr, and the run
// still exits 0. A refused argument, or a panic, exits 1 with nothing on
// stdout: never 2, which both providers read as a refusal of the prompt.
// A run whose report stderr does not take exits 1; a report lost before
// the delivery also stops the run, so it prints and records nothing.
func RunHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runHookAt(args, stdin, stdout, stderr, time.Now())
}

// hookStderr writes a run's reports to stderr and keeps the first write
// that failed, which fails the run: stderr is where the provider shows why
// a run went wrong, and a run that cannot say so does not pass as clean.
type hookStderr struct {
	w   io.Writer
	err error
}

func (s *hookStderr) printf(format string, args ...any) {
	if _, err := fmt.Fprintf(s.w, "%s: "+format, append([]any{HookSubcommand}, args...)...); err != nil && s.err == nil {
		s.err = err
	}
}

// runHookAt is RunHook at the time now.
func runHookAt(args []string, stdin io.Reader, stdout, stderr io.Writer, now time.Time) (code int) {
	report := &hookStderr{w: stderr}
	defer func() {
		if recovered := recover(); recovered != nil {
			report.printf("%v\n%s", recovered, debug.Stack())
			code = 1
		}
		if report.err != nil {
			code = 1
		}
	}()
	input, inputErr := io.ReadAll(io.LimitReader(stdin, maxHookInput))
	if _, err := io.Copy(io.Discard, stdin); err != nil && inputErr == nil {
		inputErr = err
	}
	if inputErr != nil {
		report.printf("read the hook input: %v\n", inputErr)
	}
	if len(args) != 3 {
		report.printf("want the event, the part and the memory plan, got %d arguments\n", len(args))
		return 1
	}
	event := HookEvent(args[0])
	if _, ok := hookEventName[event]; !ok {
		report.printf("%q is not a hook event\n", args[0])
		return 1
	}
	part := Part(args[1])
	var plan Plan
	decoder := json.NewDecoder(bytes.NewReader([]byte(args[2])))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		report.printf("read the memory plan: %v\n", err)
		return 1
	}
	if err := plan.validateFor(event, part); err != nil {
		report.printf("%v\n", err)
		return 1
	}
	var session hookInput
	if err := json.Unmarshal(input, &session); err != nil {
		report.printf("the hook input names no session, so nothing is recorded: %v\n", err)
		session = hookInput{}
	} else if session.AgentID != "" {
		return 0
	} else if session.SessionID == "" {
		report.printf("the hook input names no session, so nothing is recorded\n")
	}
	d := readDelivery(plan, now, part)
	// The warnings part reads every part to measure it; each part's own run
	// reports its failures.
	if part != PartWarnings {
		for _, problem := range d.errs {
			report.printf("%v\n", problem)
		}
	}
	sections := d.sectionsOf(part)
	// The user part, which every delivery has, speaks for the parts an
	// earlier session's plan had and this one does not: a prompt names
	// their sections as gone, and their records go once it has printed.
	var absent []Part
	if part == PartUser && plan.Records != "" && session.SessionID != "" {
		absent = plan.absentParts()
	}
	context := ""
	if event == AtPrompt && session.SessionID != "" {
		prior, err := readRecord(plan.Records, session.SessionID, part)
		if err != nil {
			report.printf("%v; the whole memory is delivered\n", err)
		}
		if prior != nil {
			before := prior.Sections
			for _, gone := range absent {
				record, err := readRecord(plan.Records, session.SessionID, gone)
				if err != nil {
					report.printf("%v\n", err)
				}
				if record != nil {
					before = append(before, record.Sections...)
				}
			}
			if context = changes(before, sections); context == "" {
				return 0
			}
		}
	}
	if report.err != nil {
		return 1
	}
	if context == "" {
		context = Stamp(d.render(part), now)
	}
	data, err := json.Marshal(hookOutput{HookSpecificOutput: hookSpecificOutput{HookEventName: hookEventName[event], AdditionalContext: context}})
	if err != nil {
		report.printf("encode the output: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(append(data, '\n')); err != nil {
		report.printf("write the output: %v\n", err)
		return 1
	}
	if plan.Records != "" && session.SessionID != "" {
		if err := writeRecord(plan.Records, session.SessionID, part, sections, now); err != nil {
			report.printf("%v\n", err)
		}
	}
	for _, gone := range absent {
		if err := removeRecord(plan.Records, session.SessionID, gone); err != nil {
			report.printf("%v\n", err)
		}
	}
	return 0
}
