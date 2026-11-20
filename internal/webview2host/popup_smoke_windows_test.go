//go:build windows

package webview2host

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The launcher makes every window a pane page opens a page of its own
// (popup_windows.go). This runs that path on the real runtime, on the
// smoke's own desktop, playing the backend: it reads each popup report and
// drives the popup through its CDP target. It proves:
//
//   - window.open with and without a user gesture, a target=_blank link and
//     a form each become a reported popup, first among its page's reports,
//     whose target names its opener, and no window of WebView2's own
//     appears; window features change nothing, and the popup starts at its
//     opener's bounds;
//   - a middle, Ctrl or Shift click on a plain link each becomes a reported
//     popup with no opener, its target naming none, and the opener stays;
//   - a window.open popup has window.opener and its postMessage reaches the
//     opener; noopener, a link and a form leave the popup none, and
//     window.open answers null for noopener;
//   - a request the launcher refuses opens nothing and window.open answers
//     null;
//   - window.close() in a popup closes its page, reported closed, and so
//     does window.close() in a created page with one history entry;
//   - a popup's own popup is reported with the popup as its opener;
//   - a download in a popup is the launcher's, reported under the popup;
//     a popup whose first navigation is a download asks to close, stays
//     open while the download runs and closes when it ends, and the
//     download dialog WebView2 opens on its opener meanwhile is closed at
//     once; one whose download ends at once is reported first, then its
//     finished download, then its close.
//
// Every file it makes carries a per-run prefix, and exactly those files are
// deleted from the Downloads folder when it ends.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestPopupSmoke ./internal/webview2host
func TestPopupSmokeMakesEveryWindowAPage(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	s := startPopupSmoke(t)

	// window.open with a gesture: the opener waits in it until the popup
	// is the launcher's, and it answers the window.
	s.step("window.open with a gesture", nil, func() {
		if got := s.eval(s.opener, `String(window.open("/popup?k=gesture", "named") !== null)`, true); got != "true" {
			t.Fatalf("window.open answered %s", got)
		}
		gesture := s.popup("page-1")
		if title := s.title(gesture.id); title != "opener:yes:gesture:named" {
			t.Fatalf("the popup reads %q", title)
		}
		s.expectMessages("hello:gesture")
		var opener, popup rect
		s.ui.do(func() {
			opener, _ = s.page("page-1").controller.bounds()
			popup, _ = s.page(gesture.id).controller.bounds()
		})
		if opener != popup {
			t.Fatalf("the popup starts at %+v, its opener is at %+v", popup, opener)
		}
	})
	s.step("window.open with no gesture", nil, func() {
		if got := s.eval(s.opener, `String(window.open("/popup?k=nogesture") !== null)`, false); got != "true" {
			t.Fatalf("window.open with no gesture answered %s", got)
		}
		if title := s.title(s.popup("page-1").id); title != "opener:yes:nogesture:" {
			t.Fatalf("the popup reads %q", title)
		}
	})
	s.step("window features", nil, func() {
		s.eval(s.opener, `String(window.open("/popup?k=features", "f", "width=300,height=200,left=10,top=20") !== null)`, true)
		s.title(s.popup("page-1").id)
	})
	s.step("noopener", nil, func() {
		if got := s.eval(s.opener, `String(window.open("/popup?k=noopener", "_blank", "noopener"))`, true); got != "null" {
			t.Fatalf("window.open with noopener answered %s", got)
		}
		if title := s.title(s.popup("page-1").id); title != "opener:no:noopener:" {
			t.Fatalf("the noopener popup reads %q", title)
		}
	})
	s.step("a target=_blank link", nil, func() {
		s.click(s.opener, "blank")
		if title := s.title(s.popup("page-1").id); title != "opener:no:link:" {
			t.Fatalf("the link's popup reads %q", title)
		}
	})
	s.step("a target=_blank form", nil, func() {
		s.click(s.opener, "submit")
		if title := s.title(s.popup("page-1").id); title != "opener:no:form:" {
			t.Fatalf("the form's popup reads %q", title)
		}
	})
	// A click that opens a link in a new tab or window, with the middle
	// button, Ctrl or Shift, is a window request too.
	for _, click := range []struct {
		name      string
		button    string
		modifiers int
	}{
		{"a middle click on a link", "middle", 0},
		{"a Ctrl click on a link", "left", cdpModifierCtrl},
		{"a Shift click on a link", "left", cdpModifierShift},
	} {
		s.step(click.name, nil, func() {
			s.clickWith(s.opener, "plain", click.button, click.modifiers)
			popup := s.popupOpenedBy("page-1", false)
			if title := s.title(popup.id); title != "opener:no:plain:" {
				t.Fatalf("the click's popup reads %q", title)
			}
			if got := s.eval(s.opener, `location.pathname`, false); got != "/opener" {
				t.Fatalf("the click navigated the opener to %s", got)
			}
		})
	}
	s.step("a refused request", nil, func() {
		s.ui.do(func() { s.host.pendingPopups = MaxPendingPopups })
		got := s.eval(s.opener, `String(window.open("/popup?k=refused"))`, true)
		s.ui.do(func() { s.host.pendingPopups = 0 })
		if got != "null" {
			t.Fatalf("a refused window.open answered %s", got)
		}
		if extra := s.quiet(time.Second); len(extra) != 0 {
			t.Fatalf("a refused request reported %+v", extra)
		}
	})
	s.step("window.close in a popup", nil, func() {
		s.eval(s.opener, `String(window.open("/closeme") !== null)`, true)
		closing := s.popup("page-1")
		s.report(closing.id, ReportClosed)
		if s.page(closing.id) != nil {
			t.Fatal("a popup that closed itself is still a page")
		}
		s.expectMessages("done")
	})
	s.step("a popup of a popup", nil, func() {
		s.eval(s.opener, `String(window.open("/popup?k=child") !== null)`, true)
		child := s.popup("page-1")
		s.title(child.id)
		if got := s.eval(child.session, `String(window.open("/popup?k=grandchild") !== null)`, true); got != "true" {
			t.Fatalf("the popup's window.open answered %s", got)
		}
		grandchild := s.popup(child.id)
		if title := s.title(grandchild.id); title != "opener:yes:grandchild:" {
			t.Fatalf("the popup's popup reads %q", title)
		}
		// Its opener is the popup, and its message reaches it.
		if got := s.eval(child.session, `JSON.stringify(window.__msgs || [])`, false); !strings.Contains(got, "hello:grandchild") {
			t.Fatalf("the popup heard %s", got)
		}
		s.host.Apply(Directive{Op: OpClose, PageID: grandchild.id})
		s.report(grandchild.id, ReportClosed)
	})
	s.step("a download in a popup", nil, func() {
		s.eval(s.opener, `String(window.open("/popup?k=dl") !== null)`, true)
		popup := s.popup("page-1")
		s.title(popup.id)
		s.eval(popup.session, `location.href = "/cd/later.bin"; "ok"`, true)
		s.downloaded(popup.id, "later.bin")
		s.noDialog(popup.id)
		s.noDialog("page-1")
	})
	// WebView2 asks to close a popup whose first navigation is a download,
	// then opens its opener's download dialog, a window of its own, which
	// the launcher closes as it opens.
	s.step("a popup whose first navigation is a download", s.dialogFlash, func() {
		s.eval(s.opener, `String(window.open("/slow/first.bin") !== null)`, true)
		popup := s.popup("page-1")
		// WebView2 asks to close the popup within a moment of the start.
		time.Sleep(2 * time.Second)
		if s.page(popup.id) == nil {
			t.Fatal("the popup closed while its download ran")
		}
		s.noDialog("page-1")
		close(s.release)
		s.downloaded(popup.id, "first.bin")
		s.report(popup.id, ReportClosed)
		s.noDialog("page-1")
	})
	// The same with a download that ends at once: the popup closes as soon
	// as it asks, maybe before its target is read, and is still reported
	// first, then its finished download, then its close.
	s.step("a popup whose first navigation is a download that ends at once", s.dialogFlash, func() {
		s.eval(s.opener, `String(window.open("/cd/fast.bin") !== null)`, true)
		s.fastDownloadPopup("fast.bin")
		s.noDialog("page-1")
	})
	for id, kind := range s.firstKind {
		if strings.HasPrefix(id, "popup-") && kind != ReportPopup {
			t.Errorf("popup %s reported %s before its popup report", id, kind)
		}
	}
	// Last, as it closes the opener: window.close() in a created page with
	// one history entry closes it, as on every engine.
	s.step("window.close in a created page", nil, func() {
		s.eval(s.opener, `window.close(); "called"`, true)
		s.report("page-1", ReportClosed)
	})
}

