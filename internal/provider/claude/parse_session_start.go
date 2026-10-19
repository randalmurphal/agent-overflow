package claude

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// A session's SessionStart hooks (Config.SessionStartHooks) run at its spawn
// when it starts fresh and inside a compaction, each reporting a
// `system/hook_response` frame (docs/references/claude-wire.md, SessionStart
// hooks). The parser counts a hook delivered when a successful response's
// context opens with the hook's heading. Print mode awaits the start's hooks
// before the first `system/init`, and a compaction's hooks run before its
// `system/compact_boundary`, so a start settles at init and a compaction at
// its own boundary: hooks whose context did not arrive become a non-fatal
// error row, so the user learns the session runs without that context.

// NewSessionStartDelivery follows hooks for a session launched with resume,
// nil without hooks. A fresh session runs them at its spawn, so it expects
// their output from the start; a resumed or forked one runs none
// (provider.SessionStartHookSources). Shared with claude-tui, which launches
// the same binary.
func NewSessionStartDelivery(hooks []provider.SessionStartHook, resume string) (*provider.SessionStartDelivery, error) {
	delivery, err := provider.NewSessionStartDelivery(hooks)
	if err != nil {
		return nil, err
	}
	if resume == "" {
		delivery.Expect(provider.SessionStartAfterStart)
	}
	return delivery, nil
}

// observeSessionStartHookResponse records the context a SessionStart hook
// delivered, or why a run failed. A failed run names no command, so the
// failure is quoted only when the session's own hooks did not deliver.
func (p *Parser) observeSessionStartHookResponse(raw map[string]json.RawMessage) {
	if p == nil || p.sessionStart == nil || readRawString(raw["hook_event"]) != "SessionStart" {
		return
	}
	var exitCode int
	if err := json.Unmarshal(raw["exit_code"], &exitCode); err != nil || exitCode != 0 {
		p.sessionStart.Fail(hookResponseFailure(raw, err == nil, exitCode))
		return
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
			return
		}
		context = output.HookSpecificOutput.AdditionalContext
	}
	p.sessionStart.Deliver(context)
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

// settleSessionStart is the warning row for the hooks a start or a
// compaction did not deliver, after the start's EventInit or the
// compaction's EventCompactBoundary.
func (p *Parser) settleSessionStart(threadID string, now time.Time) []provider.ProviderEvent {
	if p == nil {
		return nil
	}
	warning := p.sessionStart.Settle()
	if warning == "" {
		return nil
	}
	return []provider.ProviderEvent{{
		Kind:      provider.EventError,
		ThreadID:  threadID,
		Content:   warning,
		Meta:      json.RawMessage(`{"fatal":false}`),
		Timestamp: now,
	}}
}
