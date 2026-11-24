package app

import (
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// The proof does not wait on the App's handling of the CLI's frames: with
// the thread's provider event worker stuck on the init it is handling, the
// start still proves the session.
func TestEnsureProvenSessionIgnoresAStalledEventWorker(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	app.providerEvents.drainStall = 50 * time.Millisecond
	thread := e2eThread("thread-proof-stalled-worker", string(provider.Claude), t.TempDir())
	thread.SessionRef = workflowResumeSession
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' '" + initLine(workflowResumeSession) + "'\n" +
		"while IFS= read -r line; do :; done\n"
	binary := mockexec.Write(t, filepath.Join(t.TempDir(), "claude-init.sh"), script)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(_ string, evt provider.ProviderEvent) {
		if evt.Kind == provider.EventInit {
			<-release
		}
	})
	t.Cleanup(func() {
		close(release)
		unsubscribe()
		stopSessionChecked(t, app, thread.ID)
	})

	type proof struct {
		started bool
		err     error
	}
	done := make(chan proof, 1)
	go func() {
		started, err := (workflowHostAdapter{app: app}).EnsureProvenSession(t.Context(), thread.ID, func() error { return nil })
		done <- proof{started, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || !got.started {
			t.Fatalf("proving start = started %v, err %v; want the session started and proven", got.started, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the proof waited on the stalled event worker")
	}
}
