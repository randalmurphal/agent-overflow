//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"context"
	"strings"
	"testing"
	"unsafe"
)

// Engine selection is the one part of the WKWebView engine that can be proven
// without a window, and it is the part that decides what every OTHER
// environment gets: no window means NO engine, which is what keeps
// `--connect`, the harness, and `go test` itself off an in-process one.
// The windowless half of that rule is tag-free, in manager_test.go.

func TestNativeEngineNeedsAWindowToExist(t *testing.T) {
	if engine := newNativeEngine(t.TempDir(), ManagerOptions{}, engineEvents{}); engine != nil {
		t.Fatal("without a window provider there must be no native engine at all")
	}
}

func TestNativeEngineRefusesToStartBeforeTheWindowExists(t *testing.T) {
	// The provider exists from boot but answers nil until the app loop has
	// created the window. Starting then must fail cleanly — never reach AppKit,
	// and never leave the engine reporting itself as running.
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{})
	if engine == nil {
		// An older macOS has no callAsyncJavaScript and therefore no engine
		// at all, which is a legitimate answer rather than a failure.
		if wkSupported() {
			t.Fatal("a window provider must select the native engine")
		}
		return
	}
	err := engine.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("start error = %v", err)
	}
	if engine.Running() {
		t.Fatal("a failed start must not report the engine as running")
	}
	// Stop is the shutdown path and must be safe on an engine that never
	// started, because that is exactly the state a failed boot leaves.
	engine.Stop()
	engine.Interrupt()
}

// The pane background is the one piece of the present path that is pure Go, and
// its failure mode is silent: a mis-parsed colour paints the wrong background
// behind a page rather than raising anything.
func TestPaneBackgroundCodeParsesOnlyRRGGBB(t *testing.T) {
	for value, want := range map[string]int{
		"#000000": 0x000000,
		"#ffffff": 0xffffff,
		"#1a1B26": 0x1a1b26,
		"#0a0b0c": 0x0a0b0c,
		// Everything that is not exactly #rrggbb is "no colour", never a
		// partially parsed one.
		"":           wkNoBackground,
		"#fff":       wkNoBackground,
		"1a1b26":     wkNoBackground,
		"#1a1b2":     wkNoBackground,
		"#1a1b26 ":   wkNoBackground,
		" #1a1b26":   wkNoBackground,
		"#1a1b2g":    wkNoBackground,
		"#12345678":  wkNoBackground,
		"rgb(0,0,0)": wkNoBackground,
	} {
		if got := wkBackgroundCode(value); got != want {
			t.Fatalf("wkBackgroundCode(%q) = %#x, want %#x", value, got, want)
		}
	}
}

func TestNativeEngineRefusesProfilesWhileStopped(t *testing.T) {
	engine := newNativeEngine(t.TempDir(), ManagerOptions{
		NativeWindow: func() unsafe.Pointer { return nil },
	}, engineEvents{})
	if engine == nil {
		return
	}
	if _, err := engine.NewProfile(context.Background(), profileOptions{Workspace: t.TempDir()}); err == nil {
		t.Fatal("a profile on a stopped engine must be an error, not a live session")
	}
}
