//go:build windows

package webview2host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A page whose renderer dies is reported as ProcessFailed with the kind the
// backend treats as a crash (render-process-exited; browser.processFailure
// Crashes), the backend's close then releases its controller, and the host
// goes on creating and loading pages. The renderer is ended with CDP's
// Page.crash through the pane environment's debugging port, the endpoint
// the relay uses.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestCrashSmoke ./internal/webview2host
func TestCrashSmokeReportsTheRendererAndKeepsTheHost(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	dataDir, err := os.MkdirTemp("", "ao-crash-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := clearUserDataDir(dataDir); err != nil {
			t.Errorf("clear %s: %v", dataDir, err)
		}
		if err := os.Remove(dataDir); err != nil {
			t.Errorf("remove %s: %v", dataDir, err)
		}
	})
	ui := startSmokeUIThread(t)
	reports := make(chan smokeReport, 64)
	host, err := New(Config{
		HostWindow:  func() uintptr { return ui.hwnd },
		UserDataDir: dataDir,
		Logf:        t.Logf,
		Report: func(pageID string, kind ReportKind, detail string) {
			reports <- smokeReport{pageID, kind, detail}
		},
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
	// await answers the first report of kind want for pageID, keeping the
	// process failures it passes.
	var failures []smokeReport
	await := func(pageID string, want ReportKind) smokeReport {
		t.Helper()
		deadline := time.After(60 * time.Second)
		for {
			select {
			case report := <-reports:
				if report.kind == ReportProcessFailed {
					failures = append(failures, report)
				}
				if report.pageID == pageID && report.kind == want {
					return report
				}
			case <-deadline:
				t.Fatalf("no %s report for %s; process failures %+v", want, pageID, failures)
			}
		}
	}
	viewOf := func(pageID string) *iCoreWebView2 {
		var view *iCoreWebView2
		ui.do(func() {
			if page := host.pages[pageID]; page != nil {
				view = page.view
			}
		})
		return view
	}
	load := func(pageID, title string) {
		t.Helper()
		var navErr error
		ui.do(func() { navErr = smokeNavigateToString(viewOf(pageID), "<title>"+title+"</title>") })
		if navErr != nil {
			t.Fatalf("NavigateToString: %v", navErr)
		}
		deadline := time.Now().Add(20 * time.Second)
		for got := ""; got != title; time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("page %s never loaded; title %q", pageID, got)
			}
			var titleErr error
			ui.do(func() { got, titleErr = smokeDocumentTitle(viewOf(pageID)) })
			if titleErr != nil {
				t.Fatal(titleErr)
			}
		}
	}

	host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	targetID := strings.TrimSpace(await("page-1", ReportCreated).detail)
	if targetID == "" {
		t.Fatal("the created page has no CDP target")
	}
	load("page-1", "ao crash smoke")

	conn := smokeDialCDP(t, host.CDPPort())
	defer func() { _ = conn.CloseNow() }()
	crashTarget(t, conn, targetID)

	failed := await("page-1", ReportProcessFailed)
	if failed.detail != "render-process-exited" {
		t.Fatalf("the renderer's death was reported as %q, want render-process-exited", failed.detail)
	}

	// The backend closes a crashed page's engine page (pageCrashed), which
	// sends the launcher this close.
	host.Apply(Directive{Op: OpClose, PageID: "page-1"})
	await("page-1", ReportClosed)
	if viewOf("page-1") != nil {
		t.Fatal("the crashed page's controller is still held")
	}

	host.Apply(Directive{Op: OpCreate, PageID: "page-2", ProfileID: "smoke"})
	await("page-2", ReportCreated)
	load("page-2", "ao crash smoke after")
	for _, report := range failures {
		if report.pageID != "page-1" {
			t.Errorf("a page that did not crash reported %+v", report)
		}
	}
}

// crashTarget ends targetID's renderer: it attaches to the target on conn
// and sends Page.crash, which the renderer never answers.
func crashTarget(t *testing.T, conn *websocket.Conn, targetID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	attach := fmt.Sprintf(`{"id":2,"method":"Target.attachToTarget","params":{"targetId":%q,"flatten":true}}`, targetID)
	if err := conn.Write(ctx, websocket.MessageText, []byte(attach)); err != nil {
		t.Fatalf("attach to %s: %v", targetID, err)
	}
	var sessionID string
	for sessionID == "" {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("attach to %s: %v", targetID, err)
		}
		var reply struct {
			ID     int64 `json:"id"`
			Result struct {
				SessionID string `json:"sessionId"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &reply); err != nil {
			t.Fatalf("CDP frame %q: %v", payload, err)
		}
		if reply.ID != 2 {
			continue
		}
		if reply.Error != nil {
			t.Fatalf("attach to %s: %s", targetID, reply.Error.Message)
		}
		sessionID = reply.Result.SessionID
	}
	crash := fmt.Sprintf(`{"id":3,"method":"Page.crash","sessionId":%q}`, sessionID)
	if err := conn.Write(ctx, websocket.MessageText, []byte(crash)); err != nil {
		t.Fatalf("Page.crash: %v", err)
	}
}
