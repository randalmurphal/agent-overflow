//go:build !windows

package app

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/testutil/helperproc"
	"agent-overflow/internal/testutil/mockexec"
)

// A provider session ends with the backend on every Unix: its group is
// handed to the orphan reaper as the provider starts, the sidecar ends it when
// the backend dies, and the next launch sweeps the record the backend left.

// The environment that makes TestProviderBackendHelper the backend that dies:
// the data directory its orphan reaper keeps the registry in, what it starts,
// and the mock CLI it runs.
const (
	providerBackendDataEnv   = "PROVIDER_ORPHAN_TEST_DATA"
	providerBackendStartEnv  = "PROVIDER_ORPHAN_TEST_START"
	providerBackendBinaryEnv = "PROVIDER_ORPHAN_TEST_BINARY"
)

// What TestProviderBackendHelper starts: a session of each provider, or the
// temporary Codex app-server a fork of a thread without a live session runs.
const codexForkStart = "codex-fork"

// TestProviderBackendHelper is not a test of its own: the test below runs this
// binary as a backend that starts a provider process and is then killed. It
// starts the orphan reaper as Startup does and prints the process's group once
// the registry records it. The start runs in the background because a Codex
// app-server is not returned until its handshake completes, and the mock never
// answers it.
func TestProviderBackendHelper(t *testing.T) {
	dataDir := os.Getenv(providerBackendDataEnv)
	if dataDir == "" {
		t.Skip("runs only as the backend TestABackendsDeathEndsItsProviderSession kills")
	}
	app, _ := setupE2EApp(t)
	start := os.Getenv(providerBackendStartEnv)
	providerName, model, binarySetting := start, "claude-opus-4-7", "claudeBinaryPath"
	if start == string(provider.Codex) || start == codexForkStart {
		providerName, model, binarySetting = string(provider.Codex), "gpt-5", "codexBinaryPath"
	}
	thread, err := createTestThread(t, app, providerName, t.TempDir(), model, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.settings.Update(map[string]any{
		binarySetting: os.Getenv(providerBackendBinaryEnv),
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.startOrphanReaper(dataDir); err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() {
		if start != codexForkStart {
			started <- app.StartSession(thread.ID)
			return
		}
		thread.SessionRef = "thr-fork-source"
		started <- app.withCodexThreadSessionPreparedBy(thread, nil, func(*codex.Session) error { return nil })
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		records, err := app.orphanRegistry.Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 1 {
			fmt.Printf("ready %d\n", records[0].PGID)
			break
		}
		select {
		case err := <-started:
			if err != nil {
				t.Fatalf("start %s: %v", start, err)
			}
		default:
		}
		if len(records) > 1 || time.Now().After(deadline) {
			t.Fatalf("the registry holds %+v, want the provider's group", records)
		}
		time.Sleep(10 * time.Millisecond)
	}
	helperproc.Hold()
}

// providerMock is a provider CLI that, like Claude, does not exit when its
// stdin closes, and starts a child the way a provider starts an MCP server.
// The child and the provider ignore the hangup of a pty, so a terminal-hosted
// one ends only if the reaper ends it. onTerm, when set, is the provider's
// SIGTERM handler.
func providerMock(t *testing.T, onTerm string) string {
	t.Helper()
	script := "#!/bin/bash\ntrap '' HUP\n"
	if onTerm != "" {
		script += "trap '" + onTerm + "' TERM\n"
	}
	script += "sleep 600 &\n" +
		"while :; do sleep 600 & wait $!; done\n"
	return mockexec.Write(t, filepath.Join(t.TempDir(), "mock-provider.sh"), script)
}

// A backend killed while a provider session runs leaves nothing to close the
// session. The provider and the child it started end with the backend: a
// signal to the provider alone would leave the child running. A provider that
// exits on SIGTERM finishes its exit before any SIGKILL, so it can complete
// the session file write it was in. A group whose sidecar died with the
// backend is reaped by the next boot's sweep. A Codex app-server is handed to
// the reaper as it starts, not once its handshake completes.
func TestABackendsDeathEndsItsProviderSession(t *testing.T) {
	for _, c := range []struct {
		name        string
		start       string
		cleanExit   bool
		sidecarDies bool
	}{
		{"the provider and its child end with the backend", string(provider.Claude), false, false},
		{"a provider that exits on SIGTERM finishes its exit", string(provider.Claude), true, false},
		{"the next boot reaps the group", string(provider.Claude), false, true},
		{"a provider still in its handshake ends with the backend", string(provider.Codex), false, false},
		{"a terminal-hosted claude ends with the backend", string(provider.ClaudeTUI), false, false},
		{"a fork's temporary app-server ends with the backend", codexForkStart, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "exited")
			onTerm := ""
			if c.cleanExit {
				// Longer than a signal's delivery, shorter than the grace
				// before SIGKILL.
				onTerm = "sleep 0.5; echo clean > " + marker + "; exit 0"
			}
			binary := providerMock(t, onTerm)
			dataDir := t.TempDir()
			backend := exec.Command(os.Args[0], "-test.run=^TestProviderBackendHelper$")
			// The backend's temporary files land where this test removes them.
			backend.Env = append(os.Environ(),
				providerBackendDataEnv+"="+dataDir,
				providerBackendStartEnv+"="+c.start,
				providerBackendBinaryEnv+"="+binary,
				"TMPDIR="+t.TempDir())
			stdout, err := backend.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			backend.Stderr = os.Stderr
			helperproc.Tie(t, backend)
			if err := backend.Start(); err != nil {
				t.Fatal(err)
			}
			pgid := 0
			scanner := bufio.NewScanner(stdout)
			for pgid == 0 && scanner.Scan() {
				if rest, ok := strings.CutPrefix(scanner.Text(), "ready "); ok {
					pgid, _ = strconv.Atoi(rest)
				}
			}
			if pgid == 0 {
				_ = backend.Process.Kill()
				_ = backend.Wait()
				t.Fatalf("the backend never recorded its provider's group: %v", scanner.Err())
			}
			t.Cleanup(func() { _ = procutil.SignalGroup(pgid, syscall.SIGKILL) })
			if c.sidecarDies {
				sidecar := sidecarOf(t, backend.Process.Pid)
				if err := syscall.Kill(sidecar, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			}
			if err := backend.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = backend.Wait()

			registry := orphanreaper.NewRegistry(filepath.Join(dataDir, "orphan-registry.json"))
			if c.sidecarDies {
				if !groupRunning(pgid) {
					t.Fatal("the group ended with the backend; this case needs one that survives it")
				}
				if err := orphanreaper.Sweep(registry, orphanreaper.GopsutilProcInfo(), 100*time.Millisecond); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(10 * time.Second)
			for groupRunning(pgid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if groupRunning(pgid) {
				t.Fatal("the provider's group outlived the backend")
			}
			if c.cleanExit {
				if got, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(got)) != "clean" {
					t.Fatalf("the provider's SIGTERM handler did not finish (%q, %v): it was killed first", got, err)
				}
			}
			if c.sidecarDies {
				if records, err := registry.Load(); err != nil || len(records) != 0 {
					t.Fatalf("the sweep left %+v %v in the registry", records, err)
				}
			}
		})
	}
}
