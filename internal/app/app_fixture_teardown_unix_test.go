//go:build unix

package app

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil"
)

// The fixtures' teardown closes a session a test left open, so its provider
// process has exited and been waited for once the teardown returns.
func TestTheFixtureTeardownEndsTheProcessOfALeftSession(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), nil)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	s, ok := app.sessionManager().get(thread.ID)
	if !ok || s.ProviderSession() == nil {
		t.Fatalf("no session for %s after StartSession", thread.ID)
	}
	pid := s.ProviderSession().PID()
	if errs := closeLeftSessions(app); len(errs) != 0 {
		t.Errorf("the teardown returned %v; want the session closed", errs)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the session's process %d is still there after the teardown (kill 0: %v)", pid, err)
	}
	if open := len(app.sessionManager().runtime.Snapshot()); open != 0 {
		t.Errorf("the App holds %d sessions after the teardown", open)
	}
}

// The teardown waits out a session start that passed the refusal before the
// teardown began, and closes the session it registers with the rest. The
// start is held until the teardown has taken the sessions, or for 200 ms
// after the teardown began, so a teardown that took the sessions without
// waiting would take them before the start registers its session.
func TestAppFixtureTeardownClosesTheSessionOfAStartInFlight(t *testing.T) {
	t.Parallel()
	const standIn = "stand-in-thread"
	var (
		app      *App
		started  provider.Session
		startErr = make(chan error, 1)
	)
	t.Run("test", func(t *testing.T) {
		// Made before the fixture, so they outlast its teardown, during
		// which the start runs.
		workspace := t.TempDir()
		binary := testutil.WriteMockClaudeScript(t, t.TempDir(), nil)
		app, _ = setupE2EApp(t)
		if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
			t.Fatal(err)
		}
		thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
		if err != nil {
			t.Fatal(err)
		}
		app.sessionManager().put(standIn, session{Provider: string(provider.Claude)})
		entered := make(chan struct{})
		app.startSessionFn = func(threadID string) error {
			close(entered)
			for !app.shuttingDown.Load() {
				time.Sleep(time.Millisecond)
			}
			for until := time.Now().Add(200 * time.Millisecond); time.Now().Before(until); time.Sleep(time.Millisecond) {
				if _, held := app.sessionManager().get(standIn); !held {
					break
				}
			}
			if err := app.startSessionNow(threadID); err != nil {
				return err
			}
			registered, _ := app.sessionManager().get(threadID)
			started = registered.ProviderSession()
			return nil
		}
		go func() { startErr <- app.startSession(context.Background(), thread.ID) }()
		<-entered
	})
	if err := <-startErr; err != nil || started == nil {
		t.Fatalf("the start in flight registered no session during the teardown: %v", err)
	}
	if open := len(app.sessionManager().runtime.Snapshot()); open != 0 {
		t.Errorf("the App holds %d sessions after its teardown, want the start's taken with the rest", open)
		for _, err := range closeLeftSessions(app) {
			t.Error(err)
		}
	}
	if err := syscall.Kill(started.PID(), 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the provider process %d the start began is still there after the teardown (kill 0: %v)", started.PID(), err)
	}
}
