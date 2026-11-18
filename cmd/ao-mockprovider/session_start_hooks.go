package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/procutil"
)

// SessionStart hooks as both CLIs run the ones a session's configuration
// declares (docs/references/claude-wire.md and codex-wire.md, SessionStart
// hooks). Each command of a group whose matcher takes the source runs through
// `sh -c` in the session's cwd, with the hook input on stdin: Claude Code's
// shell, and the POSIX shell Codex's thread shell stands for. A run that exits
// 0 delivers its stdout as context: hookSpecificOutput.additionalContext of a
// JSON object, the trimmed text otherwise. The adapters write the runs in
// their CLI's frames, and the contexts reach the spec as
// session_start_context before the frames of their outcome.
//
// Claude Code runs a fresh start's hooks before its first system/init and a
// compaction's before its compact_boundary; Codex runs a start's at the next
// turn start and a compaction's right after it. A scenario writes the turn
// start and the compaction frames, so the adapters run the hooks around those
// frames (scenarioFrameActor).

// sessionStartHook is one command handler of a SessionStart matcher group.
type sessionStartHook struct {
	matcher string
	command string
	timeout time.Duration
	// order is the handler's position across the configuration, Codex's
	// displayOrder.
	order int
	// limit is the most of the handler's context the model sees whole, in
	// UTF-16 units when utf16 is set (Claude) and bytes otherwise (Codex);
	// 0 for no limit. A longer context reaches the model as its start and
	// the path of the file the CLI saved it to.
	limit int
	utf16 bool
}

// hookOutputLimit is the default limit of a hook's context on both CLIs:
// Claude's 10,000 UTF-16 units, and Codex's 2,500 tokens of four bytes.
const hookOutputLimit = 10000

// spilledPreview is how many characters of a context over its limit the
// model sees, Claude's preview.
const spilledPreview = 2000

// modelContext is text, a context the handler delivered, as the model
// receives it: whole within the limit, else its start and a line naming the
// saved file (Claude's bundle and codex-rs/hooks/src/output_spill.rs). The
// spec reads this, so a part over the limit shows as cut.
func (h sessionStartHook) modelContext(text string) string {
	size := len(text)
	if h.utf16 {
		size = len(utf16.Encode([]rune(text)))
	}
	if h.limit == 0 || size <= h.limit {
		return text
	}
	return string([]rune(text)[:spilledPreview]) + "\n\nFull hook output saved to: (ao-mockprovider keeps no file)"
}

// defaultSessionStartHookTimeout is the timeout of a handler that names
// none, on both CLIs (Codex's normalize_command_hook, rust-v0.162.1).
const defaultSessionStartHookTimeout = 600 * time.Second

// sessionStartHookGroups is the `SessionStart` value of a hooks map, as
// Claude's --settings and Codex's thread config both spell it.
type sessionStartHookGroups []struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout *int   `json:"timeout"`
		// AdditionalContextLimit is Codex's per-handler limit in tokens, 0
		// for none.
		AdditionalContextLimit *int `json:"additionalContextLimit"`
	} `json:"hooks"`
}

// sessionStartMatcherTakes reports whether a matcher takes source: an empty
// matcher or "*" takes every source, one of letters, digits, `_` and `|` is
// a list of exact names, anything else a regular expression found anywhere
// in the source (HookMatcher in codex-rs/hooks/src/engine/matcher.rs; Claude
// Code reads matchers the same way). A matcher that does not compile takes
// nothing.
func sessionStartMatcherTakes(matcher, source string) bool {
	if matcher == "" || matcher == "*" {
		return true
	}
	exact := true
	for _, c := range matcher {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '|' {
			exact = false
			break
		}
	}
	if exact {
		for _, name := range strings.Split(matcher, "|") {
			if name == source {
				return true
			}
		}
		return false
	}
	re, err := regexp.Compile(matcher)
	return err == nil && re.MatchString(source)
}

