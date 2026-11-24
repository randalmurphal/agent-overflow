package acp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

func writeFakeACPServer(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-acp.py")
	mockexec.WriteFile(t, path, []byte(script), 0o755)
	return path
}

// standardFakeACPServer speaks multiplexed ACP v1 over stdio and supports:
// initialize, session/new, session/resume, session/load, session/list,
// session/close, session/set_config_option, session/set_mode, session/prompt,
// session/cancel, session/request_permission, spontaneous post-prompt wakes,
// mid-turn steering, and deliberate crashes ("CRASH").
const standardFakeACPServer = `#!/usr/bin/env python3
import json, sys, threading, time

write_lock = threading.Lock()
sessions = {}
session_counter = 0
pending_perms = {}
perm_counter = 0
state_lock = threading.Lock()

def send(obj):
    line = json.dumps(obj)
    with write_lock:
        sys.stdout.write(line + "\n")
        sys.stdout.flush()

def req_permission(sid, tool_call, options):
    global perm_counter
    with state_lock:
        perm_counter += 1
        pid = 9000 + perm_counter
        ev = threading.Event()
        pending_perms[pid] = {"event": ev, "result": None}
    send({
        "jsonrpc": "2.0",
        "id": pid,
        "method": "session/request_permission",
        "params": {
            "sessionId": sid,
            "toolCall": tool_call,
            "options": options,
        },
    })
    ev.wait(timeout=10.0)
    with state_lock:
        entry = pending_perms.pop(pid, {})
    return entry.get("result")

def handle_prompt(rpc_id, sid, prompt):
    text = " ".join(b.get("text", "") for b in prompt if b.get("type") == "text")
    st = sessions.get(sid, {})
    st["cancelled"] = False

    if text == "CRASH":
        os._exit(1)

    if text == "ASK_APPROVAL":
        send({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {
                "sessionId": sid,
                "update": {
                    "sessionUpdate": "tool_call",
                    "toolCallId": "call_exec_1",
                    "name": "run_command",
                    "kind": "execute",
                    "title": "echo hi",
                    "status": "in_progress",
                    "rawInput": {"CommandLine": "echo hi"},
                },
            },
        })
        res = req_permission(sid, {
            "toolCallId": "call_exec_1",
            "name": "run_command",
            "kind": "execute",
            "title": "echo hi",
            "status": "pending",
            "rawInput": {"CommandLine": "echo hi"},
        }, [
            {"optionId": "allow", "name": "Allow", "kind": "allow_once"},
            {"optionId": "deny", "name": "Deny", "kind": "reject_once"},
        ])
        outcome = (res or {}).get("outcome", {})
        opt = outcome.get("optionId", "")
        status = "completed" if opt == "allow" else "failed"
        send({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {
                "sessionId": sid,
                "update": {
                    "sessionUpdate": "tool_call_update",
                    "toolCallId": "call_exec_1",
                    "name": "run_command",
                    "kind": "execute",
                    "status": status,
                    "rawOutput": {"output": "outcome=" + opt, "exit_code": 0 if opt == "allow" else 1},
                },
            },
        })
        send({
            "jsonrpc": "2.0",
            "id": rpc_id,
            "result": {"stopReason": "end_turn", "usage": {"inputTokens": 100, "outputTokens": 20, "cachedReadTokens": 50, "thoughtTokens": 5}},
        })
        return

    if text == "ASK_INTERACTION":
        res = req_permission(sid, {
            "toolCallId": "interaction_choice_1",
            "title": "Pick a color",
            "kind": "other",
            "status": "pending",
        }, [
            {"optionId": "opt_blue", "name": "Blue", "kind": "allow_once"},
            {"optionId": "opt_red", "name": "Red", "kind": "allow_once"},
        ])
        opt = ((res or {}).get("outcome") or {}).get("optionId", "")
        send({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {
                "sessionId": sid,
                "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "picked:" + opt}},
            },
        })
        send({"jsonrpc": "2.0", "id": rpc_id, "result": {"stopReason": "end_turn"}})
        return

    if text == "ASK_TOOL_Q":
        send({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {
                "sessionId": sid,
                "update": {
                    "sessionUpdate": "tool_call",
                    "toolCallId": "call_q_1",
                    "name": "ask_question",
                    "kind": "other",
                    "status": "pending",
                    "rawInput": {"questions": [{"question": "Alpha or Beta?", "options": ["Alpha", "Beta"], "is_multi_select": False}]},
                },
            },
        })
        send({"jsonrpc": "2.0", "id": rpc_id, "result": {"stopReason": "end_turn"}})
        return

    if text == "STEER_SLOW":
        st["steer_ev"] = threading.Event()
        st["steer_ev"].wait(timeout=5.0)
        send({
            "jsonrpc": "2.0",
            "id": rpc_id,
            "result": {"stopReason": "end_turn", "usage": {"inputTokens": 10, "outputTokens": 5}},
        })
        return

    if text == "STEER_FAST":
        ev = st.get("steer_ev")
        if ev:
            ev.set()
        time.sleep(0.05)
        send({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {
                "sessionId": sid,
                "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "steered-ok"}},
            },
        })
        send({
            "jsonrpc": "2.0",
            "id": rpc_id,
            "result": {"stopReason": "end_turn", "usage": {"inputTokens": 20, "outputTokens": 15}},
        })
        return

    if text == "WAIT_CANCEL":
        for _ in range(100):
            if st.get("cancelled"):
                send({"jsonrpc": "2.0", "id": rpc_id, "result": {"stopReason": "cancelled"}})
                return
            time.sleep(0.02)
        send({"jsonrpc": "2.0", "id": rpc_id, "result": {"stopReason": "end_turn"}})
        return

    if text == "BG_WAKE":
        send({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {
                "sessionId": sid,
                "update": {"sessionUpdate": "tool_call", "toolCallId": "call_bg_1", "name": "run_command", "kind": "execute", "status": "in_progress"},
            },
        })
        send({"jsonrpc": "2.0", "id": rpc_id, "result": {"stopReason": "end_turn"}})
        def wake():
            time.sleep(0.08)
            send({
                "jsonrpc": "2.0",
                "method": "session/update",
                "params": {
                    "sessionId": sid,
                    "update": {"sessionUpdate": "tool_call_update", "toolCallId": "call_bg_1", "name": "run_command", "kind": "execute", "status": "completed", "rawOutput": {"output": "bg done", "exit_code": 0}},
                },
            })
            send({
                "jsonrpc": "2.0",
                "method": "session/update",
                "params": {
                    "sessionId": sid,
                    "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "background finished"}},
                },
            })
            send({
                "jsonrpc": "2.0",
                "method": "session/update",
                "params": {
                    "sessionId": sid,
                    "update": {"sessionUpdate": "usage_update", "used": 2048, "size": 1048576},
                },
            })
        threading.Thread(target=wake, daemon=True).start()
        return

    # Default echo turn with thinking, diff tool, usage_update, and title.
    send({
        "jsonrpc": "2.0",
        "method": "session/update",
        "params": {
            "sessionId": sid,
            "update": {"sessionUpdate": "agent_thought_chunk", "content": {"type": "text", "text": "thinking..."}},
        },
    })
    send({
        "jsonrpc": "2.0",
        "method": "session/update",
        "params": {
            "sessionId": sid,
            "update": {
                "sessionUpdate": "tool_call",
                "toolCallId": "call_edit_" + sid,
                "name": "edit_file",
                "kind": "edit",
                "status": "in_progress",
                "rawInput": {"Path": "foo.txt"},
            },
        },
    })
    send({
        "jsonrpc": "2.0",
        "method": "session/update",
        "params": {
            "sessionId": sid,
            "update": {
                "sessionUpdate": "tool_call_update",
                "toolCallId": "call_edit_" + sid,
                "name": "edit_file",
                "kind": "edit",
                "status": "completed",
                "content": [{"type": "diff", "path": "foo.txt", "oldText": "a\n", "newText": "b\n"}],
            },
        },
    })
    send({
        "jsonrpc": "2.0",
        "method": "session/update",
        "params": {
            "sessionId": sid,
            "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": f"reply[{sid}|{st.get('model')}|{st.get('effort')}|{st.get('mode')}]: {text}"}},
        },
    })
    send({
        "jsonrpc": "2.0",
        "method": "session/update",
        "params": {
            "sessionId": sid,
            "update": {"sessionUpdate": "usage_update", "used": 1000, "size": 1048576},
        },
    })
    send({
        "jsonrpc": "2.0",
        "id": rpc_id,
        "result": {
            "stopReason": "end_turn",
            "usage": {"inputTokens": 400, "outputTokens": 120, "cachedReadTokens": 500, "thoughtTokens": 80, "totalTokens": 1020},
        },
    })

import os

def make_config_options(model, effort, mode):
    return [
        {"id": "model", "name": "Model", "type": "select", "currentValue": model, "options": [
            {"value": "model-a", "name": "Model A"},
            {"value": "model-b", "name": "Model B"},
        ]},
        {"id": "thought_level", "name": "Thought", "type": "select", "currentValue": effort, "options": [
            {"value": "low", "name": "Low"},
            {"value": "high", "name": "High"},
        ]},
        {"id": "mode", "name": "Mode", "type": "select", "currentValue": mode, "options": [
            {"value": "default", "name": "Default"},
            {"value": "accept-edits", "name": "Accept Edits"},
            {"value": "auto", "name": "Auto"},
            {"value": "plan", "name": "Plan"},
        ]},
    ]

for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    msg = json.loads(raw)
    # Response to our server-initiated permission request.
    if "method" not in msg and "id" in msg:
        with state_lock:
            entry = pending_perms.get(msg["id"])
        if entry:
            entry["result"] = msg.get("result")
            entry["event"].set()
        continue

    method = msg.get("method", "")
    rpc_id = msg.get("id")
    params = msg.get("params") or {}

    if method == "initialize":
        send({
            "jsonrpc": "2.0",
            "id": rpc_id,
            "result": {
                "protocolVersion": 1,
                "agentCapabilities": {
                    "loadSession": True,
                    "load_session": True,
                    "promptCapabilities": {"image": True, "audio": False, "embeddedContext": True},
                    "sessionCapabilities": {"resume": {}, "close": {}, "load": {}, "list": {}},
                    "mcpCapabilities": {"http": True},
                },
                "agentInfo": {"name": "Fake ACP", "version": "1.5.13"},
                "authMethods": [{"id": "oauth-personal", "name": "Google"}],
            },
        })
    elif method == "session/new":
        session_counter += 1
        sid = f"sess-{session_counter}"
        sessions[sid] = {"cwd": params.get("cwd", ""), "model": "model-a", "effort": "high", "mode": "default", "mcp": params.get("mcpServers", [])}
        send({
            "jsonrpc": "2.0",
            "id": rpc_id,
            "result": {
                "sessionId": sid,
                "modes": {"currentModeId": "default", "availableModes": [
                    {"id": "default", "name": "Default"},
                    {"id": "accept-edits", "name": "Accept Edits"},
                    {"id": "auto", "name": "Auto"},
                    {"id": "plan", "name": "Plan"},
                ]},
                "configOptions": make_config_options("model-a", "high", "default"),
            },
        })
        send({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {
                "sessionId": sid,
                "update": {"sessionUpdate": "available_commands_update", "availableCommands": [{"name": "compact", "description": "Compact context"}]},
            },
        })
    elif method == "session/resume":
        sid = params.get("sessionId", "")
        sessions[sid] = {"cwd": params.get("cwd", ""), "model": "model-a", "effort": "high", "mode": "default"}
        send({
            "jsonrpc": "2.0",
            "id": rpc_id,
            "result": {
                "sessionId": sid,
                "modes": {"currentModeId": "default", "availableModes": [
                    {"id": "default", "name": "Default"},
                    {"id": "accept-edits", "name": "Accept Edits"},
                    {"id": "auto", "name": "Auto"},
                    {"id": "plan", "name": "Plan"},
                ]},
                "configOptions": make_config_options("model-a", "high", "default"),
            },
        })
    elif method == "session/load":
        sid = params.get("sessionId", "")
        send({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {"sessionId": sid, "update": {"sessionUpdate": "user_message_chunk", "content": {"type": "text", "text": "hello imported"}}},
        })
        send({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {"sessionId": sid, "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "world imported"}}},
        })
        send({"jsonrpc": "2.0", "id": rpc_id, "result": {"sessionId": sid}})
    elif method == "session/list":
        # Deliberately ignores params["cwd"] like agy 1.5.13 to verify client-side filter.
        send({
            "jsonrpc": "2.0",
            "id": rpc_id,
            "result": {"sessions": [
                {"sessionId": "s1", "cwd": "/work/alpha", "title": "Alpha"},
                {"sessionId": "s2", "cwd": "/work/beta", "title": "Beta"},
            ]},
        })
    elif method == "session/set_config_option":
        sid = params["sessionId"]
        cid = params["configId"]
        val = params["value"]
        st = sessions.setdefault(sid, {"model": "model-a", "effort": "high", "mode": "default"})
        if cid == "model":
            st["model"] = val
        elif cid == "thought_level":
            st["effort"] = val
        elif cid == "mode":
            st["mode"] = val
        send({"jsonrpc": "2.0", "id": rpc_id, "result": {"configOptions": make_config_options(st["model"], st["effort"], st["mode"])}})
    elif method == "session/set_mode":
        sid = params["sessionId"]
        st = sessions.setdefault(sid, {"model": "model-a", "effort": "high", "mode": "default"})
        st["mode"] = params["modeId"]
        send({"jsonrpc": "2.0", "id": rpc_id, "result": {}})
        send({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": sid, "update": {"sessionUpdate": "current_mode_update", "currentModeId": params["modeId"]}}})
    elif method == "session/close":
        sessions.pop(params.get("sessionId"), None)
        send({"jsonrpc": "2.0", "id": rpc_id, "result": {}})
    elif method == "session/cancel":
        st = sessions.get(params.get("sessionId"))
        if st:
            st["cancelled"] = True
    elif method == "session/prompt":
        threading.Thread(target=handle_prompt, args=(rpc_id, params["sessionId"], params["prompt"]), daemon=True).start()
    else:
        send({"jsonrpc": "2.0", "id": rpc_id, "error": {"code": -32601, "message": "Method not found: " + method}})
`

