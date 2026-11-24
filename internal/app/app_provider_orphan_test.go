//go:build !windows

package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
			started <- app.startSessionTakingLock(context.Background(), thread.ID)
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
			fmt.Printf("ready %d\n", records[0].Leader)
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
// one ends only if the reaper ends it.
func providerMock(t *testing.T) string {
	t.Helper()
	return mockexec.Write(t, filepath.Join(t.TempDir(), "mock-provider.sh"),
		"#!/bin/bash\ntrap '' HUP\nsleep 600 &\nwhile :; do sleep 600 & wait $!; done\n")
}

// A backend killed while a provider session runs leaves nothing to close the
// session. The provider and the child it started end with the backend: a
// signal to the provider alone would leave the child running. A group whose
// sidecar died with the backend is reaped by the next boot's sweep. A Codex app-server is handed to
// the reaper as it starts, not once its handshake completes.
func TestABackendsDeathEndsItsProviderSession(t *testing.T) {
	for _, c := range []struct {
		name        string
		start       string
		sidecarDies bool
	}{
		{"the provider and its child end with the backend", string(provider.Claude), false},
		{"the next boot reaps the group", string(provider.Claude), true},
		{"a provider still in its handshake ends with the backend", string(provider.Codex), false},
		{"a terminal-hosted claude ends with the backend", string(provider.ClaudeTUI), false},
		{"a fork's temporary app-server ends with the backend", codexForkStart, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			binary := providerMock(t)
			dataDir := t.TempDir()
			backend := exec.Command(os.Args[0], "-test.run=^TestProviderBackendHelper$")
			backend.Env = append(os.Environ(),
				providerBackendDataEnv+"="+dataDir,
				providerBackendStartEnv+"="+c.start,
				providerBackendBinaryEnv+"="+binary)
			stdout, err := backend.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			backend.Stderr = os.Stderr
			helperproc.Tie(t, backend)
			if err := backend.Start(); err != nil {
				t.Fatal(err)
			}
			requireGroupEndsWithBackend(t, backend, stdout, dataDir, c.sidecarDies, "the provider's group")
		})
	}
}
