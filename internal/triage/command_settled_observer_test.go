package triage

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

type commandSettledLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *commandSettledLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func observeSettledCommands(router *Router, thread string) *commandSettledLog {
	log := &commandSettledLog{}
	router.SetCommandSettledObserver(func(threadID, command string) {
		log.mu.Lock()
		defer log.mu.Unlock()
		if threadID == thread {
			log.calls = append(log.calls, command)
		}
	})
	return log
}

// A Claude Bash call reports its command once its result settles the row,
// a failed one included, and not while it runs.
func TestCommandSettledObserverClaudeBash(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t-cmd")
	settled := observeSettledCommands(router, "t-cmd")
	parser := claude.NewParser()
	route := func(line string) {
		t.Helper()
		events, err := parser.ParseLine("t-cmd", []byte(line))
		if err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		for _, evt := range events {
			if err := router.Handle(evt); err != nil {
				t.Fatalf("route %s: %v", evt.Kind, err)
			}
		}
	}
	use := func(msg, id, command string) string {
		return `{"type":"assistant","message":{"id":"` + msg + `","role":"assistant","model":"claude-opus-5-5","content":[` +
			`{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":` + mustEncodeJSONString(command) + `}}]}}`
	}
	result := func(id, content string, isError bool) string {
		errJSON := "false"
		if isError {
			errJSON = "true"
		}
		return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
			`","content":` + mustEncodeJSONString(content) + `,"is_error":` + errJSON + `}]}}`
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t-cmd", Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	route(use("msg-1", "toolu_create", `gh pr create --title "Add x" --body ""`))
	if got := settled.snapshot(); len(got) != 0 {
		t.Fatalf("a running command reported %q", got)
	}
	route(result("toolu_create", "https://github.com/acme/tool/pull/7", false))
	route(use("msg-2", "toolu_dup", `glab mr create --title x --yes`))
	route(result("toolu_dup", "failed to create merge request: 409 Another open merge request already exists", true))
	route(use("msg-3", "toolu_read", `README.md`))

	got := settled.snapshot()
	want := []string{`gh pr create --title "Add x" --body ""`, `glab mr create --title x --yes`}
	if len(got) < len(want) {
		t.Fatalf("settled commands = %q, want %q", got, want)
	}
	seen := map[string]bool{}
	for _, command := range got {
		seen[command] = true
	}
	for _, command := range want {
		if !seen[command] {
			t.Fatalf("settled commands = %q, missing %q", got, command)
		}
	}
}

// A Codex command execution, classified from the app-server's own
// notifications, reports its command when it completes.
func TestCommandSettledObserverCodexCommandExecution(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexTestThread(t, st, "t-codex")
	settled := observeSettledCommands(router, "t-codex")
	const command = "/usr/bin/zsh -lc 'glab mr create --fill --yes'"
	notifications := []struct{ method, params string }{
		{"turn/started", `{"turn":{"id":"turn-1","status":"inProgress","items":[]}}`},
		{"item/started", `{"turnId":"turn-1","item":{"id":"cmd-1","type":"commandExecution","status":"inProgress","command":` + mustEncodeJSONString(command) + `,"cwd":"/repo","commandActions":[]}}`},
		{"item/completed", `{"turnId":"turn-1","item":{"id":"cmd-1","type":"commandExecution","status":"completed","command":` + mustEncodeJSONString(command) + `,"cwd":"/repo","commandActions":[],"exitCode":0,"aggregatedOutput":"https://gitlab.com/acme/tool/-/merge_requests/4\n"}}`},
	}
	for i, n := range notifications {
		for _, evt := range codex.ClassifyNotification("t-codex", n.method, json.RawMessage(n.params)) {
			if err := router.Handle(evt); err != nil {
				t.Fatalf("handle %s: %v", n.method, err)
			}
		}
		if got := settled.snapshot(); i < 2 && len(got) != 0 {
			t.Fatalf("a running command reported %q", got)
		}
	}
	got := settled.snapshot()
	if len(got) == 0 || got[len(got)-1] != command {
		t.Fatalf("settled commands = %q, want %q", got, command)
	}
}
