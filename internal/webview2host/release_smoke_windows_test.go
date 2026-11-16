//go:build windows

package webview2host

import (
	"os"
	"testing"
	"time"
)

// A real WebView2 environment released after its last page closes comes back
// for the next create, on the same user-data folder and debugging port.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestReleaseSmoke ./internal/webview2host
func TestReleaseSmokeRecreatesTheEnvironment(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	dataDir, err := os.MkdirTemp("", "ao-release-smoke-")
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
	reports := make(chan smokeReport, 16)
	host, err := New(Config{
		HostWindow:  func() uintptr { return ui.hwnd },
		UserDataDir: dataDir,
		Logf:        t.Logf,
		Report: func(pageID string, kind ReportKind, detail string) {
			reports <- smokeReport{pageID, kind, detail}
		},
		OnMain: ui.do,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.Close()
		ui.do(host.env.release)
	})
	await := func(pageID string, want ReportKind) {
		t.Helper()
		deadline := time.After(60 * time.Second)
		for {
			select {
			case report := <-reports:
				if report.pageID != pageID || report.kind == ReportFindUnavailable {
					continue
				}
				if report.kind != want {
					t.Fatalf("page %s answered %+v, want %s", pageID, report, want)
				}
				return
			case <-deadline:
				t.Fatalf("no %s report for %s", want, pageID)
			}
		}
	}

	host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	await("page-1", ReportCreated)
	host.Apply(Directive{Op: OpRelease})
	if _, ok := host.env.current(); !ok {
		t.Fatal("released the environment of an open page")
	}
	host.Apply(Directive{Op: OpClose, PageID: "page-1"})
	await("page-1", ReportClosed)
	host.Apply(Directive{Op: OpRelease})
	if _, ok := host.env.current(); ok {
		t.Fatal("kept the environment with no page open")
	}

	host.Apply(Directive{Op: OpCreate, PageID: "page-2", ProfileID: "smoke"})
	await("page-2", ReportCreated)
}