// popupSmoke is the backend's side of one pane page and its popups.
type popupSmoke struct {
	t       *testing.T
	ui      *smokeUIThread
	host    *Host
	cdp     *smokeCDP
	server  *httptest.Server
	reports chan smokeReport
	prefix  string
	// opener is page-1's CDP session, and openerTarget its target.
	opener, openerTarget string
	// firstKind is each page's first report.
	firstKind map[string]ReportKind
	// sessions are the CDP sessions of the pages the smoke drives.
	sessions map[string]string
	windows  *smokeWindowWatch
	// release lets a /slow/ download finish.
	release chan struct{}
}

// smokePopup is one reported popup the smoke drives.
type smokePopup struct {
	id, target, session string
}

func startPopupSmoke(t *testing.T) *popupSmoke {
	downloads, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := &popupSmoke{
		t: t, prefix: "ao-popup-smoke-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-",
		reports: make(chan smokeReport, 1024), firstKind: map[string]ReportKind{},
		sessions: map[string]string{}, release: make(chan struct{}),
	}
	// Registered first, so it runs last: after the host is gone.
	t.Cleanup(func() {
		for start := time.Now(); ; time.Sleep(200 * time.Millisecond) {
			matches, _ := filepath.Glob(filepath.Join(downloads, s.prefix+"*"))
			var failed []error
			for _, match := range matches {
				if err := os.Remove(match); err != nil {
					failed = append(failed, err)
				}
			}
			if len(failed) == 0 {
				return
			}
			if time.Since(start) > 15*time.Second {
				t.Errorf("remove the smoke's files: %v", failed)
				return
			}
		}
	})
	dataDir := smokeTempDir(t, "ao-popup-smoke-")
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	t.Cleanup(func() {
		select {
		case <-s.release:
		default:
			close(s.release)
		}
	})

	s.ui = startSmokeUIThread(t)
	s.windows = watchSmokeWindows(t, s.ui.hwnd)
	s.host, err = New(Config{
		HostWindow:  func() uintptr { return s.ui.hwnd },
		UserDataDir: dataDir,
		Logf:        t.Logf,
		Report: func(pageID string, kind ReportKind, detail string) {
			s.reports <- smokeReport{pageID, kind, detail}
		},
		OnMain: s.ui.do,
		Post:   s.ui.post,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.host.Close()
		s.ui.do(s.host.env.release)
	})
	s.host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	s.openerTarget = s.report("page-1", ReportCreated).detail
	s.host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 0, Y: 0, W: 640, H: 480})
	s.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	s.cdp = newSmokeCDP(t, smokeDialCDP(t, s.host.CDPPort()))
	s.opener = s.attach(s.openerTarget)
	s.sessions["page-1"] = s.opener
	s.cdp.call("Page.navigate", map[string]any{"url": s.server.URL + "/opener"}, s.opener, nil)
	for s.eval(s.opener, `String(document.readyState === "complete" && !!window.__msgs)`, false) != "true" {
		time.Sleep(50 * time.Millisecond)
	}
	return s
}

