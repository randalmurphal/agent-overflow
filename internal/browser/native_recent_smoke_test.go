//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"
)

// TestNativeEngineRealFailedLoadIsNoRecentAddress holds this engine to what
// the recent addresses rely on (recent.go): a load that fails ends on no
// address of the failed one's, whether the engine answers it as an error or
// not, so only the address that loaded is recorded.
func TestNativeEngineRealFailedLoadIsNoRecentAddress(t *testing.T) {
	window := smokeWindow(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := Access{ThreadID: "native-smoke-recent", Workspace: workspace}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "loaded.html", "<title>Loaded</title>loaded"), OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	// A port nothing listens on: the connection is refused.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + listener.Addr().String() + "/"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := manager.Open(ctx, access, refused, OpenOptions{PageID: page.ID})
	t.Logf("the refused load answered %q, %v", info.URL, err)
	if err == nil && info.URL == refused {
		t.Fatalf("the refused load reports its own address %q", info.URL)
	}
	var got []string
	for _, entry := range manager.RecentAddresses(access) {
		got = append(got, entry.URL)
	}
	if want := []string{page.URL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recent = %q, want only the loaded page %q", got, want)
	}
}
