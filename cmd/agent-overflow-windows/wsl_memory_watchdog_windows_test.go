//go:build windows

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/wsllauncher"
)

func procStatFixture(pid, parent int, comm, start string, rss uint64) string {
	fields := make([]string, 22)
	fields[0] = "S"
	fields[1] = fmt.Sprint(parent)
	fields[19] = start
	fields[21] = fmt.Sprint(rss)
	return fmt.Sprintf("%d (%s) %s", pid, comm, strings.Join(fields, " "))
}

func TestParseWSLProcStatAllowsSpacesAndParenthesesInComm(t *testing.T) {
	got, err := parseWSLProcStat(procStatFixture(17, 3, "worker (render)", "9988", 42))
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 17 || got.ParentPID != 3 || got.StartTime != "9988" || got.RSSPages != 42 {
		t.Fatalf("parsed stat = %+v", got)
	}
}

func TestParseWSLProcStatRejectsMalformedIdentityFields(t *testing.T) {
	cases := []string{
		"17 worker S 1",
		procStatFixture(17, 3, "worker", "not-a-start-time", 42),
		procStatFixture(17, 3, "worker", "9988", 42)[:len(procStatFixture(17, 3, "worker", "9988", 42))-2] + "-1",
	}
	for _, line := range cases {
		if _, err := parseWSLProcStat(line); err == nil {
			t.Fatalf("malformed stat accepted: %q", line)
		}
	}
}

func TestCollectWSLProcTreeTraversesBeyondEightLevels(t *testing.T) {
	lines := make([]string, 0, 64)
	for pid := 1; pid <= 64; pid++ {
		lines = append(lines, procStatFixture(pid, pid-1, fmt.Sprintf("child (%d)", pid), fmt.Sprint(pid), 1))
	}
	got, err := collectWSLProcTree(lines, 1, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 64 {
		t.Fatalf("tree depth = %d, want 64", len(got))
	}
}

func TestCollectWSLProcTreeEnforcesCountAndByteCaps(t *testing.T) {
	line := procStatFixture(1, 0, "root", "1", 1)
	if _, err := collectWSLProcTree([]string{line}, 1, 1, len(line)-1); err == nil {
		t.Fatal("byte cap accepted oversized process data")
	}
	lines := []string{line, procStatFixture(2, 1, "child", "2", 1)}
	if _, err := collectWSLProcTree(lines, 1, 1, 1<<20); err == nil {
		t.Fatal("process cap accepted an oversized tree")
	}
}

func TestCappedProbeOutputRejectsUnboundedOutput(t *testing.T) {
	var output cappedProbeOutput
	output.limit = 4
	if _, err := output.Write([]byte("12345")); err == nil {
		t.Fatal("probe output exceeded its cap without an error")
	}
	if !output.overflow {
		t.Fatal("probe output overflow was not recorded")
	}
}

func TestWSLMemoryWatchdogStopsOnCeiling(t *testing.T) {
	oldProbe := runWSLMemoryProbe
	oldInterval := wslMemoryWatchInterval
	defer func() {
		runWSLMemoryProbe = oldProbe
		wslMemoryWatchInterval = oldInterval
	}()
	wslMemoryWatchInterval = time.Millisecond
	called := 0
	runWSLMemoryProbe = func(context.Context, string, int, string) (wslBackendSample, error) {
		called++
		return wslBackendSample{PID: 77, StartTime: "123", Executable: "/home/test/agent-overflow", RSSBytes: uint64(called) * 100}, nil
	}
	stopped := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchCancel := startWSLMemoryWatchdog(ctx, "Ubuntu", "/home/test/agent-overflow", &wsllauncher.Bootstrap{PID: 77}, 150, func() {
		close(stopped)
	})
	defer watchCancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("WSL memory watchdog did not stop after ceiling")
	}
	if called < 2 {
		t.Fatalf("probe calls = %d, want initial plus a measured sample", called)
	}
}

func TestWSLMemoryWatchdogStopsWhenIdentityProbeFails(t *testing.T) {
	oldProbe := runWSLMemoryProbe
	defer func() { runWSLMemoryProbe = oldProbe }()
	runWSLMemoryProbe = func(context.Context, string, int, string) (wslBackendSample, error) {
		return wslBackendSample{}, context.DeadlineExceeded
	}
	stopped := make(chan struct{})
	cancel := startWSLMemoryWatchdog(context.Background(), "Ubuntu", "/home/test/agent-overflow", &wsllauncher.Bootstrap{PID: 77}, 150, func() {
		close(stopped)
	})
	defer cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("WSL memory watchdog did not stop after identity failure")
	}
}

func TestWSLMemoryWatchdogRejectsBootstrapWithoutPID(t *testing.T) {
	called := false
	oldProbe := runWSLMemoryProbe
	defer func() { runWSLMemoryProbe = oldProbe }()
	runWSLMemoryProbe = func(context.Context, string, int, string) (wslBackendSample, error) {
		called = true
		return wslBackendSample{}, nil
	}
	stopped := make(chan struct{})
	cancel := startWSLMemoryWatchdog(context.Background(), "Ubuntu", "/home/test/agent-overflow", &wsllauncher.Bootstrap{}, 150, func() {
		close(stopped)
	})
	defer cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("WSL memory watchdog did not reject a missing backend pid")
	}
	if called {
		t.Fatal("identity probe ran without a backend pid")
	}
}
