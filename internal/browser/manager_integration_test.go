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

// This opt-in test uses AO_BROWSER_BINARY when supplied, otherwise downloads
// managed Chrome into a temporary directory. It is excluded from the normal
// test gate because either path launches a real browser process.
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
	installer := chromium.NewInstaller(artifactDir, "", nil)
	installer.BinaryPath = strings.TrimSpace(os.Getenv("AO_BROWSER_BINARY"))
	config := Config{Enabled: true, PersistSiteData: true}
	manager := NewManager(installer, filepath.Join(root, "state"), config, ManagerOptions{FileStateKey: true})
	manager.state = newTestStateStore(filepath.Join(root, "state"), bytes.Repeat([]byte{3}, 32))
	popupAdopted := make(chan struct{}, 1)
	manager.pageAdopted = func() {
		select {
		case popupAdopted <- struct{}{}:
		default:
		}
	}
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
	background, err := manager.Open(ctx, access, server.URL+"/popup", OpenOptions{})
	if err != nil || background.ID == opened.ID {
		t.Fatalf("open without page_id did not create a distinct page: %#v, %v", background, err)
	}
	if state := manager.CompanionState(access); state.Visible == nil || *state.Visible || state.ActivePageID != opened.ID {
		t.Fatalf("background open changed presentation state: %#v", state)
	}
	if _, err := manager.Snapshot(ctx, access, ""); err == nil || !strings.Contains(err.Error(), "page_id is required") {
		t.Fatalf("multi-page implicit operation error = %v", err)
	}
	if info, err := manager.LabelPage(ctx, access, opened.ID, "app-preview"); err != nil || info.Label != "app-preview" {
		t.Fatalf("label page = %#v, %v", info, err)
	}
	if err := manager.ClosePage(ctx, access, background.ID); err != nil {
		t.Fatal(err)
	}
	visible := true
	if _, err := manager.Visibility(ctx, access, &visible, opened.ID); err != nil {
		t.Fatal(err)
	}
	sub, err := manager.SubscribeCompanion(access, 800, 600)
	if err != nil || sub.State.ActivePageID != opened.ID {
		t.Fatalf("subscribe companion = %#v, %v", sub, err)
	}
	frameCtx, cancelFrame := context.WithTimeout(ctx, 10*time.Second)
	defer cancelFrame()
	event, err := manager.NextCompanionFrame(frameCtx, sub.ID)
	if err != nil {
		t.Fatalf("companion frame not received: %v", err)
	}
	if event.Kind != "frame" || event.PageID != opened.ID || !strings.HasPrefix(event.Frame, "/9j/") {
		t.Fatalf("companion frame = kind %q page %q JPEG %.12q", event.Kind, event.PageID, event.Frame)
	}
	if _, err := manager.Evaluate(ctx, access, opened.ID, `document.querySelector('#value').focus()`); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompanionInput(ctx, access, opened.ID, CompanionInput{Kind: "text", Text: "companion"}); err != nil {
		t.Fatal(err)
	}
	companionValue, err := manager.Evaluate(ctx, access, opened.ID, `document.querySelector('#value').value`)
	if err != nil || companionValue != "companion" {
		t.Fatalf("companion text input = %#v, %v", companionValue, err)
	}
	if err := manager.CompanionInput(ctx, access, opened.ID, CompanionInput{Kind: "key", Key: "a", Control: true}); err != nil {
		t.Fatal(err)
	}
	if err := manager.CompanionInput(ctx, access, opened.ID, CompanionInput{Kind: "text", Text: "q"}); err != nil {
		t.Fatal(err)
	}
	companionValue, err = manager.Evaluate(ctx, access, opened.ID, `document.querySelector('#value').value`)
	if err != nil || companionValue != "q" {
		t.Fatalf("companion modifier chord = %#v, %v", companionValue, err)
	}
	manager.UnsubscribeCompanion(sub.ID)
	page, _, err := manager.lookupOwnedPage(access, opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	page.streamMu.Lock()
	streaming := page.stream != nil
	page.streamMu.Unlock()
	if streaming {
		t.Fatal("companion screencast survived final unsubscribe")
	}
	snapshot, err := manager.Snapshot(ctx, access, opened.ID)
	if err != nil || len(snapshot.Elements) < 4 {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	if _, err := manager.Type(ctx, access, TypeOptions{PageID: opened.ID, Selector: "#value", Text: "typed", Clear: true}); err != nil {
		t.Fatal(err)
	}
	typedValue, err := manager.Evaluate(ctx, access, opened.ID, `document.querySelector('#value').value`)
	if err != nil || typedValue != "typed" {
		t.Fatalf("clear then type = %#v, %v", typedValue, err)
	}
	if _, err := manager.Press(ctx, access, opened.ID, "ControlOrMeta+a"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Press(ctx, access, opened.ID, "z"); err != nil {
		t.Fatal(err)
	}
	typedValue, err = manager.Evaluate(ctx, access, opened.ID, `document.querySelector('#value').value`)
	if err != nil || typedValue != "z" {
		t.Fatalf("modifier chord = %#v, %v", typedValue, err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#value"}, Action: "press", Value: "ControlOrMeta+a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#value"}, Action: "type", Value: "locator"}); err != nil {
		t.Fatal(err)
	}
	typedValue, err = manager.Evaluate(ctx, access, opened.ID, `document.querySelector('#value').value`)
	if err != nil || typedValue != "locator" {
		t.Fatalf("locator modifier chord = %#v, %v", typedValue, err)
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
	popupCtx, cancelPopup := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPopup()
	select {
	case <-popupAdopted:
	case <-popupCtx.Done():
		t.Fatalf("popup was not adopted: %v", popupCtx.Err())
	}
	pages, err := manager.Pages(ctx, access)
	if err != nil || len(pages) != 2 || (pages[0].Title != "Popup" && pages[1].Title != "Popup") {
		t.Fatalf("popup pages = %#v, %v", pages, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	manager2 := NewManager(installer, filepath.Join(root, "state"), config, ManagerOptions{FileStateKey: true})
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
	if _, err := manager2.Visibility(ctx, access, &visible, reopened.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager2.SubscribeCompanion(access, 800, 600); err != nil {
		t.Fatal(err)
	}
	closeThreadCtx, cancelCloseThread := context.WithTimeout(ctx, 5*time.Second)
	defer cancelCloseThread()
	if err := manager2.CloseThread(closeThreadCtx, access.ThreadID); err != nil {
		t.Fatalf("close thread with active companion: %v", err)
	}
}
