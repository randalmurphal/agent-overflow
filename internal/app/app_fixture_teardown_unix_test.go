//go:build unix

package app

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil"
)

// The fixtures' teardown closes a session a test left open, so its
// provider process has exited once the teardown returns, and returns the
// session's Close error. A process of the test's own joins the session's
// process group and exits without being waited for, so the group outlives
// SIGKILL and the session's Close fails, as it does for a provider that
// leaves a process the group stop cannot end.
func TestTheFixtureTeardownClosesALeftSessionAndReturnsItsCloseError(t *testing.T) {
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
	member := exec.Command("true")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pid}
	if err := member.Start(); err != nil {
		t.Fatalf("join the session's process group: %v", err)
	}

	errs := closeLeftSessions(app)
	// The member may have exited before the session's group was stopped or
	// been ended by it; either way it stayed in the group, unwaited for.
	if err := member.Wait(); err != nil && !errors.As(err, new(*exec.ExitError)) {
		t.Errorf("the group member: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the session's process %d is still there after the teardown (kill 0: %v)", pid, err)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "close claude session for thread "+thread.ID) {
		t.Errorf("the teardown returned %v; want the session's Close error", errs)
	}
	if open := len(app.sessionManager().runtime.Snapshot()); open != 0 {
		t.Errorf("the App holds %d sessions after the teardown", open)
	}
}