// serve answers /opener, the page that opens windows; /popup?k=, a page
// that reads whether it has an opener and greets it; /closeme, one that
// closes itself; /cd/<name>, an attachment; and /slow/<name>, one whose
// second half waits for release.
func (s *popupSmoke) serve(w http.ResponseWriter, r *http.Request) {
	const collect = `<script>window.__msgs=[];addEventListener("message",e=>window.__msgs.push(String(e.data)));</script>`
	switch {
	case r.URL.Path == "/opener":
		_, _ = fmt.Fprint(w, `<!doctype html><title>opener</title><body style="margin:0">
<a id="blank" target="_blank" href="/popup?k=link" style="display:block;height:40px">blank</a>
<a id="plain" href="/popup?k=plain" style="display:block;height:40px">plain</a>
<form target="_blank" action="/popup"><input name="k" value="form"><button id="submit" style="height:40px">go</button></form>`+collect)
	case r.URL.Path == "/popup":
		k := r.URL.Query().Get("k")
		_, _ = fmt.Fprintf(w, `<!doctype html><title>loading</title>%s<script>
document.title = "opener:" + (window.opener ? "yes" : "no") + ":%s:" + window.name;
if (window.opener) window.opener.postMessage("hello:%s", "*");
</script>`, collect, k, k)
	case r.URL.Path == "/closeme":
		_, _ = fmt.Fprint(w, `<!doctype html><title>opener:closeme</title><script>
setTimeout(() => { window.opener.postMessage("done", "*"); window.close() }, 300);
</script>`)
	case strings.HasPrefix(r.URL.Path, "/cd/"), strings.HasPrefix(r.URL.Path, "/slow/"):
		kind, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+s.prefix+name+`"`)
		w.Header().Set("Content-Length", "2048")
		if kind == "slow" {
			_, _ = w.Write(make([]byte, 1024))
			w.(http.Flusher).Flush()
			select {
			case <-s.release:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(make([]byte, 1024))
			return
		}
		_, _ = w.Write(make([]byte, 2048))
	default:
		http.NotFound(w, r)
	}
}

