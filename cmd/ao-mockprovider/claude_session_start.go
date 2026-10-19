package main

import (
	"encoding/json"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/harness/scenario"
)

// claudeSessionStartHooks reads the SessionStart hooks of every --settings
// value, inline JSON or the path of a JSON file, in argv order. A value that
// does not read is logged and adds none.
func claudeSessionStartHooks(args []string) []sessionStartHook {
	var hooks []sessionStartHook
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
				SessionStart sessionStartHookGroups `json:"SessionStart"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			log.Printf("claude: decode --settings: %v (its hooks are ignored)", err)
			continue
		}
		for _, group := range settings.Hooks.SessionStart {
			for _, handler := range group.Hooks {
				if handler.Type != "command" {
					continue
				}
				timeout := defaultSessionStartHookTimeout
				if handler.Timeout != nil {
					timeout = time.Duration(max(*handler.Timeout, 1)) * time.Second
				}
				hooks = append(hooks, sessionStartHook{matcher: group.Matcher, command: handler.Command, timeout: timeout})
			}
		}
	}
	return hooks
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
	name := "SessionStart:" + source
	ids := make([]string, len(hooks))
	for i := range hooks {
		ids[i] = uuid.NewString()
		a.w.writeLine(mustJSON(map[string]any{
			"type":       "system",
			"subtype":    "hook_started",
			"hook_id":    ids[i],
			"hook_name":  name,
			"hook_event": "SessionStart",
			"uuid":       uuid.NewString(),
			"session_id": sessionID,
		}), 0, 0)
	}
	runs := make([]sessionStartRun, len(hooks))
	for i, hook := range hooks {
		runs[i] = runSessionStartHook(hook, sessionID, a.e.cwd, source)
	}
	a.e.reportSessionStartRuns(sessionID, source, runs)
	for i, run := range runs {
		frame := map[string]any{
			"type":       "system",
			"subtype":    "hook_response",
			"hook_id":    ids[i],
			"hook_name":  name,
			"hook_event": "SessionStart",
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