type eventCollector struct {
	mu     sync.Mutex
	events []provider.ProviderEvent
	notify chan struct{}
}

func newEventCollector() *eventCollector {
	return &eventCollector{notify: make(chan struct{}, 1)}
}

func (c *eventCollector) record(evt provider.ProviderEvent) {
	c.mu.Lock()
	c.events = append(c.events, evt)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *eventCollector) waitFor(t *testing.T, timeout time.Duration, pred func(provider.ProviderEvent) bool) provider.ProviderEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		for _, e := range c.events {
			if pred(e) {
				c.mu.Unlock()
				return e
			}
		}
		c.mu.Unlock()
		select {
		case <-c.notify:
		case <-deadline:
			c.mu.Lock()
			kinds := make([]provider.EventKind, 0, len(c.events))
			for _, e := range c.events {
				kinds = append(kinds, e.Kind)
			}
			c.mu.Unlock()
			t.Fatalf("timed out waiting for event; observed kinds=%v", kinds)
		}
	}
}

func (c *eventCollector) all() []provider.ProviderEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]provider.ProviderEvent(nil), c.events...)
}

type questionTestFlavor struct {
	DefaultFlavor
}

func (questionTestFlavor) ExtractPendingQuestion(u *ToolCallUpdate) ([]provider.UserInputQuestion, bool) {
	if u.Name != "ask_question" {
		return nil, false
	}
	var payload struct {
		Questions []struct {
			Question      string   `json:"question"`
			Options       []string `json:"options"`
			IsMultiSelect bool     `json:"is_multi_select"`
		} `json:"questions"`
	}
	if json.Unmarshal(u.EffectiveInput(), &payload) != nil || len(payload.Questions) == 0 {
		return nil, false
	}
	var out []provider.UserInputQuestion
	for _, q := range payload.Questions {
		opts := make([]provider.UserInputQuestionOption, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, provider.UserInputQuestionOption{Label: o})
		}
		out = append(out, provider.UserInputQuestion{
			Question:    q.Question,
			Options:     opts,
			MultiSelect: q.IsMultiSelect,
		})
	}
	return out, true
}