// commandHooks are the command handlers of groups, a Claude hooks map's
// value for one event, with timeout for a handler that names none and
// Claude's limit on each handler's context. A UserPromptSubmit group's
// matcher is ignored, and reads as taking every prompt.
func commandHooks(groups sessionStartHookGroups, event string) []sessionStartHook {
	var hooks []sessionStartHook
	for _, group := range groups {
		for _, handler := range group.Hooks {
			if handler.Type != "command" {
				continue
			}
			timeout := defaultSessionStartHookTimeout
			if handler.Timeout != nil {
				timeout = time.Duration(max(*handler.Timeout, 1)) * time.Second
			}
			matcher := group.Matcher
			if event == "UserPromptSubmit" {
				matcher = ""
			}
			hooks = append(hooks, sessionStartHook{matcher: matcher, command: handler.Command, timeout: timeout, limit: hookOutputLimit, utf16: true})
		}
	}
	return hooks
}

// sessionStartHooksFor lists the hooks whose matcher takes source.
func sessionStartHooksFor(hooks []sessionStartHook, source string) []sessionStartHook {
	var out []sessionStartHook
	for _, hook := range hooks {
		if sessionStartMatcherTakes(hook.matcher, source) {
			out = append(out, hook)
		}
	}
	return out
}

// sessionStartRun is the outcome of one hook run.
type sessionStartRun struct {
	hook           sessionStartHook
	stdout, stderr string
	// exitCode is the command's exit status, -1 when it did not exit on
	// its own: it could not start or ran out of time (err).
	exitCode  int
	err       error
	startedAt time.Time
	duration  time.Duration
}

// promptSource is the source a spec reads on the run of a session's
// UserPromptSubmit hooks (control.SessionStartRun).
const promptSource = "prompt"

// runSessionStartHook runs hook in the session's cwd for source.
func (e *engine) runSessionStartHook(hook sessionStartHook, sessionRef, source string) sessionStartRun {
	return e.hooks.run(hook, e.cwd, map[string]string{
		"session_id":      sessionRef,
		"cwd":             e.cwd,
		"hook_event_name": "SessionStart",
		"source":          source,
	})
}

// runPromptHook runs a UserPromptSubmit hook in the session's cwd for prompt.
func (e *engine) runPromptHook(hook sessionStartHook, sessionRef, prompt string) sessionStartRun {
	return e.hooks.run(hook, e.cwd, map[string]string{
		"session_id":      sessionRef,
		"cwd":             e.cwd,
		"hook_event_name": "UserPromptSubmit",
		"prompt":          prompt,
	})
}

const (
	// hookDrainLinger bounds reading a hook's output once its shell exited:
	// only a process it left behind keeps the pipes open that long, and the
	// sweep after the run stops it.
	hookDrainLinger = time.Second
	// hookStopWait bounds how long terminate waits for the hook runs it
	// stopped.
	hookStopWait = 5 * time.Second
)

// hookRuns owns the hook commands in flight. Each leads its own process
// group (procutil.ConfigureGroup), so its timeout stops everything the hook
// started, and the stop the app sends this process does not reach it:
// terminate stops them before the process exits. Zero value ready.
type hookRuns struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	wg     sync.WaitGroup
}

// begin admits one run: its context ends at stop. ok is false once stop has
// begun.
func (h *hookRuns) begin() (ctx context.Context, done func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, false
	}
	if h.ctx == nil {
		h.ctx, h.cancel = context.WithCancel(context.Background())
	}
	h.wg.Add(1)
	return h.ctx, h.wg.Done, true
}

// stop cancels every run, admits no more and waits a bounded time for them
// to end.
func (h *hookRuns) stop() {
	h.mu.Lock()
	h.closed = true
	if h.cancel != nil {
		h.cancel()
	}
	h.mu.Unlock()
	ended := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(hookStopWait):
		log.Printf("hook runs did not stop within %s", hookStopWait)
	}
}

// errHooksStopped is a run asked for after the process began to exit.
var errHooksStopped = errors.New("the mock provider is exiting")

