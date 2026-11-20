//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestNativeEngineRealPopupsPassTheWorkspacePolicy has a workspace page open
// an outside-workspace file in a popup, as a page script can: directly,
// without an opener, by scripting a blank popup's location, and in a blank
// popup's subframe. Each load can start before the Manager adopts the popup,
// so what keeps the file out of the adopted page is the policy the popup gets
// when WebKit creates it: its opener's. A popup of a workspace file must
// still load, in every cookie jar: a jar never decides file authority, so a
// worktree outside the project root keeps its files in a shared jar too.
func TestNativeEngineRealPopupsPassTheWorkspacePolicy(t *testing.T) {
	window := smokeWindow(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	// Left alone, the Manager adopts a popup before the popup decides its
	// first navigation, and the page's policy answers every decision.
	holdPopupAdoption(manager, time.Second)
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	cases := []struct{ name, open string }{
		{"direct", `window.open(SECRET);`},
		{"noopener", `window.open(SECRET, "_blank", "noopener");`},
		{"scripted", `window.open("about:blank").location.href = SECRET;`},
		{"subframe", `const w = window.open("about:blank"); const f = w.document.createElement("iframe"); f.src = SECRET; w.document.body.appendChild(f);`},
	}
	for _, jar := range []string{JarWorktree, JarThread, JarProject} {
		for _, tc := range cases {
			t.Run(jar+"/"+tc.name, func(t *testing.T) {
				// Resolved roots: macOS temp directories sit behind a symlink, and
				// the policy compares resolved paths.
				workspace, outside := resolvedTempDir(t), resolvedTempDir(t)
				secret := writeSmokeFile(t, outside, "secret.txt", "outside-secret")
				allowed := writeSmokeFile(t, workspace, "allowed.txt", "workspace-ok")
				opener := writeSmokeFile(t, workspace, "index.html", fmt.Sprintf(
					"<!doctype html><title>opener</title><script>const SECRET = %s; %s window.open(%s);</script>",
					jsonString(smokeFileURL(secret)), tc.open, jsonString(smokeFileURL(allowed))))
				access := manager.Bind(Caller{
					ThreadID: ThreadID("native-smoke-" + jar + "-" + tc.name), Workspace: workspace,
					ProjectRoot: resolvedTempDir(t), ProjectID: "native-smoke",
				})
				t.Cleanup(func() {
					if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
						t.Errorf("close the thread's pages: %v", err)
					}
				})
				if err := manager.SetJar(context.Background(), access, jar, JarSwitchConfirmed); err != nil {
					t.Fatalf("choose the %s jar: %v", jar, err)
				}
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
						frames, _, err := manager.Evaluate(ctx, access, PageID(id), `Array.from(document.querySelectorAll("iframe"), (f) => { try { return f.contentWindow.location.href; } catch (e) { return "unreadable"; } })`, nil)
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
}

// TestNativeEngineRealEvaluateSemantics holds this engine to the evaluate
// contract on a page whose script policy forbids eval, as AO's own UI does.
func TestNativeEngineRealEvaluateSemantics(t *testing.T) {
	window := smokeWindow(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := manager.Bind(Caller{ThreadID: "native-smoke-evaluate", Workspace: workspace})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
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
			return manager.Evaluate(ctx, access, PageID(page.ID), code, argument)
		},
		func(code string, argument json.RawMessage) (any, string, error) {
			value, note, err := manager.EvaluateReadOnly(ctx, access, PageID(page.ID), code, argument)
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
	p, _, err := manager.lookupOwnedPage(access, PageID(pageID))
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
