package claude

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// A session's SessionStart hook (Config.SessionStartHook) runs at its spawn
// when it starts fresh, at its spawn when it resumes and the hook's Resume
// is set, and inside a compaction, each run reporting a
// `system/hook_response` frame (docs/references/claude-wire.md, SessionStart
// hooks). The parser counts the hook delivered when a successful response's
// context opens with the hook's heading. Print mode awaits a spawn's hooks
// before the first `system/init`, and a compaction's hooks run before its
// `system/compact_boundary`, so a start or a resume settles at init and a
// compaction at its own boundary: a context that did not arrive becomes a
// non-fatal error row, so the user learns the session runs without it. The
// context that did arrive is kept as the session's receipt
// (Config.SessionStartReceipts) at the turn's `result`: print mode emits
// its first init before it reads any input, and writes a resume's hook
// context to the transcript only with the first turn.

// NewSessionStartDelivery follows hook for a session launched with resume,
// nil without one. A fresh session runs the hook at its spawn, so it expects
// its output from the start; a resumed or forked one runs it when its
// Resume is set. Shared with claude-tui, which launches the same binary.
func NewSessionStartDelivery(hook *provider.SessionStartHook, receipts *provider.SessionStartReceipts, resume string) (*provider.SessionStartDelivery, error) {
	delivery, err := provider.NewSessionStartDelivery(hook, receipts)
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
// failure is quoted only when the session's own hook did not deliver.
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
// EventInit or a compaction's EventCompactBoundary, holding its receipt
// under sessionRef, the session the frame names, for keepSessionStart.
func (p *Parser) settleSessionStart(threadID, sessionRef string, now time.Time) []provider.ProviderEvent {
	if p == nil {
		return nil
	}
	return sessionStartWarnings(threadID, now, p.sessionStart.Settle(sessionRef))
}

// keepSessionStart keeps the receipt the delivery holds, at a turn's
// result, by which the transcript holds what it names.
func (p *Parser) keepSessionStart(threadID string, now time.Time) []provider.ProviderEvent {
	if p == nil {
		return nil
	}
	return sessionStartWarnings(threadID, now, p.sessionStart.Keep())
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
