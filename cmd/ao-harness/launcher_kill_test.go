package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeInterop points the Windows taskkill path at a real file in a temp
// dir and records every invocation.
type fakeInterop struct {
	calls   [][]string
	killErr error
}

func (f *fakeInterop) install(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	taskkill := filepath.Join(dir, "taskkill.exe")
	if err := os.WriteFile(taskkill, []byte("stub"), 0o700); err != nil {
		t.Fatalf("write %s: %v", taskkill, err)
	}
	origKill, origRun := winTaskkillExe, runInterop
	t.Cleanup(func() { winTaskkillExe, runInterop = origKill, origRun })
	winTaskkillExe = taskkill
	runInterop = func(_ context.Context, exe string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{exe}, args...))
		if exe == winTaskkillExe {
			return nil, f.killErr
		}
		return nil, nil
	}
}

func (f *fakeInterop) killed() bool {
	for _, call := range f.calls {
		if call[0] == winTaskkillExe {
			return true
		}
	}
	return false
}

func TestStopLauncherWindowVerifiedRequiresImmutableIdentity(t *testing.T) {
	fake := &fakeInterop{}
	fake.install(t)
	original := queryLauncherProcess
	t.Cleanup(func() { queryLauncherProcess = original })
	queryLauncherProcess = func(context.Context, int) (launcherProcessIdentity, error) {
		return launcherProcessIdentity{Path: `C:\Agent\agent-overflow.exe`, StartTime: 123, Command: `agent-overflow.exe --profile perf --user-data-dir=C:\Agent\webview2-perf`}, nil
	}
	reg := launcherRegistration{PID: 4242, StartTime: "123", Executable: `C:\Agent\agent-overflow.exe`, Profile: "perf", DataRoot: `\\wsl.localhost\Ubuntu\tmp\perf`, WebviewProfile: `C:\Agent\webview2-perf`, Namespace: "windows", BootNonce: "nonce"}
	killed, note := stopLauncherWindowVerified(reg)
	if !killed || note != "" || !fake.killed() {
		t.Fatalf("verified launcher stop = %v, %q, calls=%v", killed, note, fake.calls)
	}
}

func TestStopLauncherWindowVerifiedRefusesPIDReuseAndProfileMismatch(t *testing.T) {
	fake := &fakeInterop{}
	fake.install(t)
	original := queryLauncherProcess
	t.Cleanup(func() { queryLauncherProcess = original })
	reg := launcherRegistration{PID: 4242, StartTime: "123", Executable: `C:\Agent\agent-overflow.exe`, Profile: "perf", DataRoot: `\\wsl.localhost\Ubuntu\tmp\perf`, WebviewProfile: `C:\Agent\webview2-perf`, Namespace: "windows", BootNonce: "nonce"}
	for _, identity := range []launcherProcessIdentity{
		{Path: `C:\Agent\agent-overflow.exe`, StartTime: 456, Command: `agent-overflow.exe --profile perf --user-data-dir=C:\Agent\webview2-perf`},
		{Path: `C:\Agent\agent-overflow.exe`, StartTime: 123, Command: `agent-overflow.exe --profile soak --user-data-dir=C:\Agent\webview2-soak`},
	} {
		queryLauncherProcess = func(context.Context, int) (launcherProcessIdentity, error) { return identity, nil }
		killed, note := stopLauncherWindowVerified(reg)
		if killed || note == "" || fake.killed() {
			t.Fatalf("identity %+v stop = %v, %q, calls=%v", identity, killed, note, fake.calls)
		}
	}
}

// A Windows tool that refuses says WHY on stderr, and Output() throws that
// away: every failure read "exit status 1", from a bad filter to a denied
// kill. runInterop splices the first stderr line back in.
func TestRunInteropCarriesTheToolsOwnStderr(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to act out a failing tool")
	}
	_, err := runInterop(context.Background(), "/bin/sh", "-c",
		"echo 'ERROR: Access is denied.' >&2; exit 1")
	if err == nil {
		t.Fatal("a failing tool reported success")
	}
	if !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("error = %v, want the tool's own stderr spliced in", err)
	}
}