func TestACPMultiplexedSessionsLiveConfigAndCloseIsolation(t *testing.T) {
	t.Parallel()
	bin := writeFakeACPServer(t, standardFakeACPServer)
	conn, err := Dial(context.Background(), ConnConfig{
		Binary:       bin,
		WorkDir:      t.TempDir(),
		Reaper:       procutiltest.Reaper(),
		ProviderName: "acp",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if conn.AgentInfo().Version != "1.5.13" || !conn.Capabilities().LoadSession {
		t.Fatalf("unexpected initialize state: info=%+v caps=%+v", conn.AgentInfo(), conn.Capabilities())
	}

	evA := newEventCollector()
	evB := newEventCollector()
	var relA, relB bool
	sessA, err := conn.OpenPooled(context.Background(), "thread-a", SessionConfig{
		WorkDir:         t.TempDir(),
		Model:           "model-b",
		ReasoningEffort: "low",
		RuntimeMode:     provider.RuntimeAutoAcceptEdits,
	}, evA.record, func() { relA = true })
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	sessB, err := conn.OpenPooled(context.Background(), "thread-b", SessionConfig{
		WorkDir:         t.TempDir(),
		Model:           "model-a",
		ReasoningEffort: "high",
		RuntimeMode:     provider.RuntimeFullAccess,
	}, evB.record, func() { relB = true })
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	if sessA.PID() != sessB.PID() || sessA.PID() <= 0 {
		t.Fatalf("expected shared PID, got A=%d B=%d", sessA.PID(), sessB.PID())
	}

	// Send concurrently on both sessions over the single Conn.
	if err := sessA.Send(context.Background(), "hello A", provider.SendOptions{}); err != nil {
		t.Fatalf("Send A: %v", err)
	}
	if err := sessB.Send(context.Background(), "hello B", provider.SendOptions{}); err != nil {
		t.Fatalf("Send B: %v", err)
	}

	doneA := evA.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventTurnComplete })
	doneB := evB.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventTurnComplete })

	if wt, ok := doneA.TurnComplete.(*provider.WireTurnCompleteMeta); !ok || wt.Usage == nil || wt.Usage.InputTokens != 400 || wt.Usage.CacheReadInputTokens != 500 || wt.Usage.OutputTokens != 120 || wt.Usage.ReasoningOutputTokens != 80 {
		t.Fatalf("unexpected usage on A: %+v", doneA.TurnComplete)
	}
	_ = doneB

	// Verify text, diff, and live config isolation.
	textA := evA.waitFor(t, 2*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventTextDelta })
	if !strings.Contains(textA.Content, "model-b|low|accept-edits") || !strings.Contains(textA.Content, "hello A") {
		t.Fatalf("unexpected A reply: %q", textA.Content)
	}
	toolCompA := evA.waitFor(t, 2*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventToolComplete && e.ItemType == "file_change"
	})
	if !strings.Contains(string(toolCompA.Meta), "@@") || !strings.Contains(string(toolCompA.Meta), "+b") || !strings.Contains(string(toolCompA.Meta), `"changes"`) {
		t.Fatalf("unexpected file_change tool_complete meta: %s", string(toolCompA.Meta))
	}

	// Live config update on B without restarting.
	if err := sessB.ApplyLiveUpdate(context.Background(), LiveUpdate{
		Model:           "model-b",
		HasModel:        true,
		ReasoningEffort: "low",
		HasEffort:       true,
		RuntimeMode:     provider.RuntimeAuto,
		HasRuntimeMode:  true,
	}); err != nil {
		t.Fatalf("ApplyLiveUpdate B: %v", err)
	}

	// Close A; B and Conn must stay alive.
	if err := sessA.Close(); err != nil {
		t.Fatalf("Close A: %v", err)
	}
	if !relA || relB {
		t.Fatalf("expected relA=true relB=false, got relA=%v relB=%v", relA, relB)
	}

	// Send second turn on B and verify new live config took effect.
	if err := sessB.Send(context.Background(), "after live update", provider.SendOptions{}); err != nil {
		t.Fatalf("Send B #2: %v", err)
	}
	evB.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventTextDelta && strings.Contains(e.Content, "model-b|low|auto")
	})

	if err := sessB.Close(); err != nil {
		t.Fatalf("Close B: %v", err)
	}
	if !relB {
		t.Fatal("expected relB=true after Close B")
	}
}

func TestACPRuntimeModePermissionsAndQuestions(t *testing.T) {
	t.Parallel()
	bin := writeFakeACPServer(t, standardFakeACPServer)
	conn, err := Dial(context.Background(), ConnConfig{
		Binary:       bin,
		WorkDir:      t.TempDir(),
		Reaper:       procutiltest.Reaper(),
		ProviderName: "acp",
		Flavor:       questionTestFlavor{DefaultFlavor{Kind: "acp"}},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 1. RuntimeReadOnly auto-denies mutating run_command without prompting user.
	evRO := newEventCollector()
	sessRO, err := conn.Open(context.Background(), "t-ro", SessionConfig{
		WorkDir:     t.TempDir(),
		RuntimeMode: provider.RuntimeReadOnly,
	}, evRO.record)
	if err != nil {
		t.Fatalf("Open RO: %v", err)
	}
	if err := sessRO.Send(context.Background(), "ASK_APPROVAL", provider.SendOptions{}); err != nil {
		t.Fatalf("Send RO: %v", err)
	}
	evRO.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventTurnComplete })
	for _, e := range evRO.all() {
		if e.Kind == provider.EventApprovalRequest {
			t.Fatal("RuntimeReadOnly should auto-deny mutating tool without prompting user")
		}
	}
	tcRO := evRO.waitFor(t, 2*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventToolComplete })
	if !strings.Contains(tcRO.Content, "outcome=deny") {
		t.Fatalf("expected outcome=deny in read-only, got %q", tcRO.Content)
	}
	_ = sessRO.Close()

	// 2. RuntimeFullAccess auto-allows run_command, but still prompts interaction_* questions.
	evFA := newEventCollector()
	sessFA, err := conn.Open(context.Background(), "t-fa", SessionConfig{
		WorkDir:     t.TempDir(),
		RuntimeMode: provider.RuntimeFullAccess,
	}, evFA.record)
	if err != nil {
		t.Fatalf("Open FA: %v", err)
	}
	if err := sessFA.Send(context.Background(), "ASK_APPROVAL", provider.SendOptions{}); err != nil {
		t.Fatalf("Send FA approval: %v", err)
	}
	tcFA := evFA.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventToolComplete })
	if !strings.Contains(tcFA.Content, "outcome=allow") {
		t.Fatalf("expected outcome=allow in full-access, got %q", tcFA.Content)
	}
	evFA.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventTurnComplete })

	if err := sessFA.Send(context.Background(), "ASK_INTERACTION", provider.SendOptions{}); err != nil {
		t.Fatalf("Send FA interaction: %v", err)
	}
	uiEvt := evFA.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventUserInputRequest })
	var uiReq provider.UserInputRequest
	if err := json.Unmarshal(uiEvt.Meta, &uiReq); err != nil {
		t.Fatalf("unmarshal user input req: %v", err)
	}
	if err := sessFA.RespondToUserInput(context.Background(), provider.UserInputResponse{
		RequestID: uiReq.RequestID,
		Decision:  "accept",
		Answers:   map[string]provider.UserInputAnswer{uiReq.Questions[0].ID: {"Blue"}},
	}); err != nil {
		t.Fatalf("RespondToUserInput interaction: %v", err)
	}
	evFA.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventTextDelta && strings.Contains(e.Content, "picked:opt_blue")
	})

	// 3. Pending ask_question tool answered via RespondToUserInput -> text session/prompt.
	if err := sessFA.Send(context.Background(), "ASK_TOOL_Q", provider.SendOptions{}); err != nil {
		t.Fatalf("Send ask_question: %v", err)
	}
	qEvt := evFA.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventUserInputRequest && e.ItemID == "call_q_1"
	})
	var qReq provider.UserInputRequest
	_ = json.Unmarshal(qEvt.Meta, &qReq)
	if err := sessFA.RespondToUserInput(context.Background(), provider.UserInputResponse{
		RequestID: qReq.RequestID,
		Decision:  "accept",
		Answers:   map[string]provider.UserInputAnswer{qReq.Questions[0].ID: {"Beta"}},
	}); err != nil {
		t.Fatalf("RespondToUserInput ask_question: %v", err)
	}
	evFA.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventToolComplete && e.ItemID == "call_q_1"
	})
	evFA.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventTextDelta && strings.Contains(e.Content, ": 2")
	})
	_ = sessFA.Close()
}

