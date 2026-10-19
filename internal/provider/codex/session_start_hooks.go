package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"agent-overflow/internal/provider"
)

// A thread's SessionStart hooks (Config.SessionStartHooks) ride the
// thread/start|resume `config` map, which Codex reads as the thread's
// session-flags config layer. Codex runs a hook from a non-managed layer
// only when the config's `hooks.state` holds its trust hash, so the map
// carries the hash of each hook it registers: the trust covers exactly
// these commands and nothing else the user configured
// (docs/references/codex-wire.md, SessionStart hooks). The runs are
// followed by observeSessionStartHooks.
//
// Codex keeps the hooks a start or a compaction queued only in the loaded
// thread, so a cold load (every resume AO sends, and a rehydrate) drops
// them. A delivery still expected is recorded across sessions
// (SessionStartPending), and the load that follows registers the hooks for
// `resume` too, once, so the next turn runs them.

// sessionFlagsHookKeySource is the key source Codex gives the session-flags
// layer's hooks (codex-rs/hooks/src/engine/discovery.rs, 0.153 to 0.162).
const sessionFlagsHookKeySource = "/<session-flags>/config.toml"

// sessionStartHookSourcesOwed adds a resume to the sources the hooks run
// for, on a load while a delivery is still owed: the load dropped the hooks
// Codex had queued for it.
const sessionStartHookSourcesOwed = provider.SessionStartHookSources + "|resume"

// SessionStartPending is the record of the SessionStart delivery a thread's
// Codex thread is expected to make, kept across its sessions. A Config
// without one (a throwaway session that only forks or reverts) records
// nothing.
type SessionStartPending struct {
	// After is the expectation recorded for the Codex thread being resumed,
	// "" when none is.
	After provider.SessionStartEvent
	// Record persists the expectation Codex thread codexThreadID holds:
	// after when one opens, "" when it settles.
	Record func(codexThreadID string, after provider.SessionStartEvent) error
}

// sessionStartHandler is one hook's command handler: what the thread
// config registers and what its trust hash covers.
type sessionStartHandler struct {
	command string
	timeout int
}

func newSessionStartHandler(hook provider.SessionStartHook) sessionStartHandler {
	return sessionStartHandler{command: hook.Command, timeout: provider.SessionStartHookTimeoutSeconds}
}

func (h sessionStartHandler) config() map[string]any {
	return map[string]any{"type": "command", "command": h.command, "timeout": h.timeout}
}

