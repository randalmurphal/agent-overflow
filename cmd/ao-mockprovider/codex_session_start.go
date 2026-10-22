package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/harness/scenario"
)

// codexSessionFlagsSource is the source path Codex gives the hooks of a
// thread's `config`, its session-flags layer, in trust keys and run
// summaries (codex-rs/hooks/src/engine/discovery.rs, rust-v0.162.1).
const codexSessionFlagsSource = "/<session-flags>/config.toml"

// codexSessionStartHooks reads the SessionStart and the UserPromptSubmit
// hooks of a thread/start or thread/resume `config.hooks` that Codex would
// run. A hook of the session-flags layer runs only when `hooks.state` holds,
// under its key, the sha256 of its normalized identity; any other is
// untrusted and skipped (hook_hash and hook_trust_status in discovery.rs).
func codexSessionStartHooks(params json.RawMessage) (hooks, prompt []sessionStartHook) {
	var config struct {
		Hooks struct {
			SessionStart     sessionStartHookGroups `json:"SessionStart"`
			UserPromptSubmit sessionStartHookGroups `json:"UserPromptSubmit"`
			State            map[string]struct {
				TrustedHash string `json:"trusted_hash"`
			} `json:"state"`
		} `json:"hooks"`
	}
	if raw := readParamRaw(params, "config"); len(raw) > 0 {
		if err := json.Unmarshal(raw, &config); err != nil {
			log.Printf("codex: decode config.hooks: %v (its hooks are ignored)", err)
			return nil, nil
		}
	}
	trusted := func(groups sessionStartHookGroups, event, label string) []sessionStartHook {
		var out []sessionStartHook
		order := 0
		for g, group := range groups {
			for h, handler := range group.Hooks {
				if handler.Type != "command" {
					continue
				}
				timeout := int(defaultSessionStartHookTimeout / time.Second)
				if handler.Timeout != nil {
					timeout = max(*handler.Timeout, 1)
				}
				// UserPromptSubmit reads no matcher, and its identity has none.
				matcher := group.Matcher
				if event == "user_prompt_submit" {
					matcher = ""
				}
				key := fmt.Sprintf("%s:%s:%d:%d", codexSessionFlagsSource, event, g, h)
				if config.Hooks.State[key].TrustedHash != codexHookHash(event, matcher, handler.Command, timeout) {
					log.Printf("codex: %s hook %s is not trusted (it does not run)", label, key)
					continue
				}
				out = append(out, sessionStartHook{matcher: matcher, command: handler.Command, timeout: time.Duration(timeout) * time.Second, order: order})
				order++
			}
		}
		return out
	}
	return trusted(config.Hooks.SessionStart, "session_start", "SessionStart"), trusted(config.Hooks.UserPromptSubmit, "user_prompt_submit", "UserPromptSubmit")
}

