package codex

import (
	"context"
	"path/filepath"
	"testing"

	"agent-overflow/internal/provider"
)

func TestAdditionalInstructionsPreserveNativeConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	capture := filepath.Join(t.TempDir(), "requests.ndjson")
	binary := queueFakeScript(t, "codex/0.153.4", "native-thread", capture, map[string]string{"config/read": `{"config":{"developer_instructions":"Native project instructions"}}`})
	s := newQueueSession(t, binary, func(provider.ProviderEvent) {})
	got, err := s.appendDeveloperInstructions(context.Background(), "AO commands")
	if err != nil || got != "Native project instructions\n\nAO commands" {
		t.Fatalf("instructions: %q %v", got, err)
	}
	requests := capturedRequestParams(t, capture, "config/read")
	if len(requests) != 1 || requests[0]["includeLayers"] != false {
		t.Fatalf("config reads: %#v", requests)
	}
	opts := provider.SessionOptions{SystemPrompt: "primary", AdditionalInstructions: "AO commands"}
	params := buildThreadParams(ConfigFromOptions(opts), "0.153.4")
	if params["baseInstructions"] != "primary" || params["developerInstructions"] != "AO commands" {
		t.Fatalf("thread params: %#v", params)
	}
	next := opts
	next.AdditionalInstructions = ""
	if _, live := PlanLiveUpdate(opts, next); live {
		t.Fatal("changing developer instructions must restart at the existing safe boundary")
	}
	if _, present := buildThreadParams(ConfigFromOptions(next), "0.153.4")["developerInstructions"]; present {
		t.Fatal("disabled feature overrides native instructions")
	}
}
