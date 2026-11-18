package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/atomicfile"
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
// for part of the delivery whose plan is in planFile (WritePlan). The plan
// travels by its file, so the command's length does not grow with it: a
// Claude launch carries every part's commands in one argument. Both
// providers run it through a shell, so every word is quoted. exe and
// planFile must be absolute, since the hook runs in the session's
// workspace, and valid UTF-8, since both providers read the command from
// JSON.
func HookCommand(exe string, event HookEvent, part Part, planFile string) (string, error) {
	for _, path := range []string{exe, planFile} {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("%q is not an absolute path", path)
		}
		if !utf8.ValidString(path) {
			return "", fmt.Errorf("%q is not valid UTF-8, which the provider's hook settings cannot carry", path)
		}
	}
	return stringsx.ShellQuote(exe) + " " + HookSubcommand + " " + string(event) + " " + string(part) + " " + stringsx.ShellQuote(planFile), nil
}

// PlanFile is the file threadID's sessions read their plan from, under the
// app's config directory: one path per thread, so the hook commands, which
// name it and which Codex trusts by their hash, stay the same from one
// session to the next. It is outside the records folder, which a rollback
// deletes under a live session.
func PlanFile(configDir, threadID string) (string, error) {
	if err := checkThreadID(threadID); err != nil {
		return "", err
	}
	return filepath.Join(configDir, plansFolder, threadID+".json"), nil
}

// WritePlan writes plan to path for the hook commands that name it, before
// the session that runs them starts. A plan the hook could not read is
// refused.
func WritePlan(path string, plan Plan) error {
	if err := plan.validate(); err != nil {
		return err
	}
	if err := atomicfile.WriteJSON(path, plan); err != nil {
		return fmt.Errorf("write the memory plan: %w", err)
	}
	return nil
}

// RemovePlan deletes the plan file at path; one that is not there is gone.
func RemovePlan(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete the memory plan: %w", err)
	}
	return nil
}

// readPlan is the plan in the file at path, as WritePlan writes one.
func readPlan(path string) (_ Plan, err error) {
	file, err := os.Open(path)
	if err != nil {
		return Plan{}, fmt.Errorf("read the memory plan: %w", err)
	}
	defer closeInto(file, &err)
	data, err := io.ReadAll(io.LimitReader(file, maxPlanBytes+1))
	if err != nil {
		return Plan{}, fmt.Errorf("read the memory plan %s: %w", path, err)
	}
	if len(data) > maxPlanBytes {
		return Plan{}, fmt.Errorf("the memory plan %s is over %d bytes", path, maxPlanBytes)
	}
	var plan Plan
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return Plan{}, fmt.Errorf("read the memory plan %s: %w", path, err)
	}
	return plan, nil
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
// event, the part and the plan's file, as HookCommand writes them. AtSessionStart
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
// still exits 0. A refused argument, a plan file that cannot be read, or a
// panic, exits 1 with nothing on stdout, which the provider reports as a
// delivery that did not arrive: never 2, which both providers read as a
// refusal of the prompt.
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
		report.printf("want the event, the part and the memory plan's file, got %d arguments\n", len(args))
		return 1
	}
	event := HookEvent(args[0])
	if _, ok := hookEventName[event]; !ok {
		report.printf("%q is not a hook event\n", args[0])
		return 1
	}
	part := Part(args[1])
	var session hookInput
	if err := json.Unmarshal(input, &session); err != nil {
		report.printf("the hook input names no session, so nothing is recorded: %v\n", err)
		session = hookInput{}
	} else if session.AgentID != "" {
		return 0
	} else if session.SessionID == "" {
		report.printf("the hook input names no session, so nothing is recorded\n")
	}
	plan, err := readPlan(args[2])
	if err != nil {
		report.printf("%v\n", err)
		return 1
	}
	if err := plan.validateFor(event, part); err != nil {
		report.printf("%v\n", err)
		return 1
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
		// The sections of a part the plan no longer has are named as gone
		// whether or not this part has a record: with none, its own
		// sections are shown whole beside them.
		var gone []section
		for _, absentPart := range absent {
			record, err := readRecord(plan.Records, session.SessionID, absentPart)
			if err != nil {
				report.printf("%v\n", err)
			}
			if record != nil {
				gone = append(gone, record.Sections...)
			}
		}
		if prior != nil || len(gone) > 0 {
			var before []section
			if prior != nil {
				before = prior.Sections
			}
			if context = changes(append(before, gone...), sections); context == "" {
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
