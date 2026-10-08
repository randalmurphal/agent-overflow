//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// nativeBrowserSmokeEnv gates the tests that drive this platform's real
// in-process engine: WebKitGTK on Linux, WKWebView on macOS. They open a
// window, so they run only when asked:
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test ./internal/browser -run TestNativeEngineReal -count=1
//
// Linux needs a display nobody is using, such as an Xvfb or a headless
// Wayland compositor named by DISPLAY or WAYLAND_DISPLAY. macOS needs a
// logged-in session. Nothing else runs them.
const nativeBrowserSmokeEnv = "AO_NATIVE_BROWSER_SMOKE"

// nativeSmokeWindow is the window the gated run hosts pages in, nil unless
// the gate is set.
var nativeSmokeWindow atomic.Pointer[application.WebviewWindow]

func TestMain(m *testing.M) {
	if os.Getenv(nativeBrowserSmokeEnv) != "1" {
		os.Exit(m.Run())
	}
	os.Exit(runInNativeWindow(m))
}

// runInNativeWindow runs the tests inside an application loop with one
// window, which is what gtkDo and wkDo dispatch to. The loop owns the main
// thread, so the tests run beside it and the process exits when they end.
func runInNativeWindow(m *testing.M) int {
	// The engine and the window's own webview inherit these, so nothing the
	// run starts writes into the developer's home.
	home, err := os.MkdirTemp("", "ao-native-browser-smoke-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "native browser smoke: %v\n", err)
		return 1
	}
	for name, dir := range map[string]string{
		"HOME": home, "XDG_DATA_HOME": filepath.Join(home, "data"),
		"XDG_CACHE_HOME": filepath.Join(home, "cache"), "XDG_CONFIG_HOME": filepath.Join(home, "config"),
	} {
		if err := os.Setenv(name, dir); err != nil {
			fmt.Fprintf(os.Stderr, "native browser smoke: %v\n", err)
			return 1
		}
	}
	app := application.New(application.Options{Name: "ao-native-browser-smoke"})
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		window := app.Window.NewWithOptions(application.WebviewWindowOptions{
			Title: "AO native browser smoke", Width: 1280, Height: 800,
			HTML: "<!doctype html><title>AO native browser smoke</title>",
		})
		go func() {
			code := 1
			if awaitNativeHandle(window) {
				nativeSmokeWindow.Store(window)
				code = m.Run()
			} else {
				fmt.Fprintln(os.Stderr, "native browser smoke: the window never got a native handle")
			}
			if err := os.RemoveAll(home); err != nil {
				fmt.Fprintf(os.Stderr, "native browser smoke: remove %s: %v\n", home, err)
			}
			// Exit here rather than quit the loop: quitting can end the
			// process with its own status, which would hide a failure.
			os.Exit(code)
		}()
	})
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "native browser smoke: %v\n", err)
	}
	return 1
}

// awaitNativeHandle waits for the window's platform handle, which the loop
// creates after NewWithOptions returns.
func awaitNativeHandle(window *application.WebviewWindow) bool {
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if application.InvokeSyncWithResult(window.NativeWindow) != nil {
			return true
		}
	}
	return false
}

