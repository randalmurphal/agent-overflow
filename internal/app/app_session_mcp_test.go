package app

import (
	"path/filepath"
	"testing"

	"agent-overflow/internal/attachedbackends"
	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/buildvariant"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/store"
	"github.com/google/uuid"
)

// sessionToolURLs issues every AO tool server's URL to one session of the
// thread, the way startSession does, and returns them by server name.
func sessionToolURLs(t *testing.T, a *App, thread store.Thread, token string) map[string]string {
	t.Helper()
	browser, err := a.browserMCPConfigForThread(thread, token)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := a.remoteMCPServer().RegisterThread(thread.ID, token, remoteMCPAccess{thread.ID, token})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		appbrowser.ServerName: browser[appbrowser.ServerName].(map[string]any)["url"].(string),
		remoteMCPName:         remote[remoteMCPName].(map[string]any)["url"].(string),
		threadMCPName:         threadMCPEndpoint(t, a, thread, token),
	}
}

func requireToolURLStatus(t *testing.T, urls map[string]string, want int, when string) {
	t.Helper()
	for name, url := range urls {
		if status, _ := remoteMCPRequest(t, url, "tools/list", nil); status != want {
			t.Errorf("%s: %s URL status = %d, want %d", when, name, status, want)
		}
	}
}

func TestSessionEndRevokesEveryToolURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		end  func(*App, string, string)
	}{
		{"stop", func(a *App, threadID, _ string) {
			if err := a.StopSession(threadID); err != nil {
				t.Fatalf("StopSession: %v", err)
			}
		}},
		{"provider exit", func(a *App, threadID, token string) { a.unregisterSession(threadID, token) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAppWithStore(t)
			manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
			a.browser.manager, a.browser.mcp = manager, appbrowser.NewMCPServer(manager, true)
			t.Cleanup(func() {
				_ = a.browser.mcp.Close()
				_ = manager.Close()
				_ = a.threadMCPServer().Close()
				_ = a.remoteMCPServer().Close()
			})
			thread := makeWorkspaceThread(t, a, uuid.NewString())

			token := uuid.NewString()
			urls := sessionToolURLs(t, a, thread, token)
			a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
			requireToolURLStatus(t, urls, 200, "live session")

			tc.end(a, thread.ID, token)
			requireToolURLStatus(t, urls, 404, "ended session")

			// The next session of the same thread gets working URLs.
			next := uuid.NewString()
			nextURLs := sessionToolURLs(t, a, thread, next)
			requireToolURLStatus(t, nextURLs, 200, "restarted session")

			// A late teardown of the ended session leaves the next one alone.
			a.revokeSessionMCP(thread.ID, token)
			requireToolURLStatus(t, nextURLs, 200, "after a late teardown of the old session")
		})
	}
}

func TestFailedSessionStartRevokesEveryToolURL(t *testing.T) {
	t.Parallel()
	a := newTestAppWithStore(t)
	t.Cleanup(func() { _ = a.ServiceShutdown() })
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	a.browser.manager, a.browser.mcp = manager, appbrowser.NewMCPServer(manager, true)
	backends := t.TempDir()
	var err error
	if a.backends, err = attachedbackends.New(backends, "source", "test"); err != nil {
		t.Fatal(err)
	}
	peer := deviceclient.Session{BackendID: uuid.NewString(), SessionID: "test", Credential: "test", Endpoint: "https://127.0.0.1:1"}
	if err := deviceclient.SaveSession(backends, peer); err != nil {
		t.Fatal(err)
	}
	if err := a.backends.SetAgentAccess(peer.BackendID, true); err != nil {
		t.Fatal(err)
	}
	thread := makeWorkspaceThread(t, a, uuid.NewString())
	// An earlier registration stays unless the start replaces it with its
	// own, so the remote row below fails if the start skipped remote tools.
	if config, err := a.remoteMCPConfigForThread(thread, uuid.NewString()); err != nil || buildvariant.RemoteAccess && len(config) == 0 {
		t.Fatalf("remote tools unavailable to the thread: %v, %v", config, err)
	}
	if _, err := a.settings.Update(map[string]any{"codexBinaryPath": filepath.Join(t.TempDir(), "missing-codex")}); err != nil {
		t.Fatal(err)
	}

	if err := a.startSessionNow(thread.ID); err == nil {
		t.Fatal("a session with a missing provider binary started")
	}
	for name, registered := range map[string]bool{
		appbrowser.ServerName: a.browser.mcp.HasThread(thread.ID),
		remoteMCPName:         a.remoteMCPServer().HasThread(thread.ID),
		threadMCPName:         a.threadMCPServer().HasThread(thread.ID),
	} {
		if registered {
			t.Errorf("a failed start left the %s URL registered", name)
		}
	}
}
