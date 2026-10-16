package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil/mockexec"
)

// writeCodexInstructionsAppServer is a mock app-server that logs every
// request to requestLog and answers the requests a session start, a fork
// and an injection make: thread/start mints "codex-root", thread/resume
// echoes the thread it names, thread/fork mints an id unique to the
// request, and thread/turns/list reports the fork's cut as its tail.
func writeCodexInstructionsAppServer(t *testing.T, requestLog string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
tail=""
while IFS= read -r line; do
    /bin/echo "$line" >> '%s'
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
    elif /bin/echo "$line" | /usr/bin/grep -q '"method":"config/read"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"config":{}}}\n' "$id"
    elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"codex-root"}}}\n' "$id"
    elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        thread=$(/bin/echo "$line" | /usr/bin/grep -o '"threadId":"[^"]*"' | /usr/bin/cut -d'"' -f4)
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%%s"}}}\n' "$id" "$thread"
    elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/inject_items"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
    elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/fork"'; then
        tail=$(/bin/echo "$line" | /usr/bin/grep -o '"lastTurnId":"[^"]*"' | /usr/bin/cut -d'"' -f4)
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"codex-fork-%%s-%%s","turns":[]}}}\n' "$id" "$$" "$id"
    elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/turns/list"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"data":[{"id":"%%s"}],"nextCursor":null}}\n' "$id" "$tail"
    fi
done
`, requestLog)
	path := filepath.Join(t.TempDir(), "codex-instructions.sh")
	mockexec.Write(t, path, script)
	return path
}

// codexInjections returns the developer text of every thread/inject_items
// request the mock received for codexThreadID, in order.
func codexInjections(t *testing.T, requestLog, codexThreadID string) []string {
	t.Helper()
	data, err := os.ReadFile(requestLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read request log: %v", err)
	}
	var texts []string
	for _, line := range strings.Split(string(data), "\n") {
		var req struct {
			Method string `json:"method"`
			Params struct {
				ThreadID string `json:"threadId"`
				Items    []struct {
					Role    string `json:"role"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"items"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &req) != nil || req.Method != "thread/inject_items" || req.Params.ThreadID != codexThreadID {
			continue
		}
		if len(req.Params.Items) != 1 || req.Params.Items[0].Role != "developer" || len(req.Params.Items[0].Content) != 1 {
			t.Fatalf("inject request = %s, want one developer message", line)
		}
		texts = append(texts, req.Params.Items[0].Content[0].Text)
	}
	return texts
}

// startCodexSessionAndWait starts threadID's session and waits for its init,
// which the session emits after any injection.
func startCodexSessionAndWait(t *testing.T, app *App, bus *capturedEventBus, threadID string) {
	t.Helper()
	if err := app.StartSession(threadID); err != nil {
		t.Fatalf("StartSession(%s): %v", threadID, err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 10*time.Second)
}

func awaitSessionRef(t *testing.T, app *App, threadID, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		thread, err := app.store.GetThread(threadID)
		if err != nil {
			t.Fatalf("GetThread(%s): %v", threadID, err)
		}
		if thread.SessionRef == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread %s session ref = %q, want %q", threadID, thread.SessionRef, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A resumed Codex thread never shows a changed developerInstructions to the
// model, so a guide change between two sessions of one thread reaches it as
// one injected developer message, and only once.
func TestCodexResumeInjectsAChangedGuideOnce(t *testing.T) {
	t.Parallel()
	app, bus := setupE2EApp(t)
	requestLog := filepath.Join(t.TempDir(), "requests.jsonl")
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath":   writeCodexInstructionsAppServer(t, requestLog),
		"agentGuideEnabled": true,
	}); err != nil {
		t.Fatal(err)
	}
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}

	// A fresh thread reads the guide in its initial context: nothing is
	// injected and the text is recorded.
	startCodexSessionAndWait(t, app, bus, thread.ID)
	awaitSessionRef(t, app, thread.ID, "codex-root")
	if got := codexInjections(t, requestLog, "codex-root"); len(got) != 0 {
		t.Fatalf("a fresh start injected %q", got)
	}
	first, ok, err := app.store.CodexDeveloperInstructionsDigest(thread.ID, "codex-root")
	if err != nil || !ok {
		t.Fatalf("after a fresh start the digest is (%v, %v), want it recorded", ok, err)
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatal(err)
	}

	// The guide switched off between sessions: the resume injects the text
	// without it.
	if _, err := app.settings.Update(map[string]any{"agentGuideEnabled": false}); err != nil {
		t.Fatal(err)
	}
	startCodexSessionAndWait(t, app, bus, thread.ID)
	injected := codexInjections(t, requestLog, "codex-root")
	if len(injected) != 1 || strings.Contains(injected[0], agentGuideRendering) {
		t.Fatalf("injections after the guide was switched off = %q, want one without the guide", injected)
	}
	second, _, _ := app.store.CodexDeveloperInstructionsDigest(thread.ID, "codex-root")
	if second == first {
		t.Fatal("the recorded digest did not follow the injection")
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatal(err)
	}

	// Unchanged since: nothing more. Switched back on: the guide returns.
	startCodexSessionAndWait(t, app, bus, thread.ID)
	if got := codexInjections(t, requestLog, "codex-root"); len(got) != 1 {
		t.Fatalf("an unchanged text was injected again: %q", got)
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.settings.Update(map[string]any{"agentGuideEnabled": true}); err != nil {
		t.Fatal(err)
	}
	startCodexSessionAndWait(t, app, bus, thread.ID)
	injected = codexInjections(t, requestLog, "codex-root")
	if len(injected) != 2 || !strings.Contains(injected[1], agentGuideRendering) {
		t.Fatalf("injections after the guide came back = %q, want a second one carrying it", injected)
	}
}

