//go:build !windows

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
)

// TestHeadlessChromiumRealNavigationPolicy holds a real Chromium, driven by
// the Manager, to the navigation policy the engine answers once per request,
// browser-wide: a workspace page cannot reach a file outside its workspace
// as a document, an image, a script or a subframe (one added after load
// included), through a redirect, or in a popup with or without an opener,
// while its own workspace's files load; two worktrees sharing the project
// jar each keep their own authority; and a window a script opens with no
// user gesture is refused by Chromium itself, so it never reaches the
// Manager. Gated with TestHeadlessChromiumReal.
func TestHeadlessChromiumRealNavigationPolicy(t *testing.T) {
	if os.Getenv("AO_HEADLESS_CHROMIUM_SMOKE") != "1" {
		t.Skip("set AO_HEADLESS_CHROMIUM_SMOKE=1 to launch this machine's real Chromium")
	}
	t.Setenv("HOME", t.TempDir())
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{HeadlessChromium: &HeadlessChromiumOptions{Reaper: procutiltest.Reaper()}})
	if _, ok := manager.engine.(*headlessEngine); !ok {
		t.Fatalf("no headless engine: %T", manager.engine)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	proj := newProject(t, "policy-smoke")
	one, two := canonical(t, proj.one), canonical(t, proj.two)
	outside := resolvedTempDir(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, smokeFileURL(filepath.Join(outside, "secret.html")), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>web</title><body>web-ok"))
	}))
	defer server.Close()
	for _, dir := range []string{one, two, outside} {
		name := filepath.Base(dir)
		files := map[string]string{
			"img.svg":     `<svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"/>`,
			"s.js":        fmt.Sprintf(`window.ran = (window.ran || []).concat([%q]);`, name),
			"frame.html":  fmt.Sprintf(`<script>parent.postMessage("frame:%s:" + location.search, "*")</script>`, name),
			"secret.html": fmt.Sprintf(`<body>secret-%s</body>`, name),
		}
		for file, content := range files {
			writeSmokeFile(t, dir, file, content)
		}
	}
	page := func(from string) string {
		var b strings.Builder
		b.WriteString(`<!doctype html><title>policy</title><script>window.got = []; addEventListener("message", (e) => window.got.push(String(e.data)));</script>`)
		fmt.Fprintf(&b, `<button id="popup" onclick='window.r1 = String(window.open(%q))'>p</button>`, smokeFileURL(filepath.Join(outside, "secret.html")))
		fmt.Fprintf(&b, `<button id="noopener" onclick='window.r2 = String(window.open(%q, "_blank", "noopener"))'>n</button>`, smokeFileURL(filepath.Join(outside, "secret.html"))+"?noopener")
		fmt.Fprintf(&b, `<button id="own" onclick='window.r3 = String(window.open(%q))'>o</button>`, smokeFileURL(filepath.Join(from, "secret.html")))
		for _, dir := range []string{one, two, outside} {
			name := filepath.Base(dir)
			fmt.Fprintf(&b, `<img src=%q onload="window.got.push('img:%s')">`, smokeFileURL(filepath.Join(dir, "img.svg")), name)
			fmt.Fprintf(&b, `<script src=%q></script>`, smokeFileURL(filepath.Join(dir, "s.js")))
			fmt.Fprintf(&b, `<iframe src=%q></iframe>`, smokeFileURL(filepath.Join(dir, "frame.html")))
			fmt.Fprintf(&b, `<script>setTimeout(() => { const f = document.createElement("iframe"); f.src = %q; document.body.appendChild(f) }, 300)</script>`, smokeFileURL(filepath.Join(dir, "frame.html"))+"?late")
		}
		return b.String()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	evalString := func(access Access, pageID, code string) string {
		t.Helper()
		value, _, err := manager.Evaluate(ctx, access, pageID, code, nil)
		if err != nil {
			t.Fatalf("evaluate %s: %v", code, err)
		}
		return fmt.Sprint(value)
	}
	await := func(t *testing.T, what string, done func() bool) {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		for !done() {
			if waitCtx.Err() != nil {
				t.Fatalf("%s never happened", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	type side struct {
		access             Access
		own, other         string
		pageID             string
		ownName, otherName string
	}
	sides := []*side{
		{access: proj.access("policy-one", proj.one), own: one, other: two},
		{access: proj.access("policy-two", proj.two), own: two, other: one},
	}
	for _, s := range sides {
		s.ownName, s.otherName = filepath.Base(s.own), filepath.Base(s.other)
		t.Cleanup(func() {
			if err := manager.CloseThread(context.Background(), s.access.ThreadID); err != nil {
				t.Errorf("close %s: %v", s.access.ThreadID, err)
			}
		})
		if err := manager.SetJar(s.access, JarProject); err != nil {
			t.Fatalf("choose the project jar: %v", err)
		}
		info, err := manager.OpenFile(ctx, s.access, writeSmokeFile(t, s.own, "index.html", page(s.own)), OpenOptions{})
		if err != nil {
			t.Fatalf("open the workspace page: %v", err)
		}
		s.pageID = info.ID
	}
	if pageJar(t, manager, sides[0].pageID) != pageJar(t, manager, sides[1].pageID) {
		t.Fatal("the two worktrees do not share the project jar")
	}

	t.Run("subresources and subframes", func(t *testing.T) {
		for _, s := range sides {
			want := []string{"img:" + s.ownName, "frame:" + s.ownName + ":", "frame:" + s.ownName + ":?late"}
			await(t, s.ownName+"'s own files to load", func() bool {
				got := evalString(s.access, s.pageID, `JSON.stringify(window.got)`)
				for _, w := range want {
					if !strings.Contains(got, `"`+w+`"`) {
						return false
					}
				}
				return true
			})
			// The late frames were added 300 ms after load and loaded; any
			// refused file would have arrived by now.
			got := evalString(s.access, s.pageID, `JSON.stringify(window.got) + " ran=" + JSON.stringify(window.ran)`)
			for _, foreign := range []string{s.otherName, filepath.Base(outside)} {
				if strings.Contains(got, ":"+foreign) || strings.Contains(got, `"`+foreign+`"`) {
					t.Errorf("%s's page reached %s's files: %s", s.ownName, foreign, got)
				}
			}
			if !strings.Contains(got, `"`+s.ownName+`"`) {
				t.Errorf("%s's own script did not run: %s", s.ownName, got)
			}
		}
	})

	t.Run("popups", func(t *testing.T) {
		s := sides[0]
		for _, button := range []string{"popup", "noopener", "own"} {
			if _, err := manager.Click(ctx, s.access, s.pageID, "#"+button); err != nil {
				t.Fatalf("click %s: %v", button, err)
			}
		}
		await(t, "the opener's own file to load in a popup", func() bool {
			return smokePopupsShow(ctx, t, manager, s.access, s.pageID, "secret-"+s.ownName)
		})
		if n := len(smokePopups(ctx, t, manager, s.access, s.pageID)); n != 3 {
			t.Fatalf("%d popups became tabs, want 3", n)
		}
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if smokePopupsShow(ctx, t, manager, s.access, s.pageID, "secret-"+filepath.Base(outside)) {
				t.Fatal("a popup shows the outside file")
			}
		}
		if got := evalString(s.access, s.pageID, `window.r1 + " " + window.r2`); got != "[object Window] null" {
			t.Errorf("window.open answered %q, want a window and null for noopener", got)
		}
	})

	t.Run("a window opened with no gesture", func(t *testing.T) {
		s := sides[1]
		before := len(smokePopups(ctx, t, manager, s.access, s.pageID))
		if got := evalString(s.access, s.pageID, fmt.Sprintf(`String(window.open(%q))`, smokeFileURL(filepath.Join(s.own, "secret.html")))); got != "null" {
			t.Errorf("window.open with no gesture answered %s; headless Chromium blocks it", got)
		}
		time.Sleep(time.Second)
		if after := len(smokePopups(ctx, t, manager, s.access, s.pageID)); after != before {
			t.Errorf("a window opened with no gesture became a tab: %d tabs besides the opener, was %d", after, before)
		}
	})

	t.Run("documents", func(t *testing.T) {
		s := sides[1]
		index := filepath.Join(s.own, "index.html")
		// Each navigation starts from the workspace page: Chromium lets an
		// error page navigate nowhere.
		navigate := func(target string) {
			t.Helper()
			if _, err := manager.OpenFile(ctx, s.access, index, OpenOptions{PageID: s.pageID}); err != nil {
				t.Fatalf("reopen the workspace page: %v", err)
			}
			evalString(s.access, s.pageID, fmt.Sprintf(`location.href = %q; "ok"`, target))
		}
		body := func() string { return evalString(s.access, s.pageID, `document.body ? document.body.innerText : ""`) }
		// The workspace's own document loads, and so does the web.
		for target, want := range map[string]string{
			smokeFileURL(filepath.Join(s.own, "secret.html")): "secret-" + s.ownName,
			server.URL + "/web": "web-ok",
		} {
			navigate(target)
			await(t, "navigating to "+target, func() bool { return strings.Contains(body(), want) })
		}
		for _, target := range []string{
			smokeFileURL(filepath.Join(outside, "secret.html")),
			smokeFileURL(filepath.Join(s.other, "secret.html")),
			server.URL + "/redirect",
		} {
			navigate(target)
			time.Sleep(time.Second)
			if got := body(); strings.Contains(got, "secret-") {
				t.Errorf("navigating to %s loaded %q", target, got)
			}
		}
		if _, err := manager.Open(ctx, s.access, "chrome://version", OpenOptions{PageID: s.pageID}); err == nil {
			t.Error("the Manager opened chrome://version")
		}
	})
}