// step runs one case and fails it if a top-level window other than the
// smoke's appeared meanwhile, unless allow accepts it.
func (s *popupSmoke) step(name string, allow func([]smokeWindowSeen) error, run func()) {
	s.t.Helper()
	s.t.Logf("---- %s", name)
	if err := s.windows.runStep(s.t, run, allow); err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
}

// dialogFlash accepts WebView2's download dialog, a window of its over the
// smoke's, shown for a moment as the launcher closes it, and nothing else.
func (s *popupSmoke) dialogFlash(seen []smokeWindowSeen) error {
	for _, w := range seen {
		switch {
		case !w.webView2Window(s.ui.hwnd):
			return fmt.Errorf("a window not WebView2's over the smoke's appeared: %s", w)
		case w.statusBubble(s.ui.hwnd):
		case w.gone.IsZero():
			return fmt.Errorf("a window stayed: %s", w)
		case w.gone.Sub(w.shown) > time.Second:
			return fmt.Errorf("a window showed for %v: %s", w.gone.Sub(w.shown), w)
		}
	}
	return nil
}

// next answers the next report, noting each page's first.
func (s *popupSmoke) next(within time.Duration) (smokeReport, bool) {
	select {
	case report := <-s.reports:
		if _, ok := s.firstKind[report.pageID]; !ok {
			s.firstKind[report.pageID] = report.kind
		}
		return report, true
	case <-time.After(within):
		return smokeReport{}, false
	}
}

// report waits for pageID's report of kind, passing over every other.
func (s *popupSmoke) report(pageID string, kind ReportKind) smokeReport {
	s.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		report, ok := s.next(time.Until(deadline))
		if !ok {
			s.t.Fatalf("no %s report for %s", kind, pageID)
		}
		if report.pageID == pageID && report.kind == kind {
			return report
		}
		s.t.Logf("report %s %s %.200s", report.pageID, report.kind, report.detail)
	}
}

// quiet answers the reports made within d.
func (s *popupSmoke) quiet(d time.Duration) []smokeReport {
	var out []smokeReport
	deadline := time.Now().Add(d)
	for {
		report, ok := s.next(time.Until(deadline))
		if !ok {
			return out
		}
		out = append(out, report)
	}
}

// popup waits for the next popup report, of a window opener opened, checks
// that its target names that opener, and attaches to it.
func (s *popupSmoke) popup(opener string) smokePopup {
	s.t.Helper()
	return s.popupOpenedBy(opener, true)
}