func TestACPSteerCancelWakeAndCrashFanOut(t *testing.T) {
	t.Parallel()
	bin := writeFakeACPServer(t, standardFakeACPServer)
	conn, err := Dial(context.Background(), ConnConfig{
		Binary:       bin,
		WorkDir:      t.TempDir(),
		Reaper:       procutiltest.Reaper(),
		ProviderName: "acp",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 1. Mid-turn steer sums usage and emits one TurnComplete after both prompts finish.
	ev := newEventCollector()
	sess, err := conn.Open(context.Background(), "t-ctrl", SessionConfig{WorkDir: t.TempDir()}, ev.record)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sess.Send(context.Background(), "STEER_SLOW", provider.SendOptions{}); err != nil {
		t.Fatalf("Send slow: %v", err)
	}
	if err := sess.Send(context.Background(), "STEER_FAST", provider.SendOptions{}); err != nil {
		t.Fatalf("Send steer: %v", err)
	}
	done := ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventTurnComplete })
	if wt, ok := done.TurnComplete.(*provider.WireTurnCompleteMeta); !ok || wt.Usage == nil || wt.Usage.InputTokens != 30 || wt.Usage.OutputTokens != 20 {
		t.Fatalf("expected combined steer usage (30/20), got %+v", done.TurnComplete)
	}

	// 2. Interrupt via session/cancel -> stopReason=cancelled.
	if err := sess.Send(context.Background(), "WAIT_CANCEL", provider.SendOptions{}); err != nil {
		t.Fatalf("Send cancel: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		if e.Kind != provider.EventTurnComplete {
			return false
		}
		wt, ok := e.TurnComplete.(*provider.WireTurnCompleteMeta)
		return ok && wt.Aborted && wt.StopReason == "cancelled"
	})

	// 3. Spontaneous post-prompt wake opens a wake turn and closes on trailing usage_update.
	evWake := newEventCollector()
	sessWake, err := conn.Open(context.Background(), "t-wake", SessionConfig{WorkDir: t.TempDir()}, evWake.record)
	if err != nil {
		t.Fatalf("Open wake: %v", err)
	}
	if err := sessWake.Send(context.Background(), "BG_WAKE", provider.SendOptions{}); err != nil {
		t.Fatalf("Send BG_WAKE: %v", err)
	}
	// Wait for the second TurnComplete (first is BG_WAKE prompt end_turn, second is spontaneous wake usage_update).
	evWake.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventTurnComplete && strings.HasPrefix(e.TurnID, "acp-wake-")
	})

	// 4. ListSessions client-side cwd filter & ReplayLoad bounded capture.
	filtered, err := conn.ListSessions(context.Background(), "/work/alpha")
	if err != nil || len(filtered) != 1 || filtered[0].SessionID != "s1" {
		t.Fatalf("ListSessions filtered = %+v, err=%v", filtered, err)
	}
	replay, err := conn.ReplayLoad(context.Background(), SessionLoadParams{SessionID: "s1", CWD: "/work/alpha"}, 100)
	if err != nil || len(replay.Updates) != 2 {
		t.Fatalf("ReplayLoad = %+v, err=%v", replay, err)
	}

	// 5. Crash fan-out to both attached sessions.
	_ = sess.Send(context.Background(), "CRASH", provider.SendOptions{})
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("conn.Done did not close after crash")
	}
	ev.waitFor(t, 2*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventSessionStatus && e.Content == "error"
	})
	evWake.waitFor(t, 2*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventSessionStatus && e.Content == "disconnected"
	})
}

func TestRedactOutboundACPFrameMasksMCPSecrets(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/w","mcpServers":[{"type":"http","name":"ao-thread-tools","url":"http://127.0.0.1:9999/mcp","headers":[{"name":"Authorization","value":"Bearer super-secret"}],"env":[{"name":"API_KEY","value":"top-secret"}]}]}}`)
	redacted := string(redactOutboundACPFrame("out", raw))
	if strings.Contains(redacted, "super-secret") || strings.Contains(redacted, "top-secret") {
		t.Fatalf("secrets leaked in redacted frame: %s", redacted)
	}
	if !strings.Contains(redacted, "[redacted]") {
		t.Fatalf("expected [redacted] marker: %s", redacted)
	}
	// Inbound frames are untouched.
	if got := string(redactOutboundACPFrame("in", raw)); got != string(raw) {
		t.Fatalf("inbound frame should not be modified")
	}
}

func TestPlanLiveUpdateAllowsLifecycleDriftAndLiveAxes(t *testing.T) {
	t.Parallel()
	base := provider.SessionOptions{
		WorkDir:         "/repo",
		Model:           "gemini-3.1-pro-high",
		ReasoningEffort: provider.EffortHigh,
		RuntimeMode:     provider.RuntimeAuto,
		Mode:            provider.ModeChat,
	}

	// Recording a newly assigned SessionRef or ForkSession flag on the thread
	// row must not force a restart when updating live-appliable axes.
	target := base
	target.Resume = "acp-sess-1"
	target.ForkSession = true
	target.Model = "gemini-3-flash"
	target.ReasoningEffort = provider.EffortLow
	target.RuntimeMode = provider.RuntimeApprovalRequired
	target.Mode = provider.ModePlan

	u, ok := PlanLiveUpdate(base, target)
	if !ok {
		t.Fatal("PlanLiveUpdate returned false when only lifecycle state + live axes changed")
	}
	if !u.HasModel || u.Model != "gemini-3-flash" ||
		!u.HasEffort || u.ReasoningEffort != "low" ||
		!u.HasRuntimeMode || u.RuntimeMode != provider.RuntimeApprovalRequired ||
		!u.HasInteraction || u.InteractionMode != provider.ModePlan {
		t.Fatalf("unexpected LiveUpdate: %+v", u)
	}

	// Spawn-only changes (WorkDir, SystemPrompt, DisabledTools) still require restart.
	for _, mutate := range []func(*provider.SessionOptions){
		func(o *provider.SessionOptions) { o.WorkDir = "/other" },
		func(o *provider.SessionOptions) { o.SystemPrompt = "custom" },
		func(o *provider.SessionOptions) { o.DisabledTools = []string{"bash"} },
	} {
		changed := base
		mutate(&changed)
		if _, ok := PlanLiveUpdate(base, changed); ok {
			t.Fatalf("PlanLiveUpdate unexpectedly allowed spawn-only change: %+v", changed)
		}
	}
}

func TestExtractStructuredJSONObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"raw", `{"status":"done","outputs":{"ok":true}}`, `{"status":"done","outputs":{"ok":true}}`},
		{"fenced", "```json\n{\"status\":\"done\"}\n```", `{"status":"done"}`},
		{"prose-wrapped", "Here is the result:\n{\"status\":\"stuck\",\"reason\":\"blocked\"}\nThanks!", `{"status":"stuck","reason":"blocked"}`},
		{"invalid", "not json at all", ""},
	}
	for _, tc := range cases {
		got := string(extractStructuredJSONObject(tc.in))
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReplayUpdatesToEventsPreservesRichToolMetadata(t *testing.T) {
	t.Parallel()
	exitZero := 0
	rawIn, _ := json.Marshal(map[string]any{"command": "echo hi"})
	rawOut, _ := json.Marshal(map[string]any{"output": "hi\n", "exit_code": exitZero, "duration_ms": 42})
	updates := []SessionUpdate{
		{
			SessionUpdate: "user_message_chunk",
			Content:       json.RawMessage(`{"type":"text","text":"run it"}`),
		},
		{
			SessionUpdate: "tool_call",
			ToolCallID:    "cmd-1",
			Name:          "Bash",
			Kind:          "execute",
			RawInput:      rawIn,
		},
		{
			SessionUpdate: "tool_call_update",
			ToolCallID:    "cmd-1",
			Name:          "Bash",
			Kind:          "execute",
			Status:        "completed",
			RawInput:      rawIn,
			RawOutput:     rawOut,
		},
		{
			SessionUpdate: "tool_call",
			ToolCallID:    "edit-1",
			Name:          "Edit",
			Kind:          "edit",
		},
		{
			SessionUpdate: "tool_call_update",
			ToolCallID:    "edit-1",
			Name:          "Edit",
			Kind:          "edit",
			Status:        "completed",
			Content:       json.RawMessage(`[{"type":"diff","path":"/repo/a.txt","oldText":"old\n","newText":"new\n"}]`),
		},
	}
	events := ReplayUpdatesToEvents(updates, DefaultFlavor{Kind: provider.Agy})
	var sawCmdOut, sawFileChangeMeta bool
	for _, ev := range events {
		if ev.Kind == provider.EventCommandOutput && ev.ItemID == "cmd-1" && ev.Content == "hi\n" {
			sawCmdOut = true
		}
		if ev.Kind == provider.EventToolComplete && ev.ItemID == "edit-1" {
			var meta map[string]any
			if json.Unmarshal(ev.Meta, &meta) == nil {
				if item, ok := meta["item"].(map[string]any); ok && item["type"] == "file_change" {
					sawFileChangeMeta = true
				}
			}
		}
	}
	if !sawCmdOut {
		t.Fatalf("ReplayUpdatesToEvents did not emit EventCommandOutput: %+v", events)
	}
	if !sawFileChangeMeta {
		t.Fatalf("ReplayUpdatesToEvents did not emit structured file_change item meta: %+v", events)
	}
}

func TestReplayUpdatesToEventsCompletesInlineTerminalToolCalls(t *testing.T) {
	t.Parallel()
	// Mirror real agy 1.5.14 `session/load` replay: each tool call arrives as a
	// single `tool_call` update already carrying status="completed" and output.
	updates := []SessionUpdate{
		{
			SessionUpdate: "user_message_chunk",
			Content:       json.RawMessage(`{"type":"text","text":"run echo"}`),
		},
		{
			SessionUpdate: "tool_call",
			ToolCallID:    "cmd-inline",
			Name:          "Bash",
			Kind:          "execute",
			Status:        "completed",
			RawInput:      json.RawMessage(`{"command":"echo ok"}`),
			RawOutput:     json.RawMessage(`{"stdout":"ok\n","exitCode":0}`),
		},
		// A redundant follow-up tool_call_update must not duplicate completion.
		{
			SessionUpdate: "tool_call_update",
			ToolCallID:    "cmd-inline",
			Name:          "Bash",
			Kind:          "execute",
			Status:        "completed",
			RawOutput:     json.RawMessage(`{"stdout":"ok\n","exitCode":0}`),
		},
		{
			SessionUpdate: "agent_message_chunk",
			Content:       json.RawMessage(`{"type":"text","text":"done"}`),
		},
	}
	events := ReplayUpdatesToEvents(updates, DefaultFlavor{Kind: provider.Agy})
	var starts, completes, cmdOuts int
	for _, ev := range events {
		if ev.Kind == provider.EventToolStart {
			starts++
		} else if ev.Kind == provider.EventToolComplete {
			completes++
		} else if ev.Kind == provider.EventCommandOutput && ev.Content == "ok\n" {
			cmdOuts++
		}
	}
	if starts != 1 || completes != 1 || cmdOuts != 1 {
		t.Fatalf("got starts=%d completes=%d cmdOuts=%d, want 1/1/1: %+v", starts, completes, cmdOuts, events)
	}
	if len(events) < 2 || events[0].Kind != provider.EventTurnStart || events[1].Kind != provider.EventUserText {
		t.Fatalf("expected EventTurnStart before EventUserText, got: %+v", events[:min(3, len(events))])
	}
	last := events[len(events)-1]
	if last.Kind != provider.EventTurnComplete || last.TurnComplete == nil {
		t.Fatalf("expected typed EventTurnComplete last, got %+v", last)
	}
}

func TestSessionConfigSyncAndToolUpdateStreamingAndCancel(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s := &Session{
		threadID:         "t-sync",
		flavor:           DefaultFlavor{Kind: provider.Agy},
		onEvent:          func(e provider.ProviderEvent) { events = append(events, e) },
		startedTools:     make(map[string]NormalizedToolStart),
		trackedSubagents: make(map[string]SubagentLaunchContext),
		model:            "gemini-4-neon",
		reasoningEffort:  "medium",
		activeTurnID:     "turn-1",
		turnOpen:         true,
	}

	// 1. config_option_update syncs model and reasoningEffort live.
	s.handleUpdate(SessionUpdate{
		SessionUpdate: "config_option_update",
		ConfigOptions: []ConfigOption{
			{ID: "model", CurrentValue: "gemini-4-hydrogen"},
			{ID: "thought_level", CurrentValue: "high"},
		},
	})
	if s.model != "gemini-4-hydrogen" || s.reasoningEffort != "high" {
		t.Fatalf("config_option_update sync = (%q, %q), want (gemini-4-hydrogen, high)", s.model, s.reasoningEffort)
	}

	// 2. tool_call -> in_progress streaming output -> cancelled terminal (mapped to killed).
	s.handleUpdate(SessionUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    "cmd-1",
		Name:          "Bash",
		Kind:          "execute",
		Status:        "in_progress",
		RawInput:      json.RawMessage(`{"command":"sleep 10"}`),
	})
	s.handleUpdate(SessionUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    "cmd-1",
		Name:          "Bash",
		Kind:          "execute",
		Status:        "in_progress",
		RawOutput:     json.RawMessage(`{"stdout":"partial line\n"}`),
	})
	s.handleUpdate(SessionUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    "cmd-1",
		Name:          "Bash",
		Kind:          "execute",
		Status:        "cancelled",
	})

	var sawStreamOut, sawKilled bool
	for _, ev := range events {
		if ev.Kind == provider.EventCommandOutput && ev.ItemID == "cmd-1" && ev.Content == "partial line\n" {
			sawStreamOut = true
		}
		if ev.Kind == provider.EventToolComplete && ev.ItemID == "cmd-1" {
			var meta map[string]any
			if json.Unmarshal(ev.Meta, &meta) == nil && meta["item_status"] == "killed" {
				sawKilled = true
			}
		}
	}
	if !sawStreamOut {
		t.Fatalf("expected streaming EventCommandOutput during in_progress: %+v", events)
	}
	if !sawKilled {
		t.Fatalf("expected cancelled tool_call_update to emit item_status=killed: %+v", events)
	}
}

