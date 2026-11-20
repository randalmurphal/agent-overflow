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
// start still proves the session and reads its content.
func TestStartSessionProvingResumeIgnoresAStalledEventWorker(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	app.providerEvents.drainStall = 50 * time.Millisecond
	thread := e2eThread("thread-proof-stalled-worker", string(provider.Claude), t.TempDir())
	thread.SessionRef = workflowResumeSession
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' '" + initLine(workflowResumeSession) + "'\n" +
		"while IFS= read -r line; do\n" +
		"  case \"$line\" in *'\"subtype\":\"get_context_usage\"'*)\n" +
		"    reqid=$(printf '%s' \"$line\" | sed -n 's/.*\"request_id\":\"\\([^\"]*\\)\".*/\\1/p')\n" +
		"    printf '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"%s\",\"response\":%s}}\\n' \"$reqid\" '" + resumeContextUsage + "' ;;\n" +
		"  esac\n" +
		"done\n"
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
		_ = app.StopSession(thread.ID)
	})

	done := make(chan error, 1)
	go func() { done <- (workflowHostAdapter{app: app}).StartSessionProvingResume(t.Context(), thread.ID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("proving start = %v, want the session proven", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the proof waited on the stalled event worker")
	}
}