// TestNativeEngineRealPopupsPassTheWorkspacePolicy has a workspace page open
// an outside-workspace file in a popup, as a page script can: directly,
// without an opener, by scripting a blank popup's location, and in a blank
// popup's subframe. Each load can start before the Manager adopts the popup,
// so what keeps the file out of the adopted page is the policy the popup gets
// when WebKit creates it. A popup of a workspace file must still load.
func TestNativeEngineRealPopupsPassTheWorkspacePolicy(t *testing.T) {
	window := nativeSmokeWindow.Load()
	if window == nil {
		t.Skipf("set %s=1 to drive this platform's browser engine in a real window", nativeBrowserSmokeEnv)
	}
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	// Left alone, the Manager adopts a popup before the popup decides its
	// first navigation, and the page's policy answers every decision.
	holdPopupAdoption(manager, time.Second)
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	for _, tc := range []struct{ name, open string }{
		{"direct", `window.open(SECRET);`},
		{"noopener", `window.open(SECRET, "_blank", "noopener");`},
		{"scripted", `window.open("about:blank").location.href = SECRET;`},
		{"subframe", `const w = window.open("about:blank"); const f = w.document.createElement("iframe"); f.src = SECRET; w.document.body.appendChild(f);`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Resolved roots: macOS temp directories sit behind a symlink, and
			// the policy compares resolved paths.
			workspace, outside := resolvedTempDir(t), resolvedTempDir(t)
			secret := writeSmokeFile(t, outside, "secret.txt", "outside-secret")
			allowed := writeSmokeFile(t, workspace, "allowed.txt", "workspace-ok")
			opener := writeSmokeFile(t, workspace, "index.html", fmt.Sprintf(
				"<!doctype html><title>opener</title><script>const SECRET = %s; %s window.open(%s);</script>",
				jsonString(smokeFileURL(secret)), tc.open, jsonString(smokeFileURL(allowed))))
			access := Access{ThreadID: "native-smoke-" + tc.name, Workspace: workspace}
			t.Cleanup(func() {
				if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
					t.Errorf("close the thread's pages: %v", err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			openerPage, err := manager.OpenFile(ctx, access, opener, OpenOptions{})
			if err != nil {
				t.Fatalf("open the workspace page: %v", err)
			}
			// The workspace popup, opened last, loading is the sign that the
			// first navigations of every popup before it have been decided.
			for !smokePopupsShow(ctx, t, manager, access, openerPage.ID, "workspace-ok") {
				select {
				case <-ctx.Done():
					t.Fatal("the popup of a workspace file never loaded it")
				case <-time.After(100 * time.Millisecond):
				}
			}
			// The outside file must then stay out for a while longer, in every
			// popup the Manager has adopted and in every subframe of one.
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
				if smokePopupsShow(ctx, t, manager, access, openerPage.ID, "outside-secret") {
					t.Fatal("a popup shows the outside file")
				}
				for _, id := range smokePopups(ctx, t, manager, access, openerPage.ID) {
					frames, _, err := manager.Evaluate(ctx, access, id, `Array.from(document.querySelectorAll("iframe"), (f) => { try { return f.contentWindow.location.href; } catch (e) { return "unreadable"; } })`, nil)
					if err != nil {
						t.Fatalf("read popup %s subframes: %v", id, err)
					}
					list, _ := frames.([]any)
					for _, frame := range list {
						if frame != "about:blank" {
							t.Fatalf("a subframe of popup %s left about:blank: %v", id, frame)
						}
					}
				}
			}
		})
	}
}

// smokePopups lists the thread's pages besides the opener.
func smokePopups(ctx context.Context, t *testing.T, manager *Manager, access Access, openerID string) []string {
	t.Helper()
	pages, err := manager.Pages(ctx, access)
	if err != nil {
		t.Fatalf("list pages: %v", err)
	}
	var popups []string
	for _, page := range pages {
		if page.ID != openerID {
			popups = append(popups, page.ID)
		}
	}
	return popups
}

// smokePopupsShow reports whether any popup's document shows text.
func smokePopupsShow(ctx context.Context, t *testing.T, manager *Manager, access Access, openerID, text string) bool {
	t.Helper()
	for _, id := range smokePopups(ctx, t, manager, access, openerID) {
		value, _, err := manager.Evaluate(ctx, access, id, `document.body ? document.body.innerText : ""`, nil)
		if err != nil {
			t.Fatalf("read popup %s: %v", id, err)
		}
		if s, _ := value.(string); strings.Contains(s, text) {
			return true
		}
	}
	return false
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeSmokeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func smokeFileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// TestNativeEngineRealEvaluateSemantics holds this engine to the evaluate
// contract on a page whose script policy forbids eval, as AO's own UI does.
func TestNativeEngineRealEvaluateSemantics(t *testing.T) {
	window := nativeSmokeWindow.Load()
	if window == nil {
		t.Skipf("set %s=1 to drive this platform's browser engine in a real window", nativeBrowserSmokeEnv)
	}
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := Access{ThreadID: "native-smoke-evaluate", Workspace: workspace}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "evaluate.html", evaluateSemanticsPage), OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	assertEvaluateSemantics(t, evaluateEngineWebKit,
		func(code string, argument json.RawMessage) (any, string, error) {
			return manager.Evaluate(ctx, access, page.ID, code, argument)
		},
		func(code string, argument json.RawMessage) (any, string, error) {
			value, note, err := manager.EvaluateReadOnly(ctx, access, page.ID, code, argument)
			if err == nil && !strings.HasPrefix(note, "Note: this browser engine cannot reject side effects") {
				t.Errorf("read-only evaluate of %q carried the note %q, not the caveat: this engine has no side-effect check", code, note)
			}
			return value, note, err
		})
	t.Run("the native bridge carries JSON data or fails", func(t *testing.T) {
		assertNativeEvalBridge(ctx, t, manager, access, page.ID)
	})
}

// assertNativeEvalBridge holds the glue under every WebKit page operation to
// its contract: a result crosses as JSON text, undefined as no result, and a
// value with no JSON form fails rather than ending the app or reading as no
// result. The evaluate tools never reach these cases, since their runner
// returns a string.
func assertNativeEvalBridge(ctx context.Context, t *testing.T, manager *Manager, access Access, pageID string) {
	p, _, err := manager.lookupOwnedPage(access, pageID)
	if err != nil {
		t.Fatalf("look the page up: %v", err)
	}
	bridge, ok := p.driver.(interface {
		evalBody(context.Context, string) (json.RawMessage, error)
	})
	if !ok {
		t.Fatalf("the page driver %T has no evalBody", p.driver)
	}
	for body, want := range map[string]string{
		`return {a: [1, "x", null, true]};`: `{"a":[1,"x",null,true]}`,
		`return "text";`:                    `"text"`,
		`return undefined;`:                 ``,
	} {
		raw, err := bridge.evalBody(ctx, body)
		if err != nil || string(raw) != want {
			t.Errorf("evalBody(%q) = %q, %v; want %q", body, raw, err, want)
		}
	}
	noJSON := []string{`return () => 1;`, `const o = {}; o.o = o; return o;`}
	if runtime.GOOS == "darwin" {
		// JSC's JSON.stringify writes NaN as null; NSJSONSerialization
		// has no form for it.
		noJSON = append(noJSON, `return NaN;`, `return {at: new Date(0)};`)
	}
	for _, body := range noJSON {
		if raw, err := bridge.evalBody(ctx, body); err == nil {
			t.Errorf("evalBody(%q) = %q, want an error", body, raw)
		}
	}
}