func TestReadOnlyPolicyFailsClosedAndAvailableDecisionsMatchOptions(t *testing.T) {
	t.Parallel()
	f := DefaultFlavor{Kind: provider.Agy}

	// 1. RuntimeReadOnly + ModePlan MUST keep PolicyDenyMutating (never fail open to PolicyPrompt).
	modeID, policy := f.MapRuntimeMode(provider.RuntimeReadOnly, provider.ModePlan, []SessionMode{{ID: "plan"}, {ID: "default"}})
	if modeID != "plan" || policy != PolicyDenyMutating {
		t.Fatalf("MapRuntimeMode(ReadOnly, Plan) = (%q, %v), want (plan, PolicyDenyMutating)", modeID, policy)
	}

	// 2. PolicyDenyMutating fails closed on unknown / "other" MCP tools and allows explicit read kinds.
	mcpReq := RequestPermissionParams{
		ToolCall: ToolCallUpdate{ToolCallID: "t-mcp", Name: "mcp_github_create_issue", Kind: "other"},
		Options: []PermissionOption{
			{OptionID: "allow_1", Kind: "allow_once", Name: "Allow"},
			{OptionID: "deny_1", Kind: "reject_once", Name: "Reject"},
		},
	}
	clsMCP := f.ClassifyPermission("thr", "turn", "req-1", mcpReq, PolicyDenyMutating)
	if clsMCP.Action != PermissionAutoDeny {
		t.Fatalf("PolicyDenyMutating on kind=other MCP tool = %v, want PermissionAutoDeny", clsMCP.Action)
	}
	readReq := RequestPermissionParams{
		ToolCall: ToolCallUpdate{ToolCallID: "t-read", Name: "read_file", Kind: "read"},
		Options: []PermissionOption{
			{OptionID: "allow_1", Kind: "allow_once", Name: "Allow"},
		},
	}
	clsRead := f.ClassifyPermission("thr", "turn", "req-2", readReq, PolicyDenyMutating)
	if clsRead.Action != PermissionAutoAllow {
		t.Fatalf("PolicyDenyMutating on kind=read = %v, want PermissionAutoAllow", clsRead.Action)
	}

	// 3. AvailableDecisions advertises ONLY the scopes actually offered by ACP options.
	alwaysOnlyReq := RequestPermissionParams{
		ToolCall: ToolCallUpdate{ToolCallID: "t-bash", Name: "run_command", Kind: "execute"},
		Options: []PermissionOption{
			{OptionID: "allow_all", Kind: "allow_always", Name: "Always allow"},
			{OptionID: "deny_1", Kind: "reject_once", Name: "Reject"},
		},
	}
	clsAsk := f.ClassifyPermission("thr", "turn", "req-3", alwaysOnlyReq, PolicyPrompt)
	if clsAsk.Action != PermissionAskUser || clsAsk.Approval.AvailableDecisions == nil {
		t.Fatalf("ClassifyPermission(alwaysOnly) = %+v", clsAsk)
	}
	encodedAvail, _ := json.Marshal(*clsAsk.Approval.AvailableDecisions)
	availStr := string(encodedAvail)
	if strings.Contains(availStr, `"accept"`) || !strings.Contains(availStr, `"acceptForSession"`) || !strings.Contains(availStr, `"decline"`) || !strings.Contains(availStr, `"cancel"`) {
		t.Fatalf("AvailableDecisions = %s, want [cancel, decline, acceptForSession] without accept", availStr)
	}
	if _, hasAccept := clsAsk.OptionByKey["accept"]; hasAccept {
		t.Fatalf("OptionByKey unexpectedly mapped accept to allow_always: %+v", clsAsk.OptionByKey)
	}

	// 4. Distinct Session instances generate distinct turn IDs so reconnect never collides.
	s1 := &Session{instanceID: newSessionInstanceID(), turnSeq: 1}
	s2 := &Session{instanceID: newSessionInstanceID(), turnSeq: 1}
	if s1.nextTurnID("turn") == s2.nextTurnID("turn") {
		t.Fatalf("distinct sessions produced identical turn ID %q", s1.nextTurnID("turn"))
	}
}

