package app

import (
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/testutil/mockexec"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// writeCodexSteerBinary mints a fake codex app-server that knows how to
// answer the JSON-RPC methods the SteerMessageWithOptions path drives:
// initialize, thread/start, and turn/steer. The steer reply path is
// parameterized so individual tests can simulate the wire returning
// success or NoActiveTurn.
func writeCodexSteerBinary(t *testing.T, threadID, steerOutcome string) string {
	t.Helper()

	// steerBranch / startBranch are embedded into the outer fmt.Sprintf
	// template via %s verbs, so their own printf format specs need ONE
	// % (not %%) — the outer Sprintf is the only layer that strips a
	// percent here.
	silent := `: # simulate an accepted write whose JSON-RPC ack never arrives`
	steerBranch := `printf '{"jsonrpc":"2.0","id":%s,"result":{"turnId":"steer-turn"}}\n' "$id"`
	startBranch := `printf '{"jsonrpc":"2.0","id":%s,"result":{"turnId":"start-turn"}}\n' "$id"`
	switch steerOutcome {
	case "ok":
	case "no-active-turn":
		steerBranch = `printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"NoActiveTurn"}}\n' "$id"`
	case "refused":
		steerBranch = `printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"steer refused"}}\n' "$id"`
	case "timeout":
		steerBranch = silent
	case "start-timeout":
		startBranch = silent
	case "no-active-turn+start-timeout":
		steerBranch = `printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"NoActiveTurn"}}\n' "$id"`
		startBranch = silent
	default:
		t.Fatalf("writeCodexSteerBinary: unknown steerOutcome %q", steerOutcome)
	}

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
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/steer"'; then
        %s
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
        %s
        continue
    fi
%s    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, threadID, steerBranch, startBranch, testutil.MockCodexUnsubscribeShell)

	path := filepath.Join(t.TempDir(), "codex-steer.sh")
	mockexec.Write(t, path, script)
	return path
}

// installSteerTestSession registers an active codex Session on the App
// and pre-seeds activeTurnID so Steer doesn't short-circuit on the
// "no local active turn" branch (that branch is already covered at the
// codex.Session unit level). Returns the constructed session so the
// caller can Close() it via t.Cleanup.
func installSteerTestSession(t *testing.T, app *App, thread store.Thread, steerOutcome string) *codex.Session {
	t.Helper()
	return installSteerTestSessionWithEvents(t, app, thread, steerOutcome, func(provider.ProviderEvent) {})
}

// installSteerTestSessionWithEvents is installSteerTestSession with the
// session's own events, such as its disconnect at Close, handed to onEvent.
func installSteerTestSessionWithEvents(t *testing.T, app *App, thread store.Thread, steerOutcome string, onEvent func(provider.ProviderEvent)) *codex.Session {
	t.Helper()
	return installCodexTestSessionWithEvents(t, app, thread, writeCodexSteerBinary(t, thread.ID+"-codex", steerOutcome), onEvent)
}

// installCodexTestSessionWithBinary is installSteerTestSession over a fake
// app-server of the caller's own.
func installCodexTestSessionWithBinary(t *testing.T, app *App, thread store.Thread, binary string) *codex.Session {
	t.Helper()
	return installCodexTestSessionWithEvents(t, app, thread, binary, func(provider.ProviderEvent) {})
}

// installCodexTestSessionWithEvents is installCodexTestSessionWithBinary
// with the session's own events handed to onEvent.
func installCodexTestSessionWithEvents(t *testing.T, app *App, thread store.Thread, binary string, onEvent func(provider.ProviderEvent)) *codex.Session {
	t.Helper()
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Reaper:  procutiltest.Reaper(),
			Binary:  binary,
			WorkDir: thread.WorkspacePath,
		},
		onEvent,
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// Pre-seed an active turn id so Steer doesn't trip the
	// no-active-turn-yet guard. The triage / store layers are
	// independent of this session-local id; the test exercises the
	// wire dispatch + persistence wiring, not turn discovery.
	codex.SetActiveTurnIDForTest(sess, "active-turn")
	return sess
}
