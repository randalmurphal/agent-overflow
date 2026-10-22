package main

import (
	"encoding/json"
	"log"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"

	"agent-overflow/internal/harness/scenario"
)

// claudeSessionStartHooks reads the SessionStart and the UserPromptSubmit
// hooks of every --settings value, inline JSON or the path of a JSON file, in
// argv order. A value that does not read is logged and adds none.
func claudeSessionStartHooks(args []string) (hooks, prompt []sessionStartHook) {
	for _, value := range flagValues(args, "--settings") {
		data := []byte(value)
		if !strings.HasPrefix(strings.TrimSpace(value), "{") {
			read, err := os.ReadFile(value)
			if err != nil {
				log.Printf("claude: read --settings: %v (its hooks are ignored)", err)
				continue
			}
			data = read
		}
		var settings struct {
			Hooks struct {
				SessionStart     sessionStartHookGroups `json:"SessionStart"`
				UserPromptSubmit sessionStartHookGroups `json:"UserPromptSubmit"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			log.Printf("claude: decode --settings: %v (its hooks are ignored)", err)
			continue
		}
		hooks = append(hooks, commandHooks(settings.Hooks.SessionStart, "SessionStart")...)
		prompt = append(prompt, commandHooks(settings.Hooks.UserPromptSubmit, "UserPromptSubmit")...)
	}
	return hooks, prompt
}

// claudeStartSource is the SessionStart source of a process's start:
// "fork" for one launched with --resume and --fork-session, "resume" for one
// launched with --resume alone, "startup" otherwise.
func claudeStartSource(args []string) string {
	switch {
	case flagValue(args, "--resume") == "":
		return "startup"
	case slices.Contains(args, "--fork-session"):
		return "fork"
	default:
		return "resume"
	}
}

// runSessionStartHooks runs the hooks whose matcher takes source and writes
// the frames Claude Code reports them with: every hook's
// `system/hook_started`, then each `system/hook_response` in order
// (captured from 2.1.283, docs/references/claude-wire.md).
func (a *claudeAdapter) runSessionStartHooks(vars scenario.Vars, source string) {
	hooks := sessionStartHooksFor(a.sessionStart, source)
	if len(hooks) == 0 {
		return
	}
	sessionID := vars["SESSION_ID"]
	runs := make([]sessionStartRun, len(hooks))
	a.writeHookRuns("SessionStart", "SessionStart:"+source, sessionID, runs, func() {
		for i, hook := range hooks {
			runs[i] = runSessionStartHook(hook, sessionID, a.e.cwd, source)
		}
		a.e.reportSessionStartRuns(sessionID, source, runs)
	})
}

// runPromptHooks runs the UserPromptSubmit hooks for prompt, as Claude Code
// does for every prompt it takes, a queued one included, before the model
// sees it, with the same frames.
func (a *claudeAdapter) runPromptHooks(vars scenario.Vars, prompt string) {
	if len(a.promptHooks) == 0 {
		return
	}
	sessionID := vars["SESSION_ID"]
	runs := make([]sessionStartRun, len(a.promptHooks))
	a.writeHookRuns("UserPromptSubmit", "UserPromptSubmit", sessionID, runs, func() {
		for i, hook := range a.promptHooks {
			runs[i] = runPromptHook(hook, sessionID, a.e.cwd, prompt)
		}
		a.e.reportSessionStartRuns(sessionID, promptSource, runs)
	})
}

// writeHookRuns writes a hook_started frame per run, runs them (run fills
// runs), then writes each run's hook_response.
func (a *claudeAdapter) writeHookRuns(event, name, sessionID string, runs []sessionStartRun, run func()) {
	ids := make([]string, len(runs))
	for i := range runs {
		ids[i] = uuid.NewString()
		a.w.writeLine(mustJSON(map[string]any{
			"type":       "system",
			"subtype":    "hook_started",
			"hook_id":    ids[i],
			"hook_name":  name,
			"hook_event": event,
			"uuid":       uuid.NewString(),
			"session_id": sessionID,
		}), 0, 0)
	}
	run()
	for i, run := range runs {
		frame := map[string]any{
			"type":       "system",
			"subtype":    "hook_response",
			"hook_id":    ids[i],
			"hook_name":  name,
			"hook_event": event,
			"output":     run.stdout + run.stderr,
			"stdout":     run.stdout,
			"stderr":     run.stderr,
			"outcome":    "success",
			"uuid":       uuid.NewString(),
			"session_id": sessionID,
		}
		if run.err == nil {
			frame["exit_code"] = run.exitCode
		}
		if run.err != nil || run.exitCode != 0 {
			frame["outcome"] = "error"
		}
		a.w.writeLine(mustJSON(frame), 0, 0)
	}
}

// beforeScenarioFrame runs a compaction's SessionStart hooks before the
// `system/compact_boundary` a scenario writes for it: Claude Code runs them
// inside the compaction, ahead of the boundary.
func (a *claudeAdapter) beforeScenarioFrame(vars scenario.Vars, line string) {
	if !strings.Contains(line, `"compact_boundary"`) {
		return
	}
	var frame struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal([]byte(line), &frame) != nil || frame.Type != "system" || frame.Subtype != "compact_boundary" {
		return
	}
	a.runSessionStartHooks(vars, "compact")
}

func (a *claudeAdapter) afterScenarioFrame(scenario.Vars, string) {}