func TestSessionOpenFailsOnRejectedInitialConfig(t *testing.T) {
	t.Parallel()
	const rejectModelScript = `#!/usr/bin/env python3
import json, sys
for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    msg = json.loads(raw)
    mid = msg.get("id")
    method = msg.get("method")
    if method == "initialize":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"protocolVersion":1,"agentInfo":{"name":"mock","version":"1.0"}}}), flush=True)
    elif method == "session/new":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"sessionId":"s-reject","configOptions":[{"id":"model","currentValue":"default-model","options":[{"value":"default-model","name":"Default"}]}]}}), flush=True)
    elif method == "session/set_config_option":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"error":{"code":-32602,"message":"unknown model"}}), flush=True)
    elif method == "session/close":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{}}), flush=True)
`
	bin := writeFakeACPServer(t, rejectModelScript)
	conn, err := Dial(context.Background(), ConnConfig{
		Binary:       bin,
		WorkDir:      t.TempDir(),
		Reaper:       procutiltest.Reaper(),
		ProviderName: "agy",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, err = conn.Open(context.Background(), "t-reject", SessionConfig{
		WorkDir: t.TempDir(),
		Model:   "nonexistent-model",
	}, func(provider.ProviderEvent) {})
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("Open with rejected model err = %v, want error containing 'unknown model'", err)
	}
	if conn.HasActiveSession("s-reject") {
		t.Fatal("failed Open left session registered on Conn")
	}
}

func TestSessionSendEmitsTranscriptRecordedConfirmationAndCancelApproval(t *testing.T) {
	t.Parallel()
	const cancelCheckScript = `#!/usr/bin/env python3
import json, sys
cancelled_sessions = []
for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    msg = json.loads(raw)
    mid = msg.get("id")
    method = msg.get("method")
    params = msg.get("params") or {}
    if method == "initialize":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"protocolVersion":1,"agentInfo":{"name":"mock","version":"1.0"}}}), flush=True)
    elif method == "session/new":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"sessionId":"s-confirm","modes":{"currentModeId":"ask","availableModes":[{"id":"ask","name":"Ask"}]}}}), flush=True)
    elif method == "session/cancel":
        cancelled_sessions.append(params.get("sessionId"))
    elif method == "session/prompt":
        sid = params.get("sessionId")
        prompt = params.get("prompt") or []
        text = prompt[0].get("text", "") if prompt else ""
        if text == "REJECT_ME":
            print(json.dumps({"jsonrpc":"2.0","id":mid,"error":{"code":-32602,"message":"prompt rejected"}}), flush=True)
        elif text == "OVERLAP_SLOW":
            # Emit an update while steered prompt B may already be in flight, then wait for B and reject B before finishing A.
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"slow-a-1"}}}}), flush=True)
            b_line = sys.stdin.readline().strip()
            b_msg = json.loads(b_line)
            b_id = b_msg.get("id")
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"slow-a-2"}}}}), flush=True)
            print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"stopReason":"end_turn"}}), flush=True)
            print(json.dumps({"jsonrpc":"2.0","id":b_id,"error":{"code":-32602,"message":"steered b rejected"}}), flush=True)
        elif text == "CANCEL_ME":
            print(json.dumps({"jsonrpc":"2.0","id":99,"method":"session/request_permission","params":{"sessionId":sid,"toolCall":{"toolCallId":"tc-cancel","name":"run_command","kind":"execute"},"options":[{"optionId":"allow_1","kind":"allow_once","name":"Allow"},{"optionId":"deny_1","kind":"reject_once","name":"Reject"}]}}), flush=True)
            resp_line = sys.stdin.readline().strip()
            resp = json.loads(resp_line)
            outcome = ((resp.get("result") or {}).get("outcome") or {}).get("outcome")
            # Wait for the follow-up session/cancel notification.
            cancel_line = sys.stdin.readline().strip()
            cancel_msg = json.loads(cancel_line)
            got_cancel = cancel_msg.get("method") == "session/cancel"
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":f"outcome={outcome},cancel={got_cancel}"}}}}), flush=True)
            print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"stopReason":"cancelled"}}), flush=True)
        else:
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ok"}}}}), flush=True)
            print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"stopReason":"end_turn"}}), flush=True)
    elif method == "session/close":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{}}), flush=True)
`
	bin := writeFakeACPServer(t, cancelCheckScript)
	conn, err := Dial(context.Background(), ConnConfig{
		Binary:       bin,
		WorkDir:      t.TempDir(),
		Reaper:       procutiltest.Reaper(),
		ProviderName: "agy",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ev := newEventCollector()
	sess, err := conn.Open(context.Background(), "t-confirm", SessionConfig{
		WorkDir:     t.TempDir(),
		RuntimeMode: provider.RuntimeApprovalRequired,
	}, ev.record)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sess.Close() }()

	// 1. Send with UserMessageUUID must emit EventUserText confirmation on
	// provider acceptance (before assistant text output).
	if err := sess.Send(context.Background(), "hello queue", provider.SendOptions{UserMessageUUID: "send-uuid-123"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventTurnComplete })
	all := ev.all()
	userIdx, textIdx := -1, -1
	for i, e := range all {
		if e.Kind == provider.EventUserText && userIdx < 0 {
			userIdx = i
			var meta map[string]any
			if err := json.Unmarshal(e.Meta, &meta); err != nil {
				t.Fatalf("unmarshal EventUserText meta: %v", err)
			}
			if meta["provider_item_id"] != "send-uuid-123" || meta[provider.MetaUserTextTranscriptRecordedKey] != true {
				t.Fatalf("unexpected EventUserText confirmation meta: %+v", meta)
			}
		}
		if e.Kind == provider.EventTextDelta && textIdx < 0 {
			textIdx = i
		}
	}
	if userIdx < 0 || textIdx < 0 || userIdx >= textIdx {
		t.Fatalf("expected EventUserText before EventTextDelta, got userIdx=%d textIdx=%d", userIdx, textIdx)
	}

	// 1b. A prompt rejected by the provider before any update MUST NOT emit
	// EventUserText confirmation (prevents false delivery receipts).
	beforeRejectCount := len(ev.all())
	if err := sess.Send(context.Background(), "REJECT_ME", provider.SendOptions{UserMessageUUID: "send-uuid-rejected"}); err != nil {
		t.Fatalf("Send REJECT_ME: %v", err)
	}
	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventError && strings.Contains(e.Content, "prompt rejected")
	})
	for _, e := range ev.all()[beforeRejectCount:] {
		if e.Kind == provider.EventUserText {
			t.Fatalf("rejected prompt falsely emitted EventUserText confirmation: %+v", e)
		}
	}

	// 1b-2. Overlapping prompt A updates and A's completion MUST NOT falsely
	// confirm a steered prompt B whose own RPC is rejected.
	beforeOverlapCount := len(ev.all())
	if err := sess.Send(context.Background(), "OVERLAP_SLOW", provider.SendOptions{UserMessageUUID: "send-uuid-a"}); err != nil {
		t.Fatalf("Send OVERLAP_SLOW: %v", err)
	}
	if err := sess.Send(context.Background(), "STEER_B_REJECT", provider.SendOptions{UserMessageUUID: "send-uuid-b-rejected"}); err != nil {
		t.Fatalf("Send STEER_B_REJECT: %v", err)
	}
	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventError && strings.Contains(e.Content, "steered b rejected")
	})
	// Wait for the logical turn to settle so subsequent sub-checks start cleanly.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		settled := false
		for _, after := range ev.all()[beforeOverlapCount:] {
			if after.Kind == provider.EventTurnComplete {
				settled = true
				break
			}
		}
		if settled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var confirmedA bool
	for _, e := range ev.all()[beforeOverlapCount:] {
		if e.Kind == provider.EventUserText && strings.Contains(string(e.Meta), "send-uuid-a") {
			confirmedA = true
		}
		if e.Kind == provider.EventUserText && strings.Contains(string(e.Meta), "send-uuid-b-rejected") {
			t.Fatalf("overlapping prompt A falsely confirmed rejected steered prompt B: %+v", e)
		}
	}
	if !confirmedA {
		t.Fatalf("expected prompt A (send-uuid-a) to still be confirmed")
	}

	// 1c. ReplayLoad on an active sessionId on the same Conn must return ErrSessionBusy.
	if _, err := conn.ReplayLoad(context.Background(), SessionLoadParams{SessionID: "s-confirm", CWD: t.TempDir()}, 100); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ReplayLoad on live session err = %v, want ErrSessionBusy", err)
	}

	// 2. RespondToApproval("cancel") must send ACP outcome=cancelled AND session/cancel.
	if err := sess.Send(context.Background(), "CANCEL_ME", provider.SendOptions{}); err != nil {
		t.Fatalf("Send CANCEL_ME: %v", err)
	}
	appEvt := ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool { return e.Kind == provider.EventApprovalRequest })
	var req provider.ApprovalRequest
	if err := json.Unmarshal(appEvt.Meta, &req); err != nil {
		t.Fatalf("unmarshal approval req: %v", err)
	}
	if err := sess.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: req.RequestID,
		Decision:  "cancel",
	}); err != nil {
		t.Fatalf("RespondToApproval(cancel): %v", err)
	}
	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventTextDelta && strings.Contains(e.Content, "outcome=cancelled,cancel=True")
	})
}