// popupOpenedBy waits for the next popup report, of a window opener opened,
// and attaches to it; its target names that opener as its CDP opener when
// targetOpener is set, and none when it is not.
func (s *popupSmoke) popupOpenedBy(opener string, targetOpener bool) smokePopup {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		report, ok := s.next(time.Until(deadline))
		if !ok {
			s.t.Fatalf("no popup report of %s", opener)
		}
		if report.kind != ReportPopup {
			s.t.Logf("report %s %s %.200s", report.pageID, report.kind, report.detail)
			continue
		}
		parsed, err := ParsePopupReport(report.detail)
		if err != nil {
			s.t.Fatal(err)
		}
		if parsed.Opener != opener {
			s.t.Fatalf("popup %s of %s, want one of %s", report.pageID, parsed.Opener, opener)
		}
		popup := smokePopup{id: report.pageID, target: parsed.Target, session: s.attach(parsed.Target)}
		var info struct {
			TargetInfo struct {
				OpenerID string `json:"openerId"`
				Type     string `json:"type"`
			} `json:"targetInfo"`
		}
		s.cdp.call("Target.getTargetInfo", map[string]any{"targetId": parsed.Target}, "", &info)
		openerTarget := s.openerTarget
		if opener != "page-1" {
			var own struct {
				TargetInfo struct {
					TargetID string `json:"targetId"`
				} `json:"targetInfo"`
			}
			s.cdp.call("Target.getTargetInfo", map[string]any{}, s.sessionOf(opener), &own)
			openerTarget = own.TargetInfo.TargetID
		}
		if !targetOpener {
			openerTarget = ""
		}
		if info.TargetInfo.OpenerID != openerTarget || info.TargetInfo.Type != "page" {
			s.t.Fatalf("popup target %s: %+v, want a page opened by %q", parsed.Target, info.TargetInfo, openerTarget)
		}
		s.t.Logf("popup %s of %s at %s: %s", popup.id, opener, popup.target, parsed.URL)
		s.sessions[popup.id] = popup.session
		return popup
	}
}

func (s *popupSmoke) sessionOf(pageID string) string { return s.sessions[pageID] }

func (s *popupSmoke) attach(targetID string) string {
	s.t.Helper()
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	s.cdp.call("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "", &attached)
	return attached.SessionID
}

// eval evaluates expression in session, as a user gesture or not, and
// answers its value as a string.
func (s *popupSmoke) eval(session, expression string, gesture bool) string {
	s.t.Helper()
	var result struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	s.cdp.call("Runtime.evaluate", map[string]any{"expression": expression, "userGesture": gesture, "returnByValue": true}, session, &result)
	if len(result.ExceptionDetails) > 0 {
		s.t.Fatalf("%s threw %s", expression, result.ExceptionDetails)
	}
	if result.Result.Value == nil {
		return "null"
	}
	return fmt.Sprint(result.Result.Value)
}

// click clicks the element with id in session with the left button.
func (s *popupSmoke) click(session, id string) {
	s.t.Helper()
	s.clickWith(session, id, "left", 0)
}

// Input.dispatchMouseEvent's modifier bits.
const (
	cdpModifierCtrl  = 2
	cdpModifierShift = 8
)

// clickWith clicks the element with id in session with button, holding
// the modifier keys modifiers names.
func (s *popupSmoke) clickWith(session, id, button string, modifiers int) {
	s.t.Helper()
	var xy []float64
	if err := json.Unmarshal([]byte(s.eval(session, `JSON.stringify((() => { const r = document.getElementById("`+id+`").getBoundingClientRect(); return [r.x + r.width / 2, r.y + r.height / 2] })())`, false)), &xy); err != nil || len(xy) != 2 {
		s.t.Fatalf("locate %s: %v", id, err)
	}
	buttons := map[string]int{"left": 1, "middle": 4}[button]
	for _, kind := range []string{"mousePressed", "mouseReleased"} {
		s.cdp.call("Input.dispatchMouseEvent", map[string]any{
			"type": kind, "x": xy[0], "y": xy[1], "button": button, "buttons": buttons, "clickCount": 1, "modifiers": modifiers,
		}, session, nil)
	}
}

// title waits for a popup's page to read whether it has an opener.
func (s *popupSmoke) title(pageID string) string {
	s.t.Helper()
	var title string
	for start := time.Now(); time.Since(start) < 10*time.Second; time.Sleep(50 * time.Millisecond) {
		s.ui.do(func() {
			if page := s.page(pageID); page != nil && page.view != nil {
				title, _ = smokeDocumentTitle(page.view)
			}
		})
		if strings.HasPrefix(title, "opener:") {
			return title
		}
	}
	s.t.Fatalf("popup %s reads %q", pageID, title)
	return ""
}

// expectMessages waits for the opener to have heard want.
func (s *popupSmoke) expectMessages(want string) {
	s.t.Helper()
	var got string
	for start := time.Now(); time.Since(start) < 10*time.Second; time.Sleep(50 * time.Millisecond) {
		if got = s.eval(s.opener, `JSON.stringify(window.__msgs.splice(0))`, false); strings.Contains(got, `"`+want+`"`) {
			return
		}
	}
	s.t.Fatalf("the opener heard %s, want %q", got, want)
}

// downloaded waits for a download of name in pageID to complete, reported
// under pageID, and checks its file.
func (s *popupSmoke) downloaded(pageID, name string) {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		report, ok := s.next(time.Until(deadline))
		if !ok {
			s.t.Fatalf("no completed download of %s in %s", name, pageID)
		}
		if report.kind != ReportDownload {
			s.t.Logf("report %s %s %.200s", report.pageID, report.kind, report.detail)
			continue
		}
		parsed, err := ParseDownloadReport(report.detail)
		if err != nil {
			s.t.Fatal(err)
		}
		if report.pageID != pageID {
			s.t.Fatalf("a download reported under %s, want %s: %+v", report.pageID, pageID, parsed)
		}
		if parsed.State != DownloadCompleted {
			continue
		}
		if filepath.Base(parsed.Path) != s.prefix+name || parsed.Received != 2048 {
			s.t.Fatalf("the download completed %+v", parsed)
		}
		if info, err := os.Stat(parsed.Path); err != nil || info.Size() != 2048 {
			s.t.Fatalf("the downloaded file: %v", err)
		}
		return
	}
}

