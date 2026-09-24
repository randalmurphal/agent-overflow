package harnessrpc

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/forgefake"
)

func TestHarnessForgeSeedIsStrictAndPositioned(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	_, err := h.HarnessForgeSeed(json.RawMessage("{\n  \"repos\": [{\"forge\": \"github\", \"project\": \"a/b\", \"pullz\": []}]\n}"))
	if err == nil || !strings.Contains(err.Error(), "pullz") || !strings.Contains(err.Error(), "at line ") {
		t.Fatalf("unknown field error = %v, want the field and its position", err)
	}
	if _, err := h.HarnessForgeSeed(json.RawMessage(`{"repos":[{"forge":"github","project":"a/b"}]} {}`)); err == nil {
		t.Fatal("a trailing document was accepted")
	}
	if _, err := h.HarnessForgeSeed(json.RawMessage(`{"repos":[{"forge":"svn","project":"a/b"}]}`)); err == nil {
		t.Fatal("an invalid fixture was accepted")
	}
}

// The whole harness-side path: a forwarded call reaches the engine
// through the control server StartControl builds, is recorded, fans out
// as harness:forge, and HarnessReset clears both the state and the log.
func TestForgeCallsReachTheSeededFixtureThroughControl(t *testing.T) {
	h, host := newHarnessTestHost(t)
	var mu sync.Mutex
	var events []forgefake.Invocation
	host.emit = func(channel eventchan.Channel, data any) {
		if channel == eventchan.HarnessForge {
			mu.Lock()
			events = append(events, data.(forgefake.Invocation))
			mu.Unlock()
		}
	}
	controlServer, env, err := StartControl(h)
	if err != nil {
		t.Fatalf("StartControl: %v", err)
	}
	t.Cleanup(controlServer.Shutdown)
	t.Setenv(control.EnvAddr, env[control.EnvAddr])
	t.Setenv(control.EnvToken, env[control.EnvToken])
	client, ok := control.FromEnv()
	if !ok {
		t.Fatal("no control env")
	}

	seeded, err := h.HarnessForgeSeed(json.RawMessage(`{"repos":[{"forge":"github","project":"acme/forge-rpc",
		"pulls":[{"number":4,"title":"Seeded","comments":[{"body":"hi"}]}]}]}`))
	if err != nil {
		t.Fatalf("HarnessForgeSeed: %v", err)
	}
	if seeded.Repos[0].Pulls[0].Comments[0].ID == 0 {
		t.Fatal("seed did not return the generated comment id")
	}

	view := []string{"pr", "view", "--repo", "acme/forge-rpc", "4", "--json", "title"}
	result, err := client.Forge(control.ForgeCall{CLI: "gh", Args: view, Cwd: "/ws"})
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(string(result.Stdout)) != `{"title":"Seeded"}` {
		t.Fatalf("forge call = %+v, %v", result, err)
	}
	log := h.HarnessForgeInvocations(0)
	if len(log.Invocations) != 1 || log.Invocations[0].Route != "gh pr view" || log.Invocations[0].Cwd != "/ws" {
		t.Fatalf("invocations = %+v", log)
	}
	mu.Lock()
	if len(events) != 1 || events[0].Seq != log.Invocations[0].Seq {
		t.Fatalf("harness:forge events = %+v", events)
	}
	mu.Unlock()

	if err := h.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	if log := h.HarnessForgeInvocations(0); len(log.Invocations) != 0 {
		t.Fatalf("reset kept the invocation log: %+v", log)
	}
	result, err = client.Forge(control.ForgeCall{CLI: "gh", Args: view})
	if err != nil || result.ExitCode == 0 {
		t.Fatalf("the seeded pull survived reset: %+v, %v", result, err)
	}
}
