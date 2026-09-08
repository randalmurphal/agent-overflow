package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// Exercise both model bindings against real pipe-speaking mock processes. A
// session-map presence check alone passes even when a picker kills and replaces
// the process; identity, the wire write, and the live tray must all survive.
func TestModelSelectionPreservesSessionAndBackgroundWork(t *testing.T) {
	for _, providerName := range []string{"claude", "codex"} {
		for _, activeTurn := range []bool{false, true} {
			t.Run(providerName+"/active="+strconv.FormatBool(activeTurn), func(t *testing.T) {
				app := newTestAppWithStore(t)
				app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
				thread := testThread("model-background")
				thread.Provider = providerName
				thread.WorkspacePath = t.TempDir()
				thread.Model = "claude-sonnet-5"
				nextModel := "claude-fable-5"
				if providerName == "codex" {
					thread.Model = "gpt-5.4"
					thread.ContextWindow = provider.CodexStandardContextWindow
					nextModel = "gpt-5.4-mini"
				}
				thread.ReasoningEffort = "high"
				if err := app.store.CreateThread(thread); err != nil {
					t.Fatal(err)
				}
				thread, _ = app.store.GetThread(thread.ID)
				if err := app.store.UpsertChatModelProfile(store.ChatModelProfile{
					Provider: providerName, Model: nextModel, ReasoningEffort: thread.ReasoningEffort,
					ContextWindow: thread.ContextWindow, RuntimeMode: thread.RuntimeMode,
				}); err != nil {
					t.Fatal(err)
				}
				opts, err := app.buildSessionOptions(app.sanitizeThreadModelSettings(thread))
				if err != nil {
					t.Fatal(err)
				}
				binary, capture := modelSwitchMockBinary(t)
				entry := session{Provider: providerName, Token: "original", LaunchOptions: opts, Liveness: newSessionLiveness(time.Now())}
				if activeTurn {
					entry.Liveness.ActiveTurns.Store(1)
				}
				if providerName == "claude" {
					cfg := claude.ConfigFromOptions(opts)
					cfg.Binary = binary
					entry.Claude, err = claude.NewSession(context.Background(), thread.ID, cfg, func(provider.ProviderEvent) {})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = entry.Claude.Close() })
				} else {
					cfg := codex.ConfigFromOptions(opts)
					cfg.Binary = binary
					entry.Codex, err = codex.NewSession(context.Background(), thread.ID, cfg, func(provider.ProviderEvent) {})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = entry.Codex.Close() })
				}
				app.sessionManager().put(thread.ID, entry)
				seedBackgroundLaunchRowE2E(t, app.store, thread.ID, "running-job", "Bash", "long job")
				starts := make(chan string, 4)
				app.startSessionFn = func(id string) error { starts <- id; return nil }

				assertPreserved := func() {
					t.Helper()
					current, ok := app.sessionManager().get(thread.ID)
					if !ok || current.Token != entry.Token || current.Claude != entry.Claude || current.Codex != entry.Codex {
						t.Fatal("model switch replaced session")
					}
					if current.LaunchOptions.Model != nextModel {
						t.Fatalf("session model = %q", current.LaunchOptions.Model)
					}
					if app.sessionManager().runtime.PendingConfigReconnect(thread.ID) {
						t.Fatal("live model switch scheduled restart")
					}
					select {
					case <-starts:
						t.Fatal("model switch restarted session")
					default:
					}
					rows, err := app.ListLiveBackgroundTasks(thread.ID)
					if err != nil || len(rows) != 1 || rows[0].ID != "running-job" || rows[0].Status != "running" {
						t.Fatalf("tray lost live job: %+v, %v", rows, err)
					}
				}
				if _, err := app.UpdateThreadModelSelection(thread.ID, providerName, nextModel); err != nil {
					t.Fatal(err)
				}
				assertPreserved()
				for i, selectModel := range []func() error{
					func() error { _, err := app.UpdateThreadModelSelection(thread.ID, providerName, nextModel); return err },
					func() error { _, err := app.UpdateThreadModel(thread.ID, nextModel); return err },
				} {
					meta, _ := json.Marshal(map[string]string{"fallbackModel": thread.Model})
					if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventModelFallback, ThreadID: thread.ID, ItemID: string(rune('a' + i)), Meta: meta}); err != nil {
						t.Fatal(err)
					}
					if err := selectModel(); err != nil {
						t.Fatal(err)
					}
					assertPreserved()
					if app.hasModelFallback(thread.ID) {
						t.Fatal("accepted model retry left stale fallback projection")
					}
				}
				// Claude accepts live set_model even mid-turn. Codex pushes while idle;
				// active turns already have the next selection in their turn config.
				method := `"set_model"`
				if providerName == "codex" {
					method = `"thread/settings/update"`
				}
				writes := 0
				for _, line := range capture.Lines(t) {
					if strings.Contains(line, method) && strings.Contains(line, nextModel) {
						writes++
					}
				}
				want := 3
				if providerName == "codex" && activeTurn {
					want = 0
				}
				if writes != want {
					t.Fatalf("model writes = %d, want %d: %v", writes, want, capture.Lines(t))
				}
			})
		}
	}
}

func modelSwitchMockBinary(t *testing.T) (string, *backgroundScriptCapture) {
	t.Helper()
	capture := newBackgroundScriptCapture(t)
	script := `#!/bin/bash
while IFS= read -r line; do
 printf '%s\n' "$line" >> ` + shellSingleQuoteForBackground(capture.capturePath) + `
 case "$line" in
  *'"request_id"'*)
   reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
   printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
   ;;
  *'"id"'*)
   reqid=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
   printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"native-model-background"}}}\n' "$reqid"
   ;;
 esac
done
`
	binary := filepath.Join(t.TempDir(), "mock-provider")
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return binary, capture
}

func TestDeferredConfigReconnectRechecksWorkAfterLiveApply(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("config-became-busy")
	thread.Provider, thread.Model = "claude", "claude-fable-5"
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	var err error
	thread, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := app.buildSessionOptions(app.sanitizeThreadModelSettings(thread))
	if err != nil {
		t.Fatal(err)
	}
	opts.Model = "claude-sonnet-5"
	binary, _ := modelSwitchMockBinary(t)
	script, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	// The read loop delivers init synchronously before the control failure,
	// making the session busy inside the live apply without sleeps or polling.
	script = []byte(strings.Replace(string(script),
		`   printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"`,
		`   printf '%s\n' '{"type":"system","subtype":"init","session_id":"became-busy","model":"claude-sonnet-5"}'
   printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"refused"}}\n' "$reqid"`, 1))
	if err := os.WriteFile(binary, script, 0700); err != nil {
		t.Fatal(err)
	}
	live := newSessionLiveness(time.Now().Add(-time.Minute))
	cfg := claude.ConfigFromOptions(opts)
	cfg.Binary = binary
	sess, err := claude.NewSession(context.Background(), thread.ID, cfg, func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventInit {
			live.ActiveTurns.Store(1)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Provider: "claude", Token: "busy-token", Claude: sess, LaunchOptions: opts, Liveness: live})
	restarted := false
	app.startSessionFn = func(string) error { restarted = true; return nil }
	unlock := app.threadLocks().Lock(thread.ID)
	done := app.fireDeferredConfigReconnectLocked(thread.ID)
	unlock()
	if live.ActiveTurns.Load() != 1 {
		t.Fatal("mock did not start work during the control request")
	}
	if done || restarted {
		t.Fatal("deferred reconnect killed work that arrived during live apply")
	}
}
