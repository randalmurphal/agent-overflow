package claude

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// A session's SessionStart hook (Config.SessionStartHook) runs at its spawn
// when it starts fresh and inside a compaction, each run reporting a
// `system/hook_response` frame (docs/references/claude-wire.md, SessionStart
// hooks). The parser counts the hook delivered when a successful response's
// context opens with the hook's heading. Print mode awaits a spawn's hooks
// before the first `system/init`, and a compaction's hooks run before its
// `system/compact_boundary`, so a start settles at init and a compaction at
// its own boundary: a context that did not arrive becomes a non-fatal error
// row, so the user learns the session runs without it. A resumed session
// runs no SessionStart hook: its history holds what earlier runs delivered,
// and its prompts' runs of the hook's PromptCommand deliver what changed.

// NewSessionStartDelivery follows hook for a session launched with resume,
// nil without one. A fresh session runs the hook at its spawn, so it expects
// its output from the start; a resumed or forked one expects it only after a
// compaction. forget runs when an expected output did not arrive. Shared
// with claude-tui, which launches the same binary.
func NewSessionStartDelivery(hook *provider.SessionStartHook, forget func() error, resume string) (*provider.SessionStartDelivery, error) {
	delivery, err := provider.NewSessionStartDelivery(hook, forget)
	if err != nil {
		return nil, err
	}
	if resume == "" {
		delivery.Expect(provider.SessionStartAfterStart)
	}
	return delivery, nil
}

// observeHookResponse records the context the SessionStart hook delivered,
// or why a run failed, and returns the warning for a failed run of the
// hook's PromptCommand. A failed SessionStart run names no command, so the
// failure is quoted only when the session's own hook did not deliver; a
// failed UserPromptSubmit run is the hook's when its error output opens with
// the hook's Failure prefix.
func (p *Parser) observeHookResponse(threadID string, raw map[string]json.RawMessage, now time.Time) []provider.ProviderEvent {
	if p == nil || p.sessionStart == nil {
		return nil
	}
	var exitCode int
	hasExitCode := json.Unmarshal(raw["exit_code"], &exitCode) == nil
	failed := !hasExitCode || exitCode != 0
	switch readRawString(raw["hook_event"]) {
	case "SessionStart":
	case "UserPromptSubmit":
		hook := p.sessionStart.Hook()
		stderr := strings.TrimSpace(readRawString(raw["stderr"]))
		if !failed || hook.Failure == "" || !strings.HasPrefix(stderr, hook.Failure) {
			return nil
		}
		return sessionStartWarnings(threadID, now, provider.PromptHookFailure(*hook, hookResponseFailure(raw, hasExitCode, exitCode)))
	default:
		return nil
	}
	if failed {
		p.sessionStart.Fail(hookResponseFailure(raw, hasExitCode, exitCode))
		return nil
	}
	stdout := strings.TrimSpace(readRawString(raw["stdout"]))
	context := stdout
	if strings.HasPrefix(stdout, "{") {
		var output struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(stdout), &output); err != nil {
			return nil
		}
		context = output.HookSpecificOutput.AdditionalContext
	}
	p.sessionStart.Deliver(context)
	return nil
}

// hookResponseFailure describes a failed run: its exit code, or its outcome
// when it has none (a run the CLI cancelled), then its error output.
func hookResponseFailure(raw map[string]json.RawMessage, hasExitCode bool, exitCode int) string {
	detail := "no exit code"
	if hasExitCode {
		detail = "exit code " + strconv.Itoa(exitCode)
	} else if outcome := readRawString(raw["outcome"]); outcome != "" {
		detail = "outcome " + outcome
	}
	if stderr := strings.TrimSpace(readRawString(raw["stderr"])); stderr != "" {
		detail += ": " + stderr
	}
	return detail
}

// settleSessionStart settles the delivery after a start's EventInit or a
// compaction's EventCompactBoundary.
func (p *Parser) settleSessionStart(threadID string, now time.Time) []provider.ProviderEvent {
	if p == nil {
		return nil
	}
	return sessionStartWarnings(threadID, now, p.sessionStart.Settle())
}

// sessionStartWarnings are non-fatal error rows for the warnings that are
// not "".
func sessionStartWarnings(threadID string, now time.Time, warnings ...string) []provider.ProviderEvent {
	var events []provider.ProviderEvent
	for _, warning := range warnings {
		if warning == "" {
			continue
		}
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   warning,
			Meta:      json.RawMessage(`{"fatal":false}`),
			Timestamp: now,
		})
	}
	return events
}