func TestBuildPromptBlocksPreservesInlineImageOrderAndApprovalDiffPreview(t *testing.T) {
	t.Parallel()
	atts := []provider.ImageAttachment{
		{ID: "img-1", MimeType: "image/png", Data: []byte("PNG1")},
		{ID: "img-2", MimeType: "image/jpeg", Data: []byte("JPG2")},
	}
	blocks := buildPromptBlocks("Before [Image #1] middle [Image #2] after", atts)
	if len(blocks) != 5 {
		t.Fatalf("len(blocks) = %d, want 5: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "Before " ||
		blocks[1].Type != "image" || blocks[1].MimeType != "image/png" ||
		blocks[2].Type != "text" || blocks[2].Text != " middle " ||
		blocks[3].Type != "image" || blocks[3].MimeType != "image/jpeg" ||
		blocks[4].Type != "text" || blocks[4].Text != " after" {
		t.Fatalf("inline image order mismatch: %+v", blocks)
	}

	var df DefaultFlavor
	cls := df.ClassifyPermission("t1", "turn-1", "req-1", RequestPermissionParams{
		ToolCall: ToolCallUpdate{
			ToolCallID: "tc-1",
			Name:       "edit_file",
			Title:      "Edit file",
			Kind:       "edit",
			Locations:  []ToolLocation{{Path: "/repo/main.go"}},
			Content: []ToolContentItem{{
				Type:    "diff",
				Path:    "/repo/main.go",
				OldText: "foo\n",
				NewText: "bar\n",
			}},
		},
		Options: []PermissionOption{
			{OptionID: "allow", Kind: "allow_once", Name: "Allow"},
			{OptionID: "deny", Kind: "reject_once", Name: "Deny"},
		},
	}, PolicyPrompt)
	if !strings.Contains(cls.Approval.Description, "/repo/main.go") {
		t.Fatalf("approval description = %q, want path included", cls.Approval.Description)
	}
	var in map[string]any
	if err := json.Unmarshal(cls.Approval.Input, &in); err != nil {
		t.Fatalf("unmarshal approval input: %v", err)
	}
	if in["path"] != "/repo/main.go" || !strings.Contains(in["diff"].(string), "-foo") || !strings.Contains(in["diff"].(string), "+bar") {
		t.Fatalf("approval input missing path/diff preview: %s", string(cls.Approval.Input))
	}
}

func TestSessionSteeredPromptConfirmsOnFirstUpdateAfterPredecessorEnds(t *testing.T) {
	t.Parallel()
	const steerConfirmScript = `#!/usr/bin/env python3
import json, sys
for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    msg = json.loads(raw)
    mid = msg.get("id")
    method = msg.get("method")
    params = msg.get("params") or {}
    if method == "initialize":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"protocolVersion":1,"agentInfo":{"name":"mock","version":"1.0"}}}), flush=True)
    elif method == "session/new":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"sessionId":"s-steer","modes":{"currentModeId":"ask","availableModes":[{"id":"ask","name":"Ask"}]}}}), flush=True)
    elif method == "session/prompt":
        sid = params.get("sessionId")
        prompt = params.get("prompt") or []
        text = prompt[0].get("text", "") if prompt else ""
        if text == "PROMPT_A":
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"from-a"}}}}), flush=True)
            b_msg = json.loads(sys.stdin.readline().strip())
            b_id = b_msg.get("id")
            # Prompt A resolves when steered prompt B arrives, then B streams updates while staying open until cancelled.
            print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"stopReason":"end_turn"}}), flush=True)
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking-b"}}}}), flush=True)
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"from-b"}}}}), flush=True)
            # Hold B open until session/cancel arrives so we prove B confirms BEFORE finishPrompt(B).
            for next_raw in sys.stdin:
                next_raw = next_raw.strip()
                if not next_raw:
                    continue
                next_msg = json.loads(next_raw)
                if next_msg.get("method") == "session/cancel":
                    print(json.dumps({"jsonrpc":"2.0","id":b_id,"result":{"stopReason":"cancelled"}}), flush=True)
                    break
    elif method == "session/close":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{}}), flush=True)
`
	bin := writeFakeACPServer(t, steerConfirmScript)
	conn, err := Dial(context.Background(), ConnConfig{
		Binary:       bin,
		WorkDir:      t.TempDir(),
		Reaper:       procutiltest.Reaper(),
		ProviderName: "agy",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ev := newEventCollector()
	sess, err := conn.Open(context.Background(), "t-steer-confirm", SessionConfig{
		WorkDir:     t.TempDir(),
		RuntimeMode: provider.RuntimeApprovalRequired,
	}, ev.record)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Send(context.Background(), "PROMPT_A", provider.SendOptions{UserMessageUUID: "uuid-a"}); err != nil {
		t.Fatalf("Send PROMPT_A: %v", err)
	}
	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventTextDelta && strings.Contains(e.Content, "from-a")
	})

	if err := sess.Send(context.Background(), "PROMPT_B", provider.SendOptions{UserMessageUUID: "uuid-b"}); err != nil {
		t.Fatalf("Send PROMPT_B: %v", err)
	}
	// Steered prompt B must confirm while B is STILL running (before EventTurnComplete).
	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventUserText && strings.Contains(string(e.Meta), "uuid-b")
	})
	for _, e := range ev.all() {
		if e.Kind == provider.EventTurnComplete {
			t.Fatalf("expected uuid-b confirmation before turn completion, but turn already completed")
		}
	}

	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventTurnComplete
	})
}

func TestACPEarlyReturningSteerStaysStagedUntilToolCompletesAndModelReplies(t *testing.T) {
	t.Parallel()
	// Models real agy 1.5.16 steering: Prompt A stays open running cmd-1 while
	// steered Prompt B returns {"stopReason":"end_turn"} immediately. Prompt B
	// must stay unconfirmed (staged above the composer) until cmd-1 finishes and
	// the model begins its next step reacting to Prompt B.
	earlySteerScript := `#!/usr/bin/env python3
import json, sys, time
for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    msg = json.loads(raw)
    mid = msg.get("id")
    method = msg.get("method")
    params = msg.get("params") or {}
    if method == "initialize":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"protocolVersion":1,"agentInfo":{"name":"mock","version":"1.0"}}}), flush=True)
    elif method == "session/new":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"sessionId":"s-early-steer","modes":{"currentModeId":"ask","availableModes":[{"id":"ask","name":"Ask"}]}}}), flush=True)
    elif method == "session/prompt":
        sid = params.get("sessionId")
        prompt = params.get("prompt") or []
        text = prompt[0].get("text", "") if prompt else ""
        if text == "PROMPT_A":
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"tool_call","toolCallId":"cmd-1","title":"run_command","kind":"execute","status":"in_progress"}}}), flush=True)
            b_msg = json.loads(sys.stdin.readline().strip())
            b_id = b_msg.get("id")
            # Prompt B returns immediately while cmd-1 is still running!
            print(json.dumps({"jsonrpc":"2.0","id":b_id,"result":{"stopReason":"end_turn"}}), flush=True)
            time.sleep(0.15)
            # Now cmd-1 finishes, and then the model starts replying to Prompt B.
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"tool_call_update","toolCallId":"cmd-1","title":"run_command","kind":"execute","status":"completed"}}}), flush=True)
            print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"reply-to-b"}}}}), flush=True)
            print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"stopReason":"end_turn"}}), flush=True)
    elif method == "session/close":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{}}), flush=True)
`
	bin := writeFakeACPServer(t, earlySteerScript)
	conn, err := Dial(context.Background(), ConnConfig{
		Binary:       bin,
		WorkDir:      t.TempDir(),
		Reaper:       procutiltest.Reaper(),
		ProviderName: "agy",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ev := newEventCollector()
	sess, err := conn.Open(context.Background(), "t-early-steer", SessionConfig{
		WorkDir:     t.TempDir(),
		RuntimeMode: provider.RuntimeApprovalRequired,
	}, ev.record)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Send(context.Background(), "PROMPT_A", provider.SendOptions{UserMessageUUID: "uuid-a"}); err != nil {
		t.Fatalf("Send PROMPT_A: %v", err)
	}
	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventToolStart && e.ItemID == "cmd-1"
	})

	if err := sess.Steer(context.Background(), "PROMPT_B", provider.SendOptions{UserMessageUUID: "uuid-b"}); err != nil {
		t.Fatalf("Steer PROMPT_B: %v", err)
	}

	ev.waitFor(t, 5*time.Second, func(e provider.ProviderEvent) bool {
		return e.Kind == provider.EventTurnComplete
	})

	all := ev.all()
	cmdCompleteIdx := -1
	uuidBIdx := -1
	replyBIdx := -1
	for i, e := range all {
		if e.Kind == provider.EventToolComplete && e.ItemID == "cmd-1" && cmdCompleteIdx < 0 {
			cmdCompleteIdx = i
		}
		if e.Kind == provider.EventUserText && strings.Contains(string(e.Meta), "uuid-b") && uuidBIdx < 0 {
			uuidBIdx = i
		}
		if e.Kind == provider.EventTextDelta && strings.Contains(e.Content, "reply-to-b") && replyBIdx < 0 {
			replyBIdx = i
		}
	}
	if cmdCompleteIdx < 0 || uuidBIdx < 0 || replyBIdx < 0 {
		t.Fatalf("missing expected events: cmdComplete=%d uuidB=%d replyB=%d; all=%+v", cmdCompleteIdx, uuidBIdx, replyBIdx, all)
	}
	if cmdCompleteIdx >= uuidBIdx || uuidBIdx >= replyBIdx {
		t.Fatalf("steered prompt B confirmed out of order: want cmdComplete (%d) < uuidB (%d) < replyB (%d)", cmdCompleteIdx, uuidBIdx, replyBIdx)
	}
}
