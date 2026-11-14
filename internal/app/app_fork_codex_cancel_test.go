package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/testutil/mockexec"
)

// A Codex fork whose caller leaves while Codex cuts the child still ends
// with a fork thread naming that child. Codex cannot delete a child, so a
// fork stopped after the cut would leave one no thread reaches.
func TestForkThreadCodexCutDuringTheForkKeepsTheChild(t *testing.T) {
	t.Parallel()
	marks := t.TempDir()
	started, release := filepath.Join(marks, "started"), filepath.Join(marks, "release")
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/fork"'; then
        : > '%s'
        while [ ! -e '%s' ]; do sleep 0.01; done
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"fork-provider-thread"}}}\n' "$id"
    fi
done
`, started, release)
	binary := filepath.Join(t.TempDir(), "codex-fork-hold.sh")
	mockexec.Write(t, binary, script)

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	source := testThread("thread-codex-fork-cut")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type answer struct {
		ref string
		err error
	}
	done := make(chan answer, 1)
	go func() {
		forked, err := app.ForkThread(ctx, source.ID, nil)
		done <- answer{forked.SessionRef, err}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fork never reached Codex")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var got answer
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("ForkThread did not return")
	}
	if got.err != nil || got.ref != "fork-provider-thread" {
		t.Fatalf("ForkThread cut during the Codex fork = %q, %v; want the fork naming the child", got.ref, got.err)
	}
}
