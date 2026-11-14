package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/accountswitch"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/usermessage"
)

// installUsageLimitClaude gives the app a mock Claude that echoes each user
// message as --replay-user-messages does, refuses the first turn for a usage
// limit (the usage-limit-claude scenario's wire lines) and answers the rest.
// It returns the file every user message is logged to.
func installUsageLimitClaude(t *testing.T, app *App) string {
	t.Helper()
	dir := t.TempDir()
	stdinLog := filepath.Join(dir, "stdin.log")
	script := `#!/bin/bash
replay() { printf '{"isReplay":true,%s\n' "${1#\{}"; }
n=0
while IFS= read -r line; do
  case "$line" in
    *'"type":"control_request"'*)
      reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
      ;;
    *'"type":"user"'*)
      printf '%s\n' "$line" >> ` + shellQuoteForTest(stdinLog) + `
      n=$((n + 1))
      if [ "$n" -eq 1 ]; then
        printf '%s\n' '` + mockClaudeInitLine + `'
        replay "$line"
        printf '%s\n' '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1798761600,"rateLimitType":"five_hour","isUsingOverage":false}}'
        printf '%s\n' '{"type":"assistant","error":"rate_limit","message":{"id":"msg-limit","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"Claude usage limit reached."}]},"session_id":"sess-request"}'
        printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"result":"Claude usage limit reached.","session_id":"sess-request"}'
      else
        replay "$line"
        printf '{"type":"assistant","message":{"id":"msg-resumed-%s","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"resumed"}]}}\n' "$n"
        printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
      fi
      ;;
  esac
done
`
	binary := filepath.Join(dir, "mock-claude-usage-limit.sh")
	mockexec.Write(t, binary, script)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	return stdinLog
}

func appOriginRows(t *testing.T, app *App, threadID string) []string {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, item := range items {
		if item.Kind != "user_text" {
			continue
		}
		var meta usermessage.Meta
		if json.Unmarshal([]byte(item.Meta), &meta) == nil && meta.Origin == usermessage.OriginApp {
			out = append(out, item.Summary)
		}
	}
	return out
}

func hasAssistantText(t *testing.T, app *App, threadID, text string) bool {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind != "user_text" && item.Summary == text {
			return true
		}
	}
	return false
}

// A real Claude process refuses a turn for a usage limit. The selection moves
// to the other pool account, and with auto-continue on the thread is sent
// the continue message once and its next turn runs.
func TestAutoSwitchRefusedClaudeTurnSwitchesAndContinues(t *testing.T) {
	t.Parallel()
	for _, autoContinue := range []bool{true, false} {
		name := "auto-continue off"
		if autoContinue {
			name = "auto-continue on"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, bus := setupE2EApp(t)
			app.configureTriageQueueCallbacks()
			t.Cleanup(func() { app.flushDispatch.wg.Wait() })
			rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
			rig.settings(t, map[string]any{"accountAutoContinue": autoContinue})
			rig.reading("first", usageLimit("session", 5, time.Hour))
			stdinLog := installUsageLimitClaude(t, app)
			thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
			if err != nil {
				t.Fatal(err)
			}
			rig.start(t)
			t.Cleanup(func() { _ = app.StopSession(thread.ID) })

			if err := app.SendMessage(thread.ID, "go", nil); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 10*time.Second)
			waitForCondition(t, "the auto-switch report", func() bool { return len(rig.reported()) > 0 })
			if reports := rig.reported(); len(reports) != 1 || reports[0].FromAccountID != "second" ||
				reports[0].ToAccountID != "first" || reports[0].Reason != accountswitch.ReasonRefused {
				t.Fatalf("reports = %+v, want one refused switch second -> first", reports)
			}
			if got := rig.selected(); got != "first" {
				t.Fatalf("selected account = %q, want first", got)
			}

			continues := func() int {
				text, err := os.ReadFile(stdinLog)
				if err != nil {
					t.Fatal(err)
				}
				return strings.Count(string(text), accountswitch.ContinueMessage)
			}
			if !autoContinue {
				holdsFor(t, "a continue was sent with auto-continue off", func() bool {
					return continues() == 0 && len(queuedMessages(app, thread.ID)) == 0
				})
				return
			}
			waitForCondition(t, "the resumed turn's answer", func() bool { return hasAssistantText(t, app, thread.ID, "resumed") })
			if got := continues(); got != 1 {
				t.Fatalf("continue messages sent to the provider = %d, want 1", got)
			}
			if rows := appOriginRows(t, app, thread.ID); len(rows) != 1 || rows[0] != accountswitch.ContinueMessage {
				t.Fatalf("app-origin rows = %q, want the continue message once", rows)
			}
		})
	}
}

