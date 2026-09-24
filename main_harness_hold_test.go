package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHoldHarnessStartupReportsUntilReleased: the hold reports its phase
// for as long as the release file is missing and ends it once the file
// appears.
func TestHoldHarnessStartupReportsUntilReleased(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	var begun, ended atomic.Int32
	var phase, detail string
	begin := func(p, d string) func() {
		phase, detail = p, d
		begun.Add(1)
		return func() { ended.Add(1) }
	}
	done := make(chan error, 1)
	go func() { done <- holdHarnessStartup(context.Background(), begin, release, time.Millisecond) }()

	select {
	case err := <-done:
		t.Fatalf("hold returned before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if begun.Load() != 1 || ended.Load() != 0 {
		t.Fatalf("while held: begun=%d ended=%d, want 1 and 0", begun.Load(), ended.Load())
	}
	if phase != "harness.hold_startup" || detail != "Holding startup for a test" {
		t.Fatalf("phase=%q detail=%q", phase, detail)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hold: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not end after the release file appeared")
	}
	if ended.Load() != 1 {
		t.Fatalf("ended=%d after release, want 1", ended.Load())
	}
}

// TestHoldHarnessStartupEndsWithContext: a boot canceled while held stops
// holding and still ends its phase.
func TestHoldHarnessStartupEndsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ended atomic.Int32
	begin := func(string, string) func() { return func() { ended.Add(1) } }
	done := make(chan error, 1)
	go func() {
		done <- holdHarnessStartup(ctx, begin, filepath.Join(t.TempDir(), "never"), time.Millisecond)
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("hold: %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not end with its context")
	}
	if ended.Load() != 1 {
		t.Fatalf("ended=%d, want 1", ended.Load())
	}
}

// TestHarnessHoldStartupOnlyInHarnessBoot: the hold is a test isolation
// aid, read by the harness boot alone.
func TestHarnessHoldStartupOnlyInHarnessBoot(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		uses := strings.Contains(readRootSource(t, name), "os.Getenv(diagenv.HarnessHoldStartup)")
		if uses != (name == "main_harness.go") {
			t.Errorf("%s reads diagenv.HarnessHoldStartup: %v", name, uses)
		}
	}
	harness := readRootSource(t, "main_harness.go")
	hold := strings.Index(harness, "holdHarnessStartup(bootCtx")
	start := strings.Index(harness, "appService.Start(bootCtx)")
	if hold < 0 || start < 0 || hold > start {
		t.Fatalf("main_harness.go: hold=%d Start=%d; want the hold before App.Start", hold, start)
	}
}
