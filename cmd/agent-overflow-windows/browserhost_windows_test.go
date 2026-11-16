//go:build windows

package main

import (
	"os"
	"testing"

	"agent-overflow/internal/webview2host"
	"agent-overflow/internal/wsllauncher"
)

// A release from a backend whose engine stopped must not build the host: with
// no host there is no environment to release, and building one would create
// the profile folder and start the CDP tunnel for nothing.
func TestBrowserHostReleaseNeverBuildsTheHost(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	a := &launcherApp{}
	a.handleBrowserHostDirective(&wsllauncher.Bootstrap{Port: 1, Token: "test-token"}, webview2host.Directive{Op: webview2host.OpRelease})
	a.mu.Lock()
	host := a.browserHost
	a.mu.Unlock()
	if host != nil {
		host.Close()
		t.Fatal("a release built the browser host")
	}
	if _, err := os.Stat(browserProfilesDir(launcherRuntimeMode())); !os.IsNotExist(err) {
		t.Fatalf("a release prepared the browser profile folder (stat: %v)", err)
	}
}
