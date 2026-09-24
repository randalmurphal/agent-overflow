package main

import (
	"os"
	"strings"
	"testing"
)

// TestSignalWaitBootsBindThenStartThenMarkReady pins the headless boot
// order the Windows launcher depends on: the transport binds and
// publishes its port before App.Start, and MarkReady follows Start. The
// launcher reads the port first and then watches the bootstrap report
// the boot's progress until ready.
func TestSignalWaitBootsBindThenStartThenMarkReady(t *testing.T) {
	for _, path := range []string{"main.go", "main_serve.go", "main_harness.go", "main_soak.go"} {
		text := readRootSource(t, path)
		bind := strings.Index(text, "srv := bootTransport(")
		start := strings.Index(text, "appService.Start(bootCtx)")
		ready := strings.Index(text, "srv.MarkReady()")
		if bind < 0 || start < 0 || ready < 0 || !(bind < start && start < ready) {
			t.Fatalf("%s: bootTransport=%d App.Start=%d MarkReady=%d; want bind, then Start, then MarkReady", path, bind, start, ready)
		}
	}
}

// TestBootTransportReportsProgressBeforeServing: the readiness gate and
// the progress reporter are both in place before the listener serves, so
// no boot answers a bootstrap request with App state or with the bare 503.
func TestBootTransportReportsProgressBeforeServing(t *testing.T) {
	text := readRootSource(t, "main.go")
	body := text[strings.Index(text, "func bootTransport("):]
	readiness := strings.Index(body, "applyBootReadiness(&cfg)")
	construct := strings.Index(body, "transport.New(cfg)")
	progress := strings.Index(body, "appservice.SetBootProgress(appService.App, transport.NewStartupReporter(srv, ")
	serve := strings.Index(body, "srv.Start()")
	if readiness < 0 || construct < 0 || progress < 0 || serve < 0 || !(readiness < construct && construct < progress && progress < serve) {
		t.Fatalf("bootTransport: readiness=%d New=%d progress=%d Start=%d; want the gate and the reporter before the listener serves", readiness, construct, progress, serve)
	}
}

// TestLauncherBootsCancelStartOnShutdownRequest: the two boots the
// launcher can abandon before they are ready cancel App.Start when it
// asks them to stop.
func TestLauncherBootsCancelStartOnShutdownRequest(t *testing.T) {
	for _, path := range []string{"main.go", "main_soak.go"} {
		text := readRootSource(t, path)
		watch := strings.Index(text, "cancelBootOnShutdownRequest(bootCtx, bootCancel, shutdownRequested)")
		start := strings.Index(text, "appService.Start(bootCtx)")
		if watch < 0 || start < 0 || watch > start {
			t.Fatalf("%s: shutdown watch=%d App.Start=%d; want the watch armed before Start", path, watch, start)
		}
	}
}

// TestDesktopBootMarksReadyFromStartDone: the desktop Start runs on its
// own goroutine, so readiness is released by its completion hook.
func TestDesktopBootMarksReadyFromStartDone(t *testing.T) {
	text := readRootSource(t, "main_desktop.go")
	hook := strings.Index(text, "appservice.SetStartDone(appService.App, func(err error) {")
	if hook < 0 {
		t.Fatal("runDesktop installs no SetStartDone hook")
	}
	body := text[hook:]
	end := strings.Index(body, "\n\t\t\t})")
	if end < 0 {
		t.Fatal("SetStartDone hook body not found")
	}
	body = body[:end]
	for _, want := range []string{"srv.MarkReady()", "srv.MarkStartupFailed()", "go app.Quit()"} {
		if !strings.Contains(body, want) {
			t.Fatalf("SetStartDone hook lacks %q", want)
		}
	}
}

func readRootSource(t *testing.T, path string) string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(source)
}
