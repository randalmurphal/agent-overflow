package claude

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// A session's SessionStart hooks (Config.SessionStartHooks) run at its spawn
// when it starts fresh, those whose Resume is set at its spawn when it
// resumes, and every hook inside a compaction, each reporting a
// `system/hook_response` frame (docs/references/claude-wire.md, SessionStart
// hooks). The parser counts a hook delivered when a successful response's
// context opens with the hook's heading. Print mode awaits a spawn's hooks
// before the first `system/init`, and a compaction's hooks run before its
// `system/compact_boundary`, so a start or a resume settles at init and a
// compaction at its own boundary: hooks whose context did not arrive become
// a non-fatal error row, so the user learns the session runs without that
// context. The context that did arrive is kept as the session's receipt
// (Config.SessionStartReceipts) at the turn's `result`: print mode emits
// its first init before it reads any input, and writes a resume's hook
// context to the transcript only with the first turn.

// NewSessionStartDelivery follows hooks for a session launched with resume,
// nil without hooks. A fresh session runs every hook at its spawn, so it
// expects their output from the start; a resumed or forked one runs those
// whose Resume is set. Shared with claude-tui, which launches the same
// binary.
func NewSessionStartDelivery(hooks []provider.SessionStartHook, receipts *provider.SessionStartReceipts, resume string) (*provider.SessionStartDelivery, error) {
	delivery, err := provider.NewSessionStartDelivery(hooks, receipts)
	if err != nil {
		return nil, err
	}
	// Only a compaction's expectation writes, and warns when it cannot.
	if resume == "" {
		delivery.Expect(provider.SessionStartAfterStart)
	} else {
		delivery.Expect(provider.SessionStartAfterResume)
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

// settleSessionStart settles the delivery after a start's or a resume's
// EventInit or a compaction's EventCompactBoundary, holding its receipts
// under sessionRef, the session the frame names, for keepSessionStart.
func (p *Parser) settleSessionStart(threadID, sessionRef string, now time.Time) []provider.ProviderEvent {
	if p == nil {
		return nil
	}
	return sessionStartWarnings(threadID, p.sessionStart.Settle(sessionRef), now)
}

// keepSessionStart keeps the receipts the delivery holds, at a turn's
// result, by which the transcript holds what they name.
func (p *Parser) keepSessionStart(threadID string, now time.Time) []provider.ProviderEvent {
	if p == nil {
		return nil
	}
	return sessionStartWarnings(threadID, p.sessionStart.Keep(), now)
}

// sessionStartWarnings are non-fatal error rows for warnings.
func sessionStartWarnings(threadID string, warnings []string, now time.Time) []provider.ProviderEvent {
	events := make([]provider.ProviderEvent, 0, len(warnings))
	for _, warning := range warnings {
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
