package aocli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/remotejobs"
	"github.com/google/uuid"
)

func TestRemoteRunPreservesArgvAndPublishesRetryID(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("AgentRemoteStart", map[string]string{"state": "running"})
	id, computer, project := uuid.NewString(), uuid.NewString(), uuid.NewString()
	argv := []string{"bash", "-lc", "printf '%s' '$(no interpolation here)'", "--computer", "still-command-argv"}
	args := append([]string{"remote", "run", "--computer", computer, "--project", project, "--id", id, "--timeout", "120", "--"}, argv...)
	code, out, errOut := runCLI(args, backend.env())
	if code != exitOK || !strings.Contains(out, "running") || !strings.Contains(errOut, id) {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	calls := backend.recorded("AgentRemoteStart")
	if len(calls) != 1 {
		t.Fatalf("calls: %v", calls)
	}
	var input struct {
		ComputerID string             `json:"computerId"`
		Request    remotejobs.Request `json:"request"`
	}
	if err := json.Unmarshal(calls[0].Params[0], &input); err != nil {
		t.Fatal(err)
	}
	if input.ComputerID != computer || input.Request.ID != id || input.Request.SourceThreadID != "" || !reflect.DeepEqual(input.Request.Argv, argv) || input.Request.TimeoutSeconds != 120 {
		t.Fatalf("input: %#v", input)
	}
}

func TestRemoteHelpNeedsNoSessionAndReadCommandsKeepTarget(t *testing.T) {
	code, out, stderr := runCLI([]string{"remote", "--help"}, nil)
	if code != exitOK || !strings.Contains(out, "--id") {
		t.Fatalf("help: %d %s %s", code, out, stderr)
	}
	backend := newFakeBackend(t)
	backend.reply("AgentRemoteComputers", []any{})
	if code, _, err := runCLI([]string{"remote", "list"}, backend.env()); code != exitOK {
		t.Fatal(err)
	}
	for _, verb := range []string{"status", "cancel"} {
		method := "AgentRemoteStatus"
		if verb == "cancel" {
			method = "AgentRemoteCancel"
		}
		backend.reply(method, map[string]string{"state": "canceled"})
		if code, _, err := runCLI([]string{"remote", verb, "--computer", "gpu", "job"}, backend.env()); code != exitOK {
			t.Fatal(err)
		}
		calls := backend.recorded(method)
		if len(calls) != 1 || string(calls[0].Params[0]) != `"gpu"` || string(calls[0].Params[1]) != `"job"` {
			t.Fatalf("calls: %v", calls)
		}
	}
}