// codexHookHash is the trust hash of a command handler of event, the event's
// key label, in a group of matcher, "" for none: sha256 of the canonical
// JSON of its normalized identity, which leaves out a matcher the event does
// not read. encoding/json spells the identity as serde_json does except for
// U+2028 and U+2029, which it escapes and no harness path holds.
func codexHookHash(event, matcher, command string, timeout int) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	identity := map[string]any{
		"event_name": event,
		"hooks":      []any{map[string]any{"type": "command", "command": command, "timeout": timeout, "async": false}},
	}
	if matcher != "" {
		identity["matcher"] = matcher
	}
	if err := encoder.Encode(identity); err != nil {
		log.Fatalf("codex: encode a hook identity: %v", err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// noteSessionStartHooks takes the hooks of a thread/start or thread/resume
// and queues their run for source, which Codex runs at the next turn start.
func (a *codexAdapter) noteSessionStartHooks(params json.RawMessage, source string) {
	var hooks, prompt []sessionStartHook
	if !a.e.sc.SessionStartHooksOff {
		hooks, prompt = codexSessionStartHooks(params)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionStart, a.promptHooks = hooks, prompt
	a.pendingSessionStart = source
}

func (a *codexAdapter) beforeScenarioFrame(scenario.Vars, string) {}

// afterScenarioFrame runs the queued SessionStart hooks where Codex runs
// them: when the thread's turn starts, before its first model request, and
// right after a compaction inside a turn, which queues the "compact" run
// (run_pending_session_start_hooks in codex-rs/core/src/session/turn.rs).
// A compaction is the contextCompaction item, or thread/compacted from a
// version older than the item; a scenario writes one of them per compaction.
func (a *codexAdapter) afterScenarioFrame(vars scenario.Vars, line string) {
	if !strings.Contains(line, `"turn/started"`) && !strings.Contains(line, `"thread/compacted"`) && !strings.Contains(line, `"contextCompaction"`) {
		return
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			Item     struct {
				Type string `json:"type"`
			} `json:"item"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &frame) != nil || frame.Params.ThreadID != vars["THREAD_ID"] {
		return
	}
	switch {
	case frame.Method == "turn/started":
		// The turn's input runs the UserPromptSubmit hooks after the
		// pending SessionStart ones (turn.rs).
		a.runPendingSessionStart(vars)
		a.runPromptHooks(vars, vars["USER_INPUT"])
		return
	case frame.Method == "thread/compacted", frame.Method == "item/completed" && frame.Params.Item.Type == "contextCompaction":
		a.mu.Lock()
		a.pendingSessionStart = "compact"
		a.mu.Unlock()
	default:
		return
	}
	a.runPendingSessionStart(vars)
}

// runPromptHooks runs the trusted UserPromptSubmit hooks for prompt, as
// Codex does for a turn's input and for a steer it drains, with the
// notifications it reports them with.
func (a *codexAdapter) runPromptHooks(vars scenario.Vars, prompt string) {
	a.mu.Lock()
	hooks := a.promptHooks
	a.mu.Unlock()
	if len(hooks) == 0 {
		return
	}
	threadID, turnID := vars["THREAD_ID"], vars["TURN_ID"]
	started := time.Now()
	for _, hook := range hooks {
		a.writeHookNotification("hook/started", threadID, turnID, codexHookRun("userPromptSubmit", hook, started, nil))
	}
	runs := make([]sessionStartRun, len(hooks))
	for i, hook := range hooks {
		runs[i] = runPromptHook(hook, threadID, a.e.cwd, prompt)
	}
	a.e.reportSessionStartRuns(threadID, promptSource, runs)
	for i, hook := range hooks {
		a.writeHookNotification("hook/completed", threadID, turnID, codexHookRun("userPromptSubmit", hook, started, &runs[i]))
	}
}

// runPendingSessionStart runs the queued source's hooks and writes the
// notifications Codex reports them with: every run's hook/started, then each
// hook/completed in order.
func (a *codexAdapter) runPendingSessionStart(vars scenario.Vars) {
	a.mu.Lock()
	source := a.pendingSessionStart
	a.pendingSessionStart = ""
	hooks := sessionStartHooksFor(a.sessionStart, source)
	a.mu.Unlock()
	if len(hooks) == 0 {
		return
	}
	threadID, turnID := vars["THREAD_ID"], vars["TURN_ID"]
	started := time.Now()
	for _, hook := range hooks {
		a.writeHookNotification("hook/started", threadID, turnID, codexHookRun("sessionStart", hook, started, nil))
	}
	runs := make([]sessionStartRun, len(hooks))
	for i, hook := range hooks {
		runs[i] = runSessionStartHook(hook, threadID, a.e.cwd, source)
	}
	a.e.reportSessionStartRuns(threadID, source, runs)
	for i, hook := range hooks {
		a.writeHookNotification("hook/completed", threadID, turnID, codexHookRun("sessionStart", hook, started, &runs[i]))
	}
}

func (a *codexAdapter) writeHookNotification(method, threadID, turnID string, run map[string]any) {
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  map[string]any{"threadId": threadID, "turnId": turnID, "run": run},
	}), 0, 0)
}

// codexHookRun is the HookRunSummary of hook, a hook of event: running
// while run is nil, then its outcome as parse_completed in
// codex-rs/hooks/src/events/session_start.rs and user_prompt_submit.rs reads
// codexHookEventWords spell each event as upstream's invalid-output error
// does.
var codexHookEventWords = map[string]string{"sessionStart": "session start", "userPromptSubmit": "user prompt submit"}

// it.
func codexHookRun(event string, hook sessionStartHook, started time.Time, run *sessionStartRun) map[string]any {
	summary := map[string]any{
		"id":            fmt.Sprintf("%s:%d:%s", event, hook.order, codexSessionFlagsSource),
		"eventName":     event,
		"handlerType":   "command",
		"executionMode": "sync",
		"scope":         "thread",
		"sourcePath":    codexSessionFlagsSource,
		"source":        "sessionFlags",
		"displayOrder":  hook.order,
		"status":        "running",
		"statusMessage": nil,
		"startedAt":     started.Unix(),
		"completedAt":   nil,
		"durationMs":    nil,
		"entries":       []any{},
	}
	if run == nil {
		return summary
	}
	summary["completedAt"] = started.Add(run.duration).Unix()
	summary["durationMs"] = run.duration.Milliseconds()
	status, entries := "completed", []any{}
	switch {
	case run.err != nil:
		status = "failed"
		entries = append(entries, map[string]any{"kind": "error", "text": run.err.Error()})
	case run.exitCode != 0:
		status = "failed"
		entries = append(entries, map[string]any{"kind": "error", "text": fmt.Sprintf("hook exited with code %d", run.exitCode)})
	default:
		if text, ok := run.context(); ok {
			entries = append(entries, map[string]any{"kind": "context", "text": text})
		} else if strings.HasPrefix(strings.TrimSpace(run.stdout), "{") && !json.Valid([]byte(strings.TrimSpace(run.stdout))) {
			status = "failed"
			entries = append(entries, map[string]any{"kind": "error", "text": "hook returned invalid " + codexHookEventWords[event] + " JSON output"})
		}
	}
	summary["status"], summary["entries"] = status, entries
	return summary
}