// A thread with no record (one that predates the record) injects once at
// its next start, and the record then outlives the app: a new App over
// the same database injects nothing.
func TestCodexThreadWithoutARecordInjectsOnceAndTheRecordSurvivesARestart(t *testing.T) {
	t.Parallel()
	dbPath, settingsDir := storetest.ClonePath(t), t.TempDir()
	requestLog := filepath.Join(t.TempDir(), "requests.jsonl")
	binary := writeCodexInstructionsAppServer(t, requestLog)

	app, bus := setupE2EAppOn(t, dbPath, settingsDir)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.UpdateSessionRef(thread.ID, "codex-existing"); err != nil {
		t.Fatal(err)
	}
	startCodexSessionAndWait(t, app, bus, thread.ID)
	if got := codexInjections(t, requestLog, "codex-existing"); len(got) != 1 {
		t.Fatalf("injections for a thread with no record = %q, want one", got)
	}
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatal(err)
	}

	restarted, restartedBus := setupE2EAppOn(t, dbPath, settingsDir)
	if _, err := restarted.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	startCodexSessionAndWait(t, restarted, restartedBus, thread.ID)
	if got := codexInjections(t, requestLog, "codex-existing"); len(got) != 1 {
		t.Fatalf("injections after a restart = %q, want the one from before", got)
	}
}

// A fork child starts with no record, whether or not the fork cut history:
// a cut can drop a message its source received after the cut. Its first
// start injects once, and its next start nothing.
func TestCodexForkChildInjectsOnce(t *testing.T) {
	t.Parallel()
	app, bus := setupE2EApp(t)
	requestLog := filepath.Join(t.TempDir(), "requests.jsonl")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": writeCodexInstructionsAppServer(t, requestLog)}); err != nil {
		t.Fatal(err)
	}
	source, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}
	startCodexSessionAndWait(t, app, bus, source.ID)
	awaitSessionRef(t, app, source.ID, "codex-root")
	if err := app.StopSession(source.ID); err != nil {
		t.Fatal(err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"provider-user-1"}`)
	insertCodexTurn(t, app.store, source.ID, 0, "turn-0")
	insertCodexTurn(t, app.store, source.ID, 1, "turn-1")

	atTurn := 0
	for _, tc := range []struct {
		name string
		at   *int
	}{
		{"whole history", nil},
		{"cut history", &atTurn},
	} {
		forked, err := app.ForkThread(t.Context(), source.ID, tc.at)
		if err != nil {
			t.Fatalf("%s: ForkThread: %v", tc.name, err)
		}
		if !strings.HasPrefix(forked.SessionRef, "codex-fork-") {
			t.Fatalf("%s: fork session ref = %q", tc.name, forked.SessionRef)
		}
		if _, ok, _ := app.store.CodexDeveloperInstructionsDigest(forked.ID, forked.SessionRef); ok {
			t.Fatalf("%s: the fork child started with a record", tc.name)
		}
		startCodexSessionAndWait(t, app, bus, forked.ID)
		if got := codexInjections(t, requestLog, forked.SessionRef); len(got) != 1 || !strings.Contains(got[0], agentGuideRendering) {
			t.Fatalf("%s: first-start injections = %q, want one carrying the composed text", tc.name, got)
		}
		if err := app.StopSession(forked.ID); err != nil {
			t.Fatal(err)
		}
		startCodexSessionAndWait(t, app, bus, forked.ID)
		if got := codexInjections(t, requestLog, forked.SessionRef); len(got) != 1 {
			t.Fatalf("%s: injections after a second start = %q, want still one", tc.name, got)
		}
		if err := app.StopSession(forked.ID); err != nil {
			t.Fatal(err)
		}
	}
}