// fastDownloadPopup waits for a popup of page-1 whose download of name
// ends at once: reported first, then its download completed with the
// file, then closed, and no page of the launcher's after.
func (s *popupSmoke) fastDownloadPopup(name string) {
	s.t.Helper()
	var popupID string
	completed := false
	deadline := time.Now().Add(30 * time.Second)
	for {
		report, ok := s.next(time.Until(deadline))
		if !ok {
			s.t.Fatalf("the popup %q never ended: completed %v", popupID, completed)
		}
		s.t.Logf("report %s %s %.200s", report.pageID, report.kind, report.detail)
		switch {
		case popupID == "" && report.kind == ReportPopup:
			parsed, err := ParsePopupReport(report.detail)
			if err != nil || parsed.Opener != "page-1" {
				s.t.Fatalf("popup report %+v: %v", parsed, err)
			}
			popupID = report.pageID
		case popupID == "" || report.pageID != popupID:
			s.t.Fatalf("a report of %s before the popup's: %s", report.pageID, report.kind)
		case report.kind == ReportDownload:
			parsed, err := ParseDownloadReport(report.detail)
			if err != nil {
				s.t.Fatal(err)
			}
			if parsed.State == DownloadCompleted {
				if filepath.Base(parsed.Path) != s.prefix+name || parsed.Received != 2048 {
					s.t.Fatalf("the download completed %+v", parsed)
				}
				completed = true
			}
		case report.kind == ReportClosed:
			if !completed {
				s.t.Fatal("the popup closed before its download completed")
			}
			if s.page(popupID) != nil {
				s.t.Fatal("the closed popup is still a page")
			}
			return
		}
	}
}

func (s *popupSmoke) noDialog(pageID string) {
	s.t.Helper()
	var open int32
	var err error
	s.ui.do(func() { open, err = smokeDownloadDialogOpen(s.page(pageID).view) })
	if err != nil || open != 0 {
		s.t.Fatalf("%s's download dialog is open (%d): %v", pageID, open, err)
	}
}

func (s *popupSmoke) page(pageID string) *hostPage {
	var page *hostPage
	s.ui.do(func() { page = s.host.pages[pageID] })
	return page
}
