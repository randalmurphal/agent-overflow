package app

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

func writeSharedPoolCodexBinary(t *testing.T) string {
	t.Helper()
	script := `#!/bin/bash
n=0
while IFS= read -r line; do
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"codex_cli_rs/0.162.1 app_server"}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"config/read"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"config":{}}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
    n=$((n+1))
    printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-pool-%s-%s"}}}\n' "$id" "$$" "$n"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/backgroundTerminals/clean"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/unsubscribe"'; then
    tid=$(/bin/echo "$line" | /usr/bin/grep -o '"threadId":"[^"]*"' | /usr/bin/head -1 | /usr/bin/cut -d'"' -f4)
    printf '{"jsonrpc":"2.0","id":%s,"result":{"status":"unloaded"}}\n' "$id"
    if [ -n "$tid" ]; then
      printf '{"jsonrpc":"2.0","method":"thread/closed","params":{"threadId":"%s"}}\n' "$tid"
    fi
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"skills/list"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[]}}\n' "$id"
  else
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  fi
done
`
	path := filepath.Join(t.TempDir(), "codex-shared-pool.sh")
	mockexec.Write(t, path, script)
	return path
}

func TestCodexPoolSharesOneAppServerAcrossThreadsAndDrainsCleanly(t *testing.T) {
	t.Parallel()
	app, bus := setupE2EApp(t)
	binary := writeSharedPoolCodexBinary(t)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}

	threadA, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}
	threadB, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}
	threadC, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatal(err)
	}

	if err := app.startSessionTakingLock(context.Background(), threadA.ID); err != nil {
		t.Fatalf("StartSession(A): %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)

	if err := app.startSessionTakingLock(context.Background(), threadB.ID); err != nil {
		t.Fatalf("StartSession(B): %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)

	entryA, okA := app.sessionRuntime.Get(threadA.ID)
	entryB, okB := app.sessionRuntime.Get(threadB.ID)
	if !okA || !okB || entryA.Codex == nil || entryB.Codex == nil {
		t.Fatalf("missing session entries: A=%v B=%v", okA, okB)
	}
	pidA := entryA.Codex.PID()
	pidB := entryB.Codex.PID()
	if pidA <= 0 || pidA != pidB {
		t.Fatalf("expected shared app-server PID across A and B, got pidA=%d pidB=%d", pidA, pidB)
	}

	// Borrowing a connection for process-wide reads reuses the same shared Conn.
	borrowed, release := app.borrowCodexConnForBinary(binary)
	if borrowed == nil {
		t.Fatal("borrowCodexConnForBinary returned nil")
	}
	if borrowed.PID() != pidA {
		release()
		t.Fatalf("borrowed conn PID = %d, want %d", borrowed.PID(), pidA)
	}
	if _, err := borrowed.ListSkills(context.Background(), []string{threadA.WorkspacePath}, false); err != nil {
		release()
		t.Fatalf("borrowed.ListSkills: %v", err)
	}
	release()

	// Closing thread A unsubscribes A while leaving B and the shared Conn alive.
	if err := app.StopSession(threadA.ID); err != nil {
		t.Fatalf("StopSession(A): %v", err)
	}
	if entryB.Codex.PID() != pidA {
		t.Fatalf("thread B PID changed after closing A: got %d, want %d", entryB.Codex.PID(), pidA)
	}

	// Draining puts the old Conn in drain mode so thread C starts on a fresh PID.
	app.drainCodexConnections()
	if err := app.startSessionTakingLock(context.Background(), threadC.ID); err != nil {
		t.Fatalf("StartSession(C): %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	entryC, okC := app.sessionRuntime.Get(threadC.ID)
	if !okC || entryC.Codex == nil {
		t.Fatalf("missing entry for C")
	}
	if pidC := entryC.Codex.PID(); pidC <= 0 || pidC == pidA {
		t.Fatalf("expected thread C on new app-server PID after drain, got pidC=%d pidA=%d", pidC, pidA)
	}

	if err := app.StopSession(threadB.ID); err != nil {
		t.Fatalf("StopSession(B): %v", err)
	}
	if err := app.StopSession(threadC.ID); err != nil {
		t.Fatalf("StopSession(C): %v", err)
	}
	_ = fmt.Sprintf("verified shared pid %d -> drained pid %d", pidA, entryC.Codex.PID())
}