// trustHash is the hash Codex trusts the handler by in a group of matcher:
// sha256 of the canonical JSON of the handler's normalized identity, keys
// sorted and strings escaped as serde_json escapes them (hook_hash in
// codex-rs/hooks/src/engine/discovery.rs and version_for_toml in
// codex-rs/config/src/fingerprint.rs).
func (h sessionStartHandler) trustHash(matcher string) string {
	var identity strings.Builder
	identity.WriteString(`{"event_name":"session_start","hooks":[{"async":false,"command":`)
	writeSerdeJSONString(&identity, h.command)
	identity.WriteString(`,"timeout":`)
	identity.WriteString(strconv.Itoa(h.timeout))
	identity.WriteString(`,"type":"command"}],"matcher":`)
	writeSerdeJSONString(&identity, matcher)
	identity.WriteString(`}`)
	sum := sha256.Sum256([]byte(identity.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// sessionStartHookConfig is the `hooks` value of the thread config: one
// SessionStart matcher group with a command handler per hook, and the
// trust state of each; owed adds `resume` to the group's sources. nil for
// no hooks.
func sessionStartHookConfig(hooks []provider.SessionStartHook, owed bool) map[string]any {
	if len(hooks) == 0 {
		return nil
	}
	matcher := provider.SessionStartHookSources
	if owed {
		matcher = sessionStartHookSourcesOwed
	}
	handlers := make([]any, 0, len(hooks))
	state := make(map[string]any, len(hooks))
	for i, hook := range hooks {
		handler := newSessionStartHandler(hook)
		handlers = append(handlers, handler.config())
		key := sessionFlagsHookKeySource + ":session_start:0:" + strconv.Itoa(i)
		state[key] = map[string]any{"trusted_hash": handler.trustHash(matcher)}
	}
	return map[string]any{
		"SessionStart": []any{map[string]any{"matcher": matcher, "hooks": handlers}},
		"state":        state,
	}
}

// withSessionStartHooks is a copy of a thread's params whose `config`
// registers hooks for a load that owes a delivery or not.
func withSessionStartHooks(params map[string]any, hooks []provider.SessionStartHook, owed bool) map[string]any {
	if len(hooks) == 0 {
		return params
	}
	config, _ := params["config"].(map[string]any)
	config = maps.Clone(config)
	if config == nil {
		config = map[string]any{}
	}
	config["hooks"] = sessionStartHookConfig(hooks, owed)
	params = maps.Clone(params)
	params["config"] = config
	return params
}

// writeSerdeJSONString writes text as serde_json writes a string: `"` and
// `\` escaped, the control characters below U+0020 as their short escapes or
// \u00xx in lower-case hex, everything else as it is. encoding/json differs
// (it escapes <, >, &, U+2028 and U+2029), and a different byte is a
// different hash.
func writeSerdeJSONString(b *strings.Builder, text string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[c>>4])
				b.WriteByte(hexDigits[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}

// observeSessionStartHooks follows the delivery of the thread's
// SessionStart hooks through a root notification, before it is classified,
// and reports whether the notification is one of their runs, which renders
// no row.
//
// Codex runs the hooks a start or a compaction queued before the first model
// request of the next turn, or right after a compaction inside a turn
// (run_pending_session_start_hooks in codex-rs/core/src/session/turn.rs).
// Only a regular turn runs them: a compaction requested on its own and a
// review are turns of their own that run none. So the hooks had their
// chance once a turn that started after the expectation, or that ran them
// after it, completes; the warning for hooks that did not deliver is
// emitted then, before the turn's own events. An interrupted or failed turn
// may have stopped before the hooks ran, and Codex keeps them queued, so
// the expectation waits for the next turn.
func (s *Session) observeSessionStartHooks(method string, params json.RawMessage) bool {
	if s.sessionStart == nil {
		return false
	}
	switch method {
	case "hook/started", "hook/completed":
		// Every hook event reports here; only a SessionStart run is decoded.
		if !bytes.Contains(params, []byte(`"sessionStart"`)) {
			return false
		}
		var wire struct {
			TurnID string `json:"turnId"`
			Run    struct {
				EventName string `json:"eventName"`
				Source    string `json:"source"`
				Status    string `json:"status"`
				Entries   []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"entries"`
			} `json:"run"`
		}
		if err := json.Unmarshal(params, &wire); err != nil || wire.Run.EventName != "sessionStart" || wire.Run.Source != "sessionFlags" {
			return false
		}
		if method == "hook/started" {
			if wire.TurnID != "" && s.sessionStart.Expecting() != "" {
				s.setSessionStartDueTurn(wire.TurnID)
			}
			return true
		}
		var failures []string
		for _, entry := range wire.Run.Entries {
			switch entry.Kind {
			case "context":
				if wire.Run.Status == "completed" {
					s.sessionStart.Deliver(entry.Text)
				}
			case "error":
				failures = append(failures, entry.Text)
			}
		}
		if wire.Run.Status != "completed" {
			if len(failures) == 0 {
				failures = append(failures, "status "+wire.Run.Status)
			}
			s.sessionStart.Fail(strings.Join(failures, "; "))
		}
		return true
	case "thread/compacted":
		s.expectSessionStartAfterCompaction()
	case "item/completed":
		if bytes.Contains(params, []byte(`"contextCompaction"`)) && classifyCodexItemType(params) == "contextCompaction" {
			s.expectSessionStartAfterCompaction()
		}
	case "turn/started":
		// A review's turns never get here: its projection consumes them,
		// and Codex runs no hooks for a review.
		turnID := readNestedString(params, "turn", "id")
		if s.sessionStart.Expecting() == "" {
			turnID = ""
		}
		s.setSessionStartDueTurn(turnID)
	case "turn/completed":
		turnID := readNestedString(params, "turn", "id")
		s.mu.Lock()
		due := turnID != "" && s.sessionStartDueTurn == turnID
		if due {
			s.sessionStartDueTurn = ""
		}
		s.mu.Unlock()
		if due && readNestedString(params, "turn", "status") == "completed" {
			if warning := s.sessionStart.Settle(); warning != "" {
				s.emitNonFatalError(warning)
			}
			s.recordSessionStart("")
		}
	}
	return false
}

// expectSessionStart opens an expectation after the thread's start and
// records it for the thread's next session.
func (s *Session) expectSessionStart() {
	s.sessionStart.Expect(provider.SessionStartAfterStart)
	s.recordSessionStart(provider.SessionStartAfterStart)
}

// expectSessionStartAfterCompaction arms the hooks for a compaction. Codex
// reports a compaction before it runs the compaction's hooks, so a delivery
// seen earlier came from before it and no longer counts, and the hooks have
// not had their chance in this turn yet.
func (s *Session) expectSessionStartAfterCompaction() {
	s.sessionStart.ExpectAfresh(provider.SessionStartAfterCompaction)
	s.setSessionStartDueTurn("")
	s.recordSessionStart(provider.SessionStartAfterCompaction)
}

// setSessionStartDueTurn records the turn that gives the hooks of the
// current expectation their chance to run, "" for none.
func (s *Session) setSessionStartDueTurn(turnID string) {
	s.mu.Lock()
	s.sessionStartDueTurn = turnID
	s.mu.Unlock()
}

// recordSessionStart persists the thread's open expectation, "" once it
// settled. A failed write is put on the thread: the next session would
// resume without the hooks it owes, or run them once more.
func (s *Session) recordSessionStart(after provider.SessionStartEvent) {
	if s.sessionStartRecord == nil {
		return
	}
	if err := s.sessionStartRecord(s.rootThreadID(), after); err != nil {
		s.emitNonFatalError(fmt.Sprintf("Agent Overflow could not record whether this thread's SessionStart hooks still owe their context, so its next session may start without it or with a second copy: %v", err))
	}
}

// sessionStartOwed reports whether a load of the thread now owes the
// hooks' delivery: one is still expected, which the load would drop.
func (s *Session) sessionStartOwed() bool {
	return s.sessionStart.Expecting() != ""
}
