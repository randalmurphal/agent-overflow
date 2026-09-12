//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/wsllauncher"
	"golang.org/x/sys/windows"
)

func TestWSLMemoryProbeRunsWithoutConsoleWindow(t *testing.T) {
	wslMemoryFixtureCompiler(t)
	if os.Getenv("AO_TEST_DETACHED_MEMORY_PROBE") != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, executable, "-test.run=^TestWSLMemoryProbeRunsWithoutConsoleWindow$", "-test.v")
		child.Env = append(os.Environ(), "AO_TEST_DETACHED_MEMORY_PROBE=1")
		// Match the GUI launcher's absence of an inherited console, including
		// when this test runner itself lives inside a headless WSL console.
		child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		if out, err := child.CombinedOutput(); err != nil {
			t.Fatalf("detached memory probe: %v\n%s", err, out)
		}
		return
	}
	installWSLMemoryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe, err := openWSLMemoryProbeCommand(ctx, "test-distro", 77, "/test/backend")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := probe.Close(); err != nil {
			t.Error(err)
		}
	})
	for i := range 3 {
		got, err := probe.Sample(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := wslBackendSample{PID: 77, StartTime: "123", Executable: "/test/backend", RSSBytes: uint64(4096 + i)}
		if got != want {
			t.Fatalf("sample = %+v, want %+v", got, want)
		}
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Sample(ctx); err == nil {
		t.Fatal("sample succeeded after close")
	}
}

func procStatFixture(pid, parent int, comm, start string, rss uint64) string {
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
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

func TestWSLMemoryWatchdogStopsOnCeiling(t *testing.T) {
	oldProbe := openWSLMemoryProbe
	oldInterval := wslMemoryWatchInterval
	defer func() {
		openWSLMemoryProbe = oldProbe
		wslMemoryWatchInterval = oldInterval
	}()
	wslMemoryWatchInterval = time.Millisecond
	called := 0
	opened := 0
	closed := make(chan struct{})
	openWSLMemoryProbe = func(context.Context, string, int, string) (wslMemorySampler, error) {
		opened++
		return &fakeWSLMemorySampler{sample: func(context.Context) (wslBackendSample, error) {
			called++
			return wslBackendSample{PID: 77, StartTime: "123", Executable: "/home/test/agent-overflow", RSSBytes: uint64(called) * 100}, nil
		}, close: func() error { close(closed); return nil }}, nil
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
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not close the probe")
	}
	if opened != 1 || called < 2 {
		t.Fatalf("probe opens=%d samples=%d, want one process and repeated samples", opened, called)
	}
}

func TestWSLMemoryWatchdogStopsWhenIdentityProbeFails(t *testing.T) {
	oldProbe := openWSLMemoryProbe
	defer func() { openWSLMemoryProbe = oldProbe }()
	openWSLMemoryProbe = func(context.Context, string, int, string) (wslMemorySampler, error) {
		return &fakeWSLMemorySampler{sample: func(context.Context) (wslBackendSample, error) {
			return wslBackendSample{}, context.DeadlineExceeded
		}}, nil
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
	oldProbe := openWSLMemoryProbe
	defer func() { openWSLMemoryProbe = oldProbe }()
	openWSLMemoryProbe = func(context.Context, string, int, string) (wslMemorySampler, error) {
		called = true
		return nil, fmt.Errorf("unexpected probe open")
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

type fakeWSLMemorySampler struct {
	sample func(context.Context) (wslBackendSample, error)
	close  func() error
}

func (p *fakeWSLMemorySampler) Sample(ctx context.Context) (wslBackendSample, error) {
	return p.sample(ctx)
}
func (p *fakeWSLMemorySampler) Close() error {
	if p.close != nil {
		return p.close()
	}
	return nil
}

func wslMemoryFixtureCompiler(t *testing.T) string {
	t.Helper()
	compiler := filepath.Join(os.Getenv("WINDIR"), "Microsoft.NET", "Framework64", "v4.0.30319", "csc.exe")
	if _, err := os.Stat(compiler); os.IsNotExist(err) {
		t.Skip("Windows .NET Framework C# compiler is unavailable")
	} else if err != nil {
		t.Fatal(err)
	}
	return compiler
}

func installWSLMemoryFixture(t *testing.T) {
	t.Helper()
	compiler := wslMemoryFixtureCompiler(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "probe.cs")
	// A standalone console executable observes the launch flags before any
	// application runtime can detach from the inherited or allocated console.
	const program = `using System;
using System.Runtime.InteropServices;
class Probe {
    [DllImport("kernel32.dll")] static extern IntPtr GetConsoleWindow();
    static int Main() {
        if (GetConsoleWindow() != IntPtr.Zero) {
            Console.Error.WriteLine("memory probe allocated or inherited a console window");
            return 1;
        }
        int n = 0;
        while (Console.ReadLine() != null) {
            string mode = Environment.GetEnvironmentVariable("AO_TEST_WSL_PROBE_REPLY");
            if (mode == "exit") { Console.Error.WriteLine("fixture identity check failed"); return 42; }
            if (mode == "oversized") { Console.Write(new string('9', 512)); return 0; }
            if (mode == "partial") { Console.Write("77 123 4096"); return 0; }
            if (mode == "hang") { System.Threading.Thread.Sleep(30000); }
            Console.WriteLine("77 123 " + (4096 + n++));
        }
        return 0;
    }
}`
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, compiler, "/nologo", "/target:exe", "/out:"+filepath.Join(dir, "wsl.exe"), source)
	build.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile console fixture: %v\n%s", err, out)
	}
	t.Setenv("PATH", dir)
}

func TestWSLMemoryProbeRejectsBrokenResponseAndCloses(t *testing.T) {
	for _, mode := range []string{"exit", "oversized", "partial", "hang"} {
		t.Run(mode, func(t *testing.T) {
			installWSLMemoryFixture(t)
			t.Setenv("AO_TEST_WSL_PROBE_REPLY", mode)
			probe, err := openWSLMemoryProbeCommand(context.Background(), "fixture", 77, "/test/backend")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := probe.Close(); err != nil {
					t.Error(err)
				}
			})
			timeout := 5 * time.Second
			if mode == "hang" {
				timeout = 300 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_, err = probe.Sample(ctx)
			if err == nil {
				t.Fatal("broken response accepted")
			}
			if mode == "exit" && !strings.Contains(err.Error(), "fixture identity check failed") {
				t.Fatalf("lost probe diagnostics: %v", err)
			}
			if mode == "hang" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("hang error = %v", err)
			}
			if _, err := probe.Sample(context.Background()); err == nil {
				t.Fatal("failed probe reused")
			}
			if err := probe.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWSLMemorySampleRejectsMalformedIdentityAndRSS(t *testing.T) {
	for _, line := range []string{"77 123", "78 123 4096", "77 invalid 4096", "77 123 -1", "77 123 18446744073709551616"} {
		if _, err := parseWSLMemorySample(line, 77, "/test/backend"); err == nil {
			t.Fatalf("accepted %q", line)
		}
	}
}

func TestWSLMemoryWatchdogRejectsIdentityChangesAndCloses(t *testing.T) {
	for _, field := range []string{"pid", "birth", "executable"} {
		t.Run(field, func(t *testing.T) {
			oldProbe, oldInterval := openWSLMemoryProbe, wslMemoryWatchInterval
			t.Cleanup(func() { openWSLMemoryProbe, wslMemoryWatchInterval = oldProbe, oldInterval })
			wslMemoryWatchInterval = time.Millisecond
			calls := 0
			closed := make(chan struct{})
			openWSLMemoryProbe = func(context.Context, string, int, string) (wslMemorySampler, error) {
				return &fakeWSLMemorySampler{sample: func(context.Context) (wslBackendSample, error) {
					calls++
					s := wslBackendSample{PID: 77, StartTime: "123", Executable: "/test/backend", RSSBytes: 1}
					if calls > 1 {
						switch field {
						case "pid":
							s.PID++
						case "birth":
							s.StartTime = "124"
						case "executable":
							s.Executable = "/different"
						}
					}
					return s, nil
				}, close: func() error { close(closed); return nil }}, nil
			}
			stopped := make(chan struct{})
			cancel := startWSLMemoryWatchdog(context.Background(), "fixture", "/test/backend", &wsllauncher.Bootstrap{PID: 77}, 100, func() { close(stopped) })
			defer cancel()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("identity change did not stop backend")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("probe not closed")
			}
		})
	}
}

func TestWSLMemoryWatchdogCancellationClosesWithoutStoppingBackend(t *testing.T) {
	oldProbe := openWSLMemoryProbe
	t.Cleanup(func() { openWSLMemoryProbe = oldProbe })
	started, closed := make(chan struct{}), make(chan struct{})
	openWSLMemoryProbe = func(context.Context, string, int, string) (wslMemorySampler, error) {
		return &fakeWSLMemorySampler{sample: func(ctx context.Context) (wslBackendSample, error) {
			close(started)
			<-ctx.Done()
			return wslBackendSample{}, ctx.Err()
		}, close: func() error { close(closed); return nil }}, nil
	}
	stopped := make(chan struct{}, 1)
	cancel := startWSLMemoryWatchdog(context.Background(), "fixture", "/test/backend", &wsllauncher.Bootstrap{PID: 77}, 100, func() { stopped <- struct{}{} })
	defer cancel()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("probe not closed after cancellation")
	}
	select {
	case <-stopped:
		t.Fatal("normal cancellation stopped the backend")
	default:
	}
}
