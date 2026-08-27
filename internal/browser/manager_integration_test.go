package browser

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/chromium"
)

// This opt-in test downloads managed Chrome into a temporary directory. It is
// excluded from the normal test gate because it needs the public Chrome for
// Testing network endpoints and downloads a large archive.
func TestManagerWithManagedChrome(t *testing.T) {
	if os.Getenv("AO_BROWSER_INTEGRATION") != "1" {
		t.Skip("set AO_BROWSER_INTEGRATION=1 to download and exercise managed Chrome")
	}
	root := t.TempDir()
	htmlPath := filepath.Join(root, "local.html")
	if err := os.WriteFile(htmlPath, []byte(`<!doctype html><title>Local file</title><main>direct file works</main>`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/popup" {
			_, _ = w.Write([]byte(`<!doctype html><title>Popup</title><main>popup body</main>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><title>Browser fixture</title>
<input id="value"><button id="trusted" onclick="window.trusted=event.isTrusted;localStorage.setItem('marker','saved')">trusted</button>
<button id="popup" onclick="window.open('/popup')">popup</button><button id="dialog" onclick="alert('dismiss me');document.title='Dialog cleared'">dialog</button>`))
	}))
	t.Cleanup(server.Close)

	artifactDir := strings.TrimSpace(os.Getenv("AO_BROWSER_ARTIFACT_DIR"))
	if artifactDir == "" {
		artifactDir = filepath.Join(root, "artifacts")
	}
	installer := chromium.NewInstaller(artifactDir, chromium.ArtifactChrome, "", nil)
	config := Config{Enabled: true, ShowWindow: os.Getenv("AO_BROWSER_HEADFUL") == "1", PersistSiteData: true}
	manager := NewManager(installer, filepath.Join(root, "state"), config)
	manager.state = newTestStateStore(filepath.Join(root, "state"), bytes.Repeat([]byte{3}, 32))
	t.Cleanup(func() { _ = manager.Close() })
	access := Access{ThreadID: "thread", Workspace: root, ProjectRoot: root}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	local, err := manager.OpenFile(ctx, access, htmlPath, OpenOptions{})
	if err != nil || local.Title != "Local file" || !strings.HasPrefix(local.URL, "file://") {
		t.Fatalf("open local = %#v, %v", local, err)
	}
	opened, err := manager.Open(ctx, access, server.URL, OpenOptions{PageID: local.ID})
	if err != nil || opened.Title != "Browser fixture" {
		t.Fatalf("open web = %#v, %v", opened, err)
	}
	snapshot, err := manager.Snapshot(ctx, access, opened.ID)
	if err != nil || len(snapshot.Elements) < 4 {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	if _, err := manager.Type(ctx, access, TypeOptions{PageID: opened.ID, Selector: "#value", Text: "typed", Clear: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Click(ctx, access, opened.ID, "#trusted"); err != nil {
		t.Fatal(err)
	}
	trusted, err := manager.Evaluate(ctx, access, opened.ID, "window.trusted")
	if err != nil || trusted != true {
		t.Fatalf("trusted click = %#v, %v", trusted, err)
	}
	permission, err := manager.Evaluate(ctx, access, opened.ID, `navigator.permissions.query({name:'geolocation'}).then(result => result.state)`)
	if err != nil || permission != "denied" {
		t.Fatalf("geolocation permission = %#v, %v", permission, err)
	}
	shot, err := manager.Screenshot(ctx, access, ScreenshotOptions{PageID: opened.ID})
	if err != nil || len(shot) < 4 || shot[0] != 0xff || shot[1] != 0xd8 {
		t.Fatalf("screenshot bytes = %d, %v", len(shot), err)
	}
	dialog, err := manager.Click(ctx, access, opened.ID, "#dialog")
	if err != nil || dialog.Title != "Dialog cleared" {
		t.Fatalf("dismiss dialog = %#v, %v", dialog, err)
	}
	if _, err := manager.Click(ctx, access, opened.ID, "#popup"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pages, err := manager.Pages(ctx, access)
		if err == nil && len(pages) == 2 && (pages[0].Title == "Popup" || pages[1].Title == "Popup") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("popup pages = %#v, %v", pages, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	manager2 := NewManager(installer, filepath.Join(root, "state"), config)
	manager2.state = newTestStateStore(filepath.Join(root, "state"), bytes.Repeat([]byte{3}, 32))
	t.Cleanup(func() { _ = manager2.Close() })
	reopened, err := manager2.Open(ctx, access, server.URL, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	marker, err := manager2.Evaluate(ctx, access, reopened.ID, `localStorage.getItem('marker')`)
	if err != nil || marker != "saved" {
		t.Fatalf("restored local storage = %#v, %v", marker, err)
	}
}
