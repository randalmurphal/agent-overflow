//go:build windows

package webview2host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The backend answers the navigation policy with one interception, on the
// browser session (internal/browser hosted_popups.go): it pauses document
// requests and file reads with Fetch.enable on no session, and answers each
// with the authority of the page its frame belongs to, by target id, by a
// frame the page reported, or by the opener of a popup not yet adopted.
// This holds the real runtime to what that relies on, playing the backend
// with a policy that refuses every file outside a workspace directory:
//
//   - a workspace page's own files load as a document, an image, a script
//     and a subframe (one added after load included), and the same from
//     outside the workspace never do;
//
//   - every pause on the browser session names a frame the backend can
//     attribute: the page's target, a frame its session reported first, or
//     a popup whose target names its opener, with or without a gesture and
//     with noopener;
//
//   - a popup a middle or Ctrl click opens has a target that names no
//     opener, so its first request waits for the launcher's ReportPopup;
//     that wait is logged and stays inside the backend's bound
//     (requestOwnerWait in internal/browser cdp_page.go);
//
//   - a redirect to an outside file and a renderer navigation to one never
//     load it.
//
//     AO_NATIVE_BROWSER_SMOKE=1 go test -run TestPolicySmoke ./internal/webview2host
func TestPolicySmokeOneBrowserWideInterception(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	workspace, outside := smokeTempDir(t, "ao-policy-smoke-ws-"), smokeTempDir(t, "ao-policy-smoke-out-")
	dataDir := smokeTempDir(t, "ao-policy-smoke-data-")
	fileURL := func(dir, name string) string { return "file:///" + filepath.ToSlash(filepath.Join(dir, name)) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, fileURL(outside, "secret.html"), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>web</title><body>web-ok"))
	}))
	t.Cleanup(server.Close)
	for _, dir := range []string{workspace, outside} {
		name := filepath.Base(dir)
		for file, content := range map[string]string{
			"img.svg":     `<svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"/>`,
			"s.js":        fmt.Sprintf(`window.ran = (window.ran || []).concat([%q]);`, name),
			"frame.html":  fmt.Sprintf(`<script>parent.postMessage("frame:%s:" + location.search, "*")</script>`, name),
			"secret.html": fmt.Sprintf(`<body>secret-%s</body>`, name),
		} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	var page strings.Builder
	page.WriteString(`<!doctype html><title>policy</title><script>window.got = []; addEventListener("message", (e) => window.got.push(String(e.data)));</script>`)
	for _, link := range []struct{ id, url string }{
		{"ctrl-out", fileURL(outside, "secret.html") + "?ctrl-out"},
		{"ctrl-own", fileURL(workspace, "secret.html") + "?ctrl-own"},
		{"middle-own", fileURL(workspace, "secret.html") + "?middle-own"},
	} {
		fmt.Fprintf(&page, `<a id=%q href=%q>%s</a> `, link.id, link.url, link.id)
	}
	for _, dir := range []string{workspace, outside} {
		name := filepath.Base(dir)
		fmt.Fprintf(&page, `<img src=%q onload="window.got.push('img:%s')">`, fileURL(dir, "img.svg"), name)
		fmt.Fprintf(&page, `<script src=%q></script>`, fileURL(dir, "s.js"))
		fmt.Fprintf(&page, `<iframe src=%q></iframe>`, fileURL(dir, "frame.html"))
		fmt.Fprintf(&page, `<script>setTimeout(() => { const f = document.createElement("iframe"); f.src = %q; document.body.appendChild(f) }, 300)</script>`, fileURL(dir, "frame.html")+"?late")
	}
	if err := os.WriteFile(filepath.Join(workspace, "index.html"), []byte(page.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	ui := startSmokeUIThread(t)
	watch := watchSmokeWindows(t, ui.hwnd)
	reports := make(chan smokeReport, 1024)
	host, err := New(Config{
		HostWindow: func() uintptr { return ui.hwnd }, UserDataDir: dataDir, Logf: t.Logf,
		Report: func(pageID string, kind ReportKind, detail string) { reports <- smokeReport{pageID, kind, detail} },
		OnMain: ui.do,
		Post:   ui.post,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.Close()
		ui.do(host.env.release)
	})
	host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	var target string
	for target == "" {
		select {
		case report := <-reports:
			if report.pageID == "page-1" && report.kind == ReportCreated {
				target = report.detail
			}
		case <-time.After(60 * time.Second):
			t.Fatal("page-1 was never created")
		}
	}
	host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 0, Y: 0, W: 640, H: 480})
	host.Apply(Directive{Op: OpShow, PageID: "page-1"})

	allow := func(url string) bool {
		return !strings.HasPrefix(url, "file:") || strings.HasPrefix(strings.ToLower(url), strings.ToLower(fileURL(workspace, "")))
	}
	b := newPolicyBackend(t, smokeDialCDP(t, host.CDPPort()), target, allow)
	go func() {
		for report := range reports {
			if report.kind != ReportPopup {
				continue
			}
			popup, err := ParsePopupReport(report.detail)
			if err != nil {
				t.Errorf("popup report %q: %v", report.detail, err)
				continue
			}
			if popup.Opener == "page-1" {
				b.reported(popup.Target)
			}
		}
	}()
	b.call("Target.setDiscoverTargets", map[string]any{"discover": true}, "")
	b.call("Fetch.enable", map[string]any{"patterns": []map[string]any{
		{"resourceType": "Document", "requestStage": "Request"},
		{"urlPattern": "file://*", "requestStage": "Request"},
	}}, "")
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(b.call("Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, ""), &attached); err != nil {
		t.Fatal(err)
	}
	session := attached.SessionID
	b.call("Page.enable", map[string]any{}, session)
	eval := func(expression string, gesture bool) string {
		t.Helper()
		var result struct {
			Result struct {
				Value any `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(b.call("Runtime.evaluate", map[string]any{"expression": expression, "userGesture": gesture, "returnByValue": true}, session), &result); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(result.Result.Value)
	}
	await := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !done(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s never happened", what)
			}
		}
	}
	step := func(name string, run func()) {
		t.Helper()
		t.Logf("---- %s", name)
		if _, err := watch.take(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		run()
		time.Sleep(300 * time.Millisecond)
		seen, err := watch.take()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, w := range seen {
			t.Logf("%s: a window appeared: %s", name, w)
			if !w.statusBubble(ui.hwnd) {
				t.Fatalf("%s: a window of WebView2's own appeared: %s", name, w)
			}
		}
	}
	navigate := func(url string) {
		t.Helper()
		b.call("Page.navigate", map[string]any{"url": url}, session)
	}
	index := fileURL(workspace, "index.html")
	// reload opens the workspace page and waits for its loads, so leaving it
	// cancels no request still paused (WebView2 then refuses its answer).
	reload := func() {
		t.Helper()
		navigate(index + "?again")
		await("the workspace page to load", func() bool {
			return eval(`String(document.readyState === "complete" && (window.got || []).length >= 2)`, false) == "true"
		})
	}
	own, out := filepath.Base(workspace), filepath.Base(outside)

	step("a workspace page's files", func() {
		navigate(index)
		await("the workspace's own files to load", func() bool {
			got := eval(`JSON.stringify(window.got || [])`, false)
			return strings.Contains(got, `"img:`+own+`"`) && strings.Contains(got, `"frame:`+own+`:"`) && strings.Contains(got, `"frame:`+own+`:?late"`)
		})
		got := eval(`JSON.stringify(window.got) + " ran=" + JSON.stringify(window.ran)`, false)
		if strings.Contains(got, out) || !strings.Contains(got, `ran=["`+own+`"]`) {
			t.Fatalf("the page loaded %s", got)
		}
	})
	step("popups", func() {
		eval(fmt.Sprintf(`String(window.open(%q))`, fileURL(outside, "secret.html")+"?gesture"), true)
		eval(fmt.Sprintf(`String(window.open(%q, "_blank", "noopener"))`, fileURL(outside, "secret.html")+"?noopener"), true)
		eval(fmt.Sprintf(`String(window.open(%q))`, fileURL(outside, "secret.html")+"?nogesture"), false)
		eval(fmt.Sprintf(`String(window.open(%q))`, fileURL(workspace, "secret.html")+"?own"), true)
		for _, query := range []string{"?gesture", "?noopener", "?nogesture", "?own"} {
			await("the popup's request "+query, func() bool { return len(b.answered(query)) > 0 })
		}
		for _, query := range []string{"?gesture", "?noopener", "?nogesture"} {
			if got := b.answered(query); got[0] != "refused by opener" {
				t.Fatalf("the outside popup %s was %v", query, got)
			}
		}
		if got := b.answered("?own"); got[0] != "allowed by opener" {
			t.Fatalf("the workspace popup was %v", got)
		}
	})
	step("clicked popups", func() {
		clicks := []struct {
			id, button string
			modifiers  int
		}{{"ctrl-out", "left", 2}, {"ctrl-own", "left", 2}, {"middle-own", "middle", 0}}
		for _, click := range clicks {
			var xy []float64
			if err := json.Unmarshal([]byte(eval(`JSON.stringify((() => { const r = document.getElementById("`+click.id+`").getBoundingClientRect(); return [r.x + r.width / 2, r.y + r.height / 2] })())`, false)), &xy); err != nil || len(xy) != 2 {
				t.Fatalf("locate %s: %v", click.id, err)
			}
			buttons := map[string]int{"left": 1, "middle": 4}[click.button]
			for _, kind := range []string{"mousePressed", "mouseReleased"} {
				b.call("Input.dispatchMouseEvent", map[string]any{
					"type": kind, "x": xy[0], "y": xy[1], "button": click.button, "buttons": buttons, "clickCount": 1, "modifiers": click.modifiers,
				}, session)
			}
			query := "?" + click.id
			await("the clicked popup's request "+query, func() bool { return len(b.answered(query)) > 0 })
			want := "refused by "
			if strings.HasSuffix(click.id, "-own") {
				want = "allowed by "
			}
			got := b.answered(query)
			if !strings.HasPrefix(got[0], want) || strings.HasSuffix(got[0], "nobody") {
				t.Fatalf("the popup %s was %v", query, got)
			}
			wait := b.waited(query)
			t.Logf("the popup %s was %s after waiting %v for its owner (negative: its report came first)", query, got[0], wait)
			if wait > policySmokeOwnerWait {
				t.Fatalf("the popup %s waited %v for its owner, past the backend's %v", query, wait, policySmokeOwnerWait)
			}
		}
	})
	step("documents", func() {
		for _, url := range []string{fileURL(outside, "secret.html") + "?location", server.URL + "/redirect"} {
			reload()
			eval(fmt.Sprintf(`location.href = %q; "ok"`, url), true)
			time.Sleep(time.Second)
			if body := eval(`document.body ? document.body.innerText : ""`, false); strings.Contains(body, "secret-") {
				t.Fatalf("navigating to %s loaded %q", url, body)
			}
		}
		reload()
		eval(fmt.Sprintf(`location.href = %q; "ok"`, fileURL(workspace, "secret.html")), true)
		await("the workspace's own document", func() bool {
			return strings.Contains(eval(`document.body ? document.body.innerText : ""`, false), "secret-"+own)
		})
	})
	if unattributed := b.unattributed(); len(unattributed) > 0 {
		t.Fatalf("pauses the backend could not attribute: %v", unattributed)
	}
}

func smokeTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for start := time.Now(); ; time.Sleep(200 * time.Millisecond) {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if time.Since(start) > 15*time.Second {
				t.Errorf("remove %s: %v", dir, err)
				return
			}
		}
	})
	return dir
}

// policySmokeOwnerWait is internal/browser's requestOwnerWait: how long the
// backend holds a paused request for its owner before refusing it.
const policySmokeOwnerWait = 2 * time.Second

// policyBackend is the backend's browser-level CDP connection: it answers
// each Fetch.requestPaused on the browser session as the policy does,
// attributing its frame from the events it read before it, or holding it
// for up to policySmokeOwnerWait until a target event or the launcher's
// popup report names its owner.
type policyBackend struct {
	t       *testing.T
	conn    *websocket.Conn
	target  string
	allow   func(url string) bool
	next    atomic.Int64
	mu      sync.Mutex
	waiters map[int64]chan json.RawMessage
	frames  map[string]bool
	openers map[string]string
	// answers are each paused URL's answers, "allowed by main" and so on.
	answers map[string][]string
	// pageSession counts pauses on any session but the browser's.
	pageSession int
	// sent names each answer by its command id.
	sent map[int64]string
	// popups are the targets the launcher reported page-1 opened, with
	// when the report arrived.
	popups map[string]time.Time
	// held are the pauses waiting for their owner, by frame.
	held map[string][]heldPause
	// waits is how long each paused URL waited for its owner; negative
	// when its popup's report came that long before the pause.
	waits map[string]time.Duration
}

type heldPause struct {
	requestID, url string
	at             time.Time
}

func newPolicyBackend(t *testing.T, conn *websocket.Conn, target string, allow func(string) bool) *policyBackend {
	b := &policyBackend{
		t: t, conn: conn, target: target, allow: allow,
		waiters: map[int64]chan json.RawMessage{}, frames: map[string]bool{},
		openers: map[string]string{}, answers: map[string][]string{}, sent: map[int64]string{},
		popups: map[string]time.Time{}, held: map[string][]heldPause{}, waits: map[string]time.Duration{},
	}
	b.next.Store(1000)
	t.Cleanup(func() { _ = conn.CloseNow() })
	go b.read()
	return b
}

func (b *policyBackend) write(method string, params any, session string) int64 {
	id := b.next.Add(1)
	frame := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		frame["sessionId"] = session
	}
	body, err := json.Marshal(frame)
	if err != nil {
		b.t.Error(err)
		return id
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.conn.Write(ctx, websocket.MessageText, body); err != nil {
		b.t.Errorf("send %s: %v", method, err)
	}
	return id
}

func (b *policyBackend) call(method string, params any, session string) json.RawMessage {
	b.t.Helper()
	reply := make(chan json.RawMessage, 1)
	b.mu.Lock()
	id := b.next.Add(1)
	b.waiters[id] = reply
	b.mu.Unlock()
	frame := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		frame["sessionId"] = session
	}
	body, err := json.Marshal(frame)
	if err != nil {
		b.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.conn.Write(ctx, websocket.MessageText, body); err != nil {
		b.t.Fatalf("send %s: %v", method, err)
	}
	select {
	case result := <-reply:
		return result
	case <-time.After(20 * time.Second):
		b.t.Fatalf("no reply to %s", method)
	}
	return nil
}

func (b *policyBackend) read() {
	for {
		_, payload, err := b.conn.Read(context.Background())
		if err != nil {
			return
		}
		var message struct {
			ID        int64           `json:"id"`
			Method    string          `json:"method"`
			SessionID string          `json:"sessionId"`
			Params    json.RawMessage `json:"params"`
			Result    json.RawMessage `json:"result"`
			Error     *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &message); err != nil {
			b.t.Errorf("CDP frame %q: %v", payload, err)
			return
		}
		if message.Method == "" {
			b.mu.Lock()
			reply := b.waiters[message.ID]
			delete(b.waiters, message.ID)
			sent := b.sent[message.ID]
			b.mu.Unlock()
			if message.Error != nil {
				b.t.Errorf("command %d (%s) refused: %s", message.ID, sent, message.Error.Message)
			}
			if reply != nil {
				reply <- message.Result
			}
			continue
		}
		b.event(message.Method, message.SessionID, message.Params)
	}
}

func (b *policyBackend) event(method, session string, params json.RawMessage) {
	var p struct {
		FrameID    string `json:"frameId"`
		RequestID  string `json:"requestId"`
		TargetInfo struct {
			TargetID string `json:"targetId"`
			OpenerID string `json:"openerId"`
		} `json:"targetInfo"`
		Request struct {
			URL string `json:"url"`
		} `json:"request"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		b.t.Errorf("%s %s: %v", method, params, err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch method {
	case "Page.frameAttached":
		b.frames[p.FrameID] = true
		b.releaseLocked(p.FrameID)
	case "Target.targetCreated", "Target.targetInfoChanged":
		if p.TargetInfo.OpenerID != "" {
			b.openers[p.TargetInfo.TargetID] = p.TargetInfo.OpenerID
			b.releaseLocked(p.TargetInfo.TargetID)
		}
	case "Fetch.requestPaused":
		if session != "" {
			b.pageSession++
			return
		}
		pause := heldPause{requestID: p.RequestID, url: p.Request.URL, at: time.Now()}
		if owner := b.ownerLocked(p.FrameID); owner != "" {
			if owner == "report" {
				b.waits[pause.url] = -pause.at.Sub(b.popups[p.FrameID])
			}
			b.answerLocked(pause, owner)
			return
		}
		frame := p.FrameID
		b.held[frame] = append(b.held[frame], pause)
		time.AfterFunc(policySmokeOwnerWait, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			for _, held := range b.held[frame] {
				b.answerLocked(held, "nobody")
			}
			delete(b.held, frame)
		})
	}
}

// reported records that the launcher reported page-1 opened target.
func (b *policyBackend) reported(target string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.popups[target] = time.Now()
	b.releaseLocked(target)
}

// ownerLocked names what attributes frame to page-1, or "" for nothing yet.
func (b *policyBackend) ownerLocked(frame string) string {
	switch {
	case frame == b.target:
		return "main"
	case b.frames[frame]:
		return "frame"
	case b.openers[frame] == b.target:
		return "opener"
	case !b.popups[frame].IsZero():
		return "report"
	}
	return ""
}

// releaseLocked answers the pauses held for frame once it has an owner.
func (b *policyBackend) releaseLocked(frame string) {
	owner := b.ownerLocked(frame)
	if owner == "" {
		return
	}
	for _, held := range b.held[frame] {
		b.waits[held.url] = time.Since(held.at)
		b.answerLocked(held, owner)
	}
	delete(b.held, frame)
}

func (b *policyBackend) answerLocked(pause heldPause, owner string) {
	answer := "refused by " + owner
	if owner != "nobody" && b.allow(pause.url) {
		answer = "allowed by " + owner
		b.sent[b.write("Fetch.continueRequest", map[string]any{"requestId": pause.requestID}, "")] = "continue " + pause.url
	} else {
		b.sent[b.write("Fetch.failRequest", map[string]any{"requestId": pause.requestID, "errorReason": "BlockedByClient"}, "")] = "fail " + pause.url
	}
	b.answers[pause.url] = append(b.answers[pause.url], answer)
}

// waited answers how long the URL ending in suffix waited for its owner.
func (b *policyBackend) waited(suffix string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	for url, wait := range b.waits {
		if strings.HasSuffix(url, suffix) {
			return wait
		}
	}
	return 0
}

// answered answers the answers given to the URL ending in suffix.
func (b *policyBackend) answered(suffix string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for url, answers := range b.answers {
		if strings.HasSuffix(url, suffix) {
			return append([]string(nil), answers...)
		}
	}
	return nil
}

// unattributed lists each URL a pause of which no owner answered for, and
// any pause on a page session, which nothing enabled.
func (b *policyBackend) unattributed() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for url, answers := range b.answers {
		for _, answer := range answers {
			if strings.HasSuffix(answer, " by nobody") {
				out = append(out, url)
			}
		}
	}
	if b.pageSession > 0 {
		out = append(out, fmt.Sprintf("%d pauses on a page session", b.pageSession))
	}
	return out
}
