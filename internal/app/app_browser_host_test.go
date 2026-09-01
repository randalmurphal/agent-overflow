package app

import (
	"reflect"
	"testing"

	"agent-overflow/internal/webview2host"
)

// The launcher posts its answers under webview2host.RPCReport, which it
// resolves by NAME over the notification bridge. Renaming the method here
// would leave the launcher calling a method that no longer exists, and
// nothing else would fail: the pane's creates would simply hang until
// their timeout. So the name is pinned.
func TestBrowserHostReportIsNamedForTheLauncherRPC(t *testing.T) {
	if _, ok := reflect.TypeOf(&App{}).MethodByName(webview2host.RPCReport); !ok {
		t.Fatalf("no *App method named %q; webview2host.RPCReport and the bound method have drifted", webview2host.RPCReport)
	}
}

func TestBrowserHostReportWithoutAManager(t *testing.T) {
	if err := (&App{}).BrowserHostReport("page1", string(webview2host.ReportClosed), ""); err == nil {
		t.Fatal("a report was accepted with no browser manager")
	}
}

// No relay means no launcher, which is the whole engine selection: an App
// that never had one must not build the hosted engine's wiring.
func TestPaneHostOptionsRequireARelay(t *testing.T) {
	if opts := (&App{}).paneHostOptions(); opts != nil {
		t.Fatalf("paneHostOptions returned %+v with no relay", opts)
	}
}