// Workflow-owned and hidden threads never receive a continue, but their
// refusal still moves the selection.
func TestAutoSwitchRefusedHiddenThreadSwitchesWithoutContinue(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{threadmode.ModeWorkflow, threadmode.ModeScratch} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.ensureTriageRouter()
			rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
			rig.settings(t, map[string]any{"accountAutoContinue": true})
			rig.reading("first", usageLimit("session", 5, time.Hour))
			thread := autoSwitchThread("hidden", string(provider.Claude), "claude-opus-5")
			thread.Mode = mode
			putAccountSession(t, app, thread, "second")
			rig.start(t)

			app.dispatchTurnObservers(thread.ID, usageLimitRefusal(thread.ID))

			waitForCondition(t, "the auto-switch report", func() bool { return len(rig.reported()) > 0 })
			if got := rig.selected(); got != "first" {
				t.Fatalf("selected account = %q, want first", got)
			}
			holdsFor(t, "a hidden thread was continued", func() bool {
				return len(queuedMessages(app, thread.ID)) == 0
			})
		})
	}
}

// A message the person sends ends the wait: once the limit resets, only the
// thread nobody wrote to is continued.
func TestAutoSwitchSendEndsTheWait(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
	rig.settings(t, map[string]any{"accountAutoContinue": true})
	reset := 3 * time.Second
	rig.reading("first", usageLimit("session", 100, reset))
	rig.reading("second", usageLimit("session", 100, reset))
	messaged := autoSwitchThread("messaged", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, messaged, "second")
	untouched := autoSwitchThread("untouched", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, untouched, "second")
	rig.start(t)

	app.dispatchTurnObservers(messaged.ID, usageLimitRefusal(messaged.ID))
	app.dispatchTurnObservers(untouched.ID, usageLimitRefusal(untouched.ID))
	if err := app.ensureProviderAccountReadyForSendLocked(messaged); err != nil {
		t.Fatalf("send gate: %v", err)
	}

	time.Sleep(reset + time.Second)
	rig.reading("second", usageLimit("session", 0, 5*time.Hour))

	waitForCondition(t, "the untouched thread's continue", func() bool {
		return len(queuedMessages(app, untouched.ID)) == 1
	})
	if got := queuedMessages(app, untouched.ID); got[0] != accountswitch.ContinueMessage {
		t.Fatalf("untouched thread queue = %q, want the continue message", got)
	}
	holdsFor(t, "the messaged thread was continued", func() bool {
		return len(queuedMessages(app, messaged.ID)) == 0
	})
}

// Turning auto-continue off ends the waits that began before it, also when it
// is turned on again before the limit resets. A thread refused after that
// waits and is continued.
func TestAutoSwitchSettingOffEndsEarlierWaits(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
	rig.settings(t, map[string]any{"accountAutoContinue": true})
	reset := 3 * time.Second
	rig.reading("first", usageLimit("session", 100, reset))
	rig.reading("second", usageLimit("session", 100, reset))
	earlier := autoSwitchThread("earlier", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, earlier, "second")
	later := autoSwitchThread("later", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, later, "second")
	rig.start(t)

	app.dispatchTurnObservers(earlier.ID, usageLimitRefusal(earlier.ID))
	rig.settings(t, map[string]any{"accountAutoContinue": false})
	rig.settings(t, map[string]any{"accountAutoContinue": true})
	app.dispatchTurnObservers(later.ID, usageLimitRefusal(later.ID))

	time.Sleep(reset + time.Second)
	rig.reading("second", usageLimit("session", 0, 5*time.Hour))

	waitForCondition(t, "the later thread's continue", func() bool {
		return len(queuedMessages(app, later.ID)) == 1
	})
	holdsFor(t, "a thread that waited before the setting was turned off was continued", func() bool {
		return len(queuedMessages(app, earlier.ID)) == 0
	})
}

