package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// writeDeveloperInstructionsAppServer is a mock app-server that answers
// `config/read` with the given cwd-scoped developer instructions and every
// other request with a thread. Requests are appended to requestLog.
func writeDeveloperInstructionsAppServer(t *testing.T, dir, requestLog, configured string) string {
	t.Helper()
	if strings.ContainsAny(configured, "'\n") {
		t.Fatalf("configured instructions %q cannot ride this single-quoted shell script", configured)
	}
	configured = strings.ReplaceAll(configured, `"`, `\"`)
	script := "#!/bin/bash\n" +
		"while IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> '" + requestLog + "'\n" +
		"  id=$(printf '%s' \"$line\" | grep -o '\"id\":[0-9]*' | head -1 | grep -o '[0-9]*')\n" +
		"  if [ -z \"$id\" ]; then continue; fi\n" +
		"  if printf '%s' \"$line\" | grep -q '\"method\":\"config/read\"'; then\n" +
		"    printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"config\":{\"developer_instructions\":\"" + configured + "\"}}}\\n' \"$id\"\n" +
		"  else\n" +
		"    printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"thread\":{\"id\":\"mock-thread\"}}}\\n' \"$id\"\n" +
		"  fi\n" +
		"done\n"
	binary := filepath.Join(dir, "codex")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock app-server: %v", err)
	}
	return binary
}

// TestThreadStartCarriesTheComposedDeveloperInstructions is the delivery
// contract for the thread tools' decision guide on Codex. Codex resolves
// developer instructions from CONFIG on a cold start with no history
// fallback, so an override naming only AO's guide would silently drop the
// user's own value: the session reads the cwd's configured text first and
// appends.
func TestThreadStartCarriesTheComposedDeveloperInstructions(t *testing.T) {
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeDeveloperInstructionsAppServer(t, dir, requestLog, "configured cwd instructions")

	sess, err := NewSession(context.Background(), "thread-1", Config{
		Binary:                binary,
		WorkDir:               dir,
		DeveloperInstructions: "AO thread tools guide",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// The cwd's value is read before the thread is started, and it is the
	// thread's own cwd that is asked about.
	read := findRequestParams(t, requestLog, "config/read")
	if read["cwd"] != dir {
		t.Errorf("config/read cwd = %v, want %s", read["cwd"], dir)
	}
	if read["includeLayers"] != false {
		t.Errorf("config/read includeLayers = %v, want false", read["includeLayers"])
	}

	params := findRequestParams(t, requestLog, "thread/start")
	got, _ := params["developerInstructions"].(string)
	if got != "configured cwd instructions\n\nAO thread tools guide" {
		t.Fatalf("developerInstructions = %q, want the configured value with the guide appended", got)
	}
}

// A cwd with no configured developer instructions carries the guide alone.
func TestThreadStartCarriesTheGuideAloneWhenNothingIsConfigured(t *testing.T) {
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeDeveloperInstructionsAppServer(t, dir, requestLog, "")

	sess, err := NewSession(context.Background(), "thread-1", Config{
		Binary:                binary,
		WorkDir:               dir,
		DeveloperInstructions: "AO thread tools guide",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	params := findRequestParams(t, requestLog, "thread/start")
	if got := params["developerInstructions"]; got != "AO thread tools guide" {
		t.Fatalf("developerInstructions = %v, want the guide alone", got)
	}
}

// With no guide to deliver there is no override at all: the field is
// omitted rather than sent empty, which would replace the user's
// configured instructions with nothing.
func TestThreadStartOmitsDeveloperInstructionsWithoutAGuide(t *testing.T) {
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeDeveloperInstructionsAppServer(t, dir, requestLog, "configured cwd instructions")

	sess, err := NewSession(context.Background(), "thread-1", Config{Binary: binary, WorkDir: dir}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	params := findRequestParams(t, requestLog, "thread/start")
	if _, present := params["developerInstructions"]; present {
		t.Fatalf("thread/start carried developerInstructions with no guide: %v", params["developerInstructions"])
	}
	// And nothing was read either: there is nothing to compose with.
	data, err := os.ReadFile(requestLog)
	if err != nil {
		t.Fatalf("read request log: %v", err)
	}
	if strings.Contains(string(data), `"config/read"`) {
		t.Error("a session with no guide still probed config/read")
	}
}

// buildThreadParams is what thread/start and thread/resume share, so the
// field's presence rule is pinned on the shared builder too.
func TestBuildThreadParamsCarriesDeveloperInstructionsOnlyWhenSet(t *testing.T) {
	params := buildThreadParams(Config{WorkDir: "/tmp/x", DeveloperInstructions: "guide"}, "0.153.4")
	if params["developerInstructions"] != "guide" {
		t.Errorf("developerInstructions = %v, want the composed text", params["developerInstructions"])
	}
	params = buildThreadParams(Config{WorkDir: "/tmp/x"}, "0.153.4")
	if _, present := params["developerInstructions"]; present {
		t.Error("an empty value still reached the wire")
	}
}

// A fork is a new thread and resolves developer instructions from config
// like any cold start, so the parent's composed value has to ride along or
// the child loses the guide.
func TestThreadForkCarriesTheParentsDeveloperInstructions(t *testing.T) {
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeDeveloperInstructionsAppServer(t, dir, requestLog, "configured cwd instructions")

	sess, err := NewSession(context.Background(), "thread-1", Config{
		Binary:                binary,
		WorkDir:               dir,
		DeveloperInstructions: "AO thread tools guide",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	if _, err := sess.Fork(context.Background()); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	params := findRequestParams(t, requestLog, "thread/fork")
	want := "configured cwd instructions\n\nAO thread tools guide"
	if got, _ := params["developerInstructions"].(string); got != want {
		t.Fatalf("fork developerInstructions = %q, want %q", got, want)
	}
}

// The per-turn collaboration-mode settings carry their own
// developer_instructions, and AO sends null there. That null is a
// different field from the thread-level value (codex-rs 0.153.4:
// TurnContext.developer_instructions comes from the session configuration,
// while collaboration mode's feeds its own world-state section), and the
// per-turn override struct has no developer-instructions field at all. A
// build that started sending one there would clobber the guide, so the
// shape is pinned here.
func TestCollaborationModeSettingsCarryNoThreadLevelDeveloperInstructions(t *testing.T) {
	mode := codexCollaborationMode(provider.ModeChat, "gpt-5", "high")
	raw, err := json.Marshal(mode["settings"])
	if err != nil {
		t.Fatalf("marshal collaboration mode settings: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode collaboration mode settings: %v", err)
	}
	value, present := decoded["developer_instructions"]
	if !present {
		t.Skip("collaboration-mode settings no longer carry the field")
	}
	if value != nil {
		t.Fatalf("collaboration mode settings developer_instructions = %v, want null "+
			"— a non-null value here is the mode's own instructions, not the thread's", value)
	}
}