// run runs hook in cwd with fields as its input, bounded by the hook's
// timeout and the process's own exit. What the hook left running when its
// shell exited is stopped with it.
func (h *hookRuns) run(hook sessionStartHook, cwd string, fields map[string]string) sessionStartRun {
	run := sessionStartRun{hook: hook, startedAt: time.Now(), exitCode: -1}
	input, err := json.Marshal(fields)
	if err != nil {
		run.err = err
		return run
	}
	life, done, ok := h.begin()
	if !ok {
		run.err = errHooksStopped
		return run
	}
	defer done()
	ctx, cancel := context.WithTimeout(life, hook.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", hook.command)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(input)
	procutil.ConfigureGroup(cmd)
	var stdout, stderr bytes.Buffer
	err = procutil.RunDrained(ctx, cmd, &stdout, &stderr, hookDrainLinger)
	if cmd.Process != nil {
		if sweepErr := procutil.KillConfiguredGroup(cmd); sweepErr != nil && !errors.Is(sweepErr, os.ErrProcessDone) {
			log.Printf("stop what hook %q left running: %v", hook.command, sweepErr)
		}
	}
	run.duration = time.Since(run.startedAt)
	run.stdout, run.stderr = stdout.String(), stderr.String()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		run.err = ctx.Err()
	case err == nil:
		run.exitCode = 0
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		run.exitCode = exit.ExitCode()
	default:
		run.err = err
	}
	return run
}

// context is the text the run delivers to the model; ok is false for a run
// that failed, printed nothing, or printed a JSON object that is not
// SessionStart output.
func (r sessionStartRun) context() (text string, ok bool) {
	if r.err != nil || r.exitCode != 0 {
		return "", false
	}
	trimmed := strings.TrimSpace(r.stdout)
	if trimmed == "" {
		return "", false
	}
	if !strings.HasPrefix(trimmed, "{") {
		return trimmed, true
	}
	var output struct {
		HookSpecificOutput struct {
			AdditionalContext *string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(trimmed), &output) != nil || output.HookSpecificOutput.AdditionalContext == nil {
		return "", false
	}
	return *output.HookSpecificOutput.AdditionalContext, true
}

// reportSessionStartRuns posts the contexts runs delivered for source,
// promptSource for a prompt's UserPromptSubmit runs, each as the model
// receives it (modelContext), before the adapter writes the frames of their
// outcome.
func (e *engine) reportSessionStartRuns(sessionRef, source string, runs []sessionStartRun) {
	contexts := []string{}
	for _, run := range runs {
		if text, ok := run.context(); ok {
			contexts = append(contexts, run.hook.modelContext(text))
		}
	}
	e.rep.report(control.Report{
		Kind:            control.ReportSessionStartContext,
		SessionRef:      sessionRef,
		SessionStartRun: &control.SessionStartRun{Source: source, Contexts: contexts},
	})
}

// scenarioFrameActor is an adapter that acts around a frame a scenario
// writes, as its CLI acts around the event the frame reports. It runs on the
// goroutine writing the frame, outside the writer's lock.
type scenarioFrameActor interface {
	beforeScenarioFrame(vars scenario.Vars, line string)
	afterScenarioFrame(vars scenario.Vars, line string)
}

// writeScenarioLines writes scenario frames, each line on its own write
// unless coalesce asks for one, with whatever the adapter writes around them.
func (e *engine) writeScenarioLines(vars scenario.Vars, lines []string, coalesce bool, chunkBytes, chunkIntervalMs, delayBetweenMs int) {
	actor, _ := e.adapter.(scenarioFrameActor)
	if coalesce {
		if actor != nil {
			for _, line := range lines {
				actor.beforeScenarioFrame(vars, line)
			}
		}
		e.w.writeLines(lines)
		if actor != nil {
			for _, line := range lines {
				actor.afterScenarioFrame(vars, line)
			}
		}
		return
	}
	for i, line := range lines {
		if i > 0 && delayBetweenMs > 0 {
			sleepMs(delayBetweenMs)
		}
		if actor != nil {
			actor.beforeScenarioFrame(vars, line)
		}
		e.w.writeLine(line, chunkBytes, chunkIntervalMs)
		if actor != nil {
			actor.afterScenarioFrame(vars, line)
		}
	}
}