// A continue that waited for the thread's action lock is checked again once
// it has the lock: a reading that arrived meanwhile and shows the selected
// account out of usage keeps the thread waiting, and it continues after the
// limit resets.
func TestAutoSwitchContinueRecheckedUnderThreadLock(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	reached := make(chan struct{}, 4)
	returned := make(chan error, 4)
	app.accountSwitcherOnce.Do(func() {
		deps := app.accountSwitchDeps()
		continueFn := deps.Continue
		deps.Continue = func(ctx context.Context, threadID string, admit func() bool) error {
			reached <- struct{}{}
			err := continueFn(ctx, threadID, admit)
			returned <- err
			return err
		}
		app.accountSwitcher = accountswitch.New(deps)
	})
	// The coordinator is built before the rig, whose listing changes reach it.
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
	rig.settings(t, map[string]any{"accountAutoContinue": true})
	rig.reading("first", usageLimit("session", 5, time.Hour))
	thread := autoSwitchThread("waiting", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "second")
	unlock := app.threadLocks().Lock(thread.ID)
	rig.start(t)

	app.dispatchTurnObservers(thread.ID, usageLimitRefusal(thread.ID))
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		unlock()
		t.Fatal("the worker never reached Continue")
	}
	reset := 2 * time.Second
	rig.reading("first", usageLimit("session", 100, reset))
	unlock()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Continue: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Continue did not return after the lock was released")
	}
	if got := queuedMessages(app, thread.ID); len(got) != 0 {
		t.Fatalf("queue = %q, want no continue on an account that is out of usage", got)
	}

	time.Sleep(reset + time.Second)
	rig.reading("first", usageLimit("session", 0, 5*time.Hour))
	waitForCondition(t, "the continue after the reset", func() bool {
		return len(queuedMessages(app, thread.ID)) == 1
	})
	if got := queuedMessages(app, thread.ID); got[0] != accountswitch.ContinueMessage {
		t.Fatalf("queue = %q, want the continue message", got)
	}
}

// Shutdown joins the worker and a continue held at the thread lock, and the
// cancelled continue writes nothing to the thread.
func TestAutoSwitchShutdownJoinsWithAWaitingThread(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()
	reachedContinue := make(chan struct{})
	var shuttingDown atomic.Bool
	var lateNotices atomic.Int32
	app.accountSwitcherOnce.Do(func() {
		deps := app.accountSwitchDeps()
		continueFn, notice := deps.Continue, deps.ThreadNotice
		deps.Continue = func(ctx context.Context, threadID string, admit func() bool) error {
			close(reachedContinue)
			return continueFn(ctx, threadID, admit)
		}
		deps.ThreadNotice = func(threadID, text string) {
			if shuttingDown.Load() {
				lateNotices.Add(1)
				t.Errorf("notice during shutdown: %q", text)
			}
			notice(threadID, text)
		}
		app.accountSwitcher = accountswitch.New(deps)
	})
	// The coordinator is built before the rig, whose listing changes reach it.
	rig := newAutoSwitchRig(t, app, string(provider.Claude), "first", "second")
	rig.settings(t, map[string]any{"accountAutoContinue": true})
	rig.reading("first", usageLimit("session", 5, time.Hour))
	thread := autoSwitchThread("waiting", string(provider.Claude), "claude-opus-5")
	putAccountSession(t, app, thread, "second")
	unlock := app.threadLocks().Lock(thread.ID)
	defer unlock()
	rig.start(t)

	app.dispatchTurnObservers(thread.ID, usageLimitRefusal(thread.ID))
	select {
	case <-reachedContinue:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never reached Continue")
	}
	app.sessionManager().take(thread.ID)

	shuttingDown.Store(true)
	done := make(chan error, 1)
	go func() { done <- app.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not join the auto-switch worker")
	}
	if n := lateNotices.Load(); n != 0 {
		t.Fatalf("%d notices written during shutdown", n)
	}
}
