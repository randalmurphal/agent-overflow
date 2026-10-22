package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"strconv"
	"strings"

	"agent-overflow/internal/provider"
)

// A thread's SessionStart hook (Config.SessionStartHook) rides the
// thread/start|resume `config` map, which Codex reads as the thread's
// session-flags config layer, in a matcher group of its own. Codex runs a
// hook from a non-managed layer only when the config's `hooks.state` holds
// its trust hash, so the map carries the hook's hash: the trust covers
// exactly this command and nothing else the user configured
// (docs/references/codex-wire.md, SessionStart hooks). The runs are
// followed by observeSessionStartHooks.
//
// A resume registers the hook for `resume` too when its Resume is set: the
// thread's model does not hold its current context. Codex keeps the hooks a
// load queued only in the loaded thread, so a rehydrate registers the hook
// for `resume` while the open expectation still waits for it.

// sessionFlagsHookKeySource is the key source Codex gives the session-flags
// layer's hooks (codex-rs/hooks/src/engine/discovery.rs, 0.153 to 0.162).
const sessionFlagsHookKeySource = "/<session-flags>/config.toml"

// sessionStartHandler is the hook's command handler: what the thread config
// registers and what its trust hash covers.
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

// sessionStartHookKey is the key Codex gives the one SessionStart group's
// one handler in the session-flags layer, which the trust state is kept
// under.
const sessionStartHookKey = sessionFlagsHookKeySource + ":session_start:0:0"

// sessionStartHookConfig is the `hooks` value of the thread config: one
// SessionStart matcher group holding the hook's command handler, with the
// resume sources when resume is set, and the handler's trust state. nil
// without a hook.
func sessionStartHookConfig(hook *provider.SessionStartHook, resume bool) map[string]any {
	if hook == nil {
		return nil
	}
	matcher := provider.SessionStartMatcher(resume)
	handler := newSessionStartHandler(*hook)
	return map[string]any{
		"SessionStart": []any{map[string]any{"matcher": matcher, "hooks": []any{handler.config()}}},
		"state":        map[string]any{sessionStartHookKey: map[string]any{"trusted_hash": handler.trustHash(matcher)}},
	}
}

// withSessionStartHook is a copy of a thread's params whose `config`
// registers hook, for its resume as well when resume is set.
func withSessionStartHook(params map[string]any, hook *provider.SessionStartHook, resume bool) map[string]any {
	if hook == nil {
		return params
	}
	config, _ := params["config"].(map[string]any)
	config = maps.Clone(config)
	if config == nil {
		config = map[string]any{}
	}
	config["hooks"] = sessionStartHookConfig(hook, resume)
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
			// The rollout holds the turn's items by its completion.
			for _, warning := range []string{s.sessionStart.Settle(s.rootThreadID()), s.sessionStart.Keep()} {
				if warning != "" {
					s.emitNonFatalError(warning)
				}
			}
		}
	}
	return false
}

// expectSessionStartAfterCompaction arms the hook for a compaction. Codex
// reports a compaction before it runs the compaction's hooks, so a delivery
// seen earlier came from before it and no longer counts, and the hook has
// not had its chance in this turn yet.
func (s *Session) expectSessionStartAfterCompaction() {
	warning := s.sessionStart.ExpectAfresh(provider.SessionStartAfterCompaction)
	s.setSessionStartDueTurn("")
	if warning != "" {
		s.emitNonFatalError(warning)
	}
}

// DropSessionStartDeliveries forgets what the SessionStart hooks delivered
// since the last settled turn, after a revert cut the thread's history in
// place: a delivery in a cut turn, which an interrupt kept from settling,
// is no longer the model's, and the open expectation waits for the hooks
// again. The caller forgets the thread's receipts.
func (s *Session) DropSessionStartDeliveries() {
	s.sessionStart.DropDeliveries()
}

// setSessionStartDueTurn records the turn that gives the hooks of the
// current expectation their chance to run, "" for none.
func (s *Session) setSessionStartDueTurn(turnID string) {
	s.mu.Lock()
	s.sessionStartDueTurn = turnID
	s.mu.Unlock()
}
