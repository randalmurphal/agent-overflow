package claude

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// A control request times out on the read loop's silence, not on the wall
// clock: the CLI writes an interrupt's kill frames before its ack, and the
// read loop delivers each one before it reads the ack
// (DefaultControlRequestTimeout).

const progressTestTimeout = 300 * time.Millisecond

// interruptBurstScript answers an interrupt with frames task_updated
// kill frames, then the ack; with frames < 0 it never answers.
func interruptBurstScript(frames int) string {
	return fmt.Sprintf(`#!/bin/sh
set -u
frames=%d
while IFS= read -r line; do
    case "$line" in
        *'"subtype":"interrupt"'*)
            [ "$frames" -lt 0 ] && continue
            reqid=$(printf '%%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            i=0
            while [ "$i" -lt "$frames" ]; do
                printf '{"type":"system","subtype":"task_updated","task_id":"task-%%s","tool_use_id":"tu-%%s","patch":{"status":"killed"}}\n' "$i" "$i"
                i=$((i+1))
            done
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`, frames)
}

func newInterruptBurstSession(t *testing.T, frames int, onEvent func(provider.ProviderEvent)) *Session {
	t.Helper()
	path := t.TempDir() + "/fake-claude"
	if err := os.WriteFile(path, []byte(interruptBurstScript(frames)), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: path})
	if err != nil {
		cancel()
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:                  proc,
		threadID:              testThread,
		parser:                NewParser(),
		onEvent:               onEvent,
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: progressTestTimeout,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A consumer that takes longer over the whole burst than the timeout, but
// never goes quiet for that long, gets its ack.
func TestInterrupt_ASlowButProgressingBurstIsAcked(t *testing.T) {
	const frames = 8
	var delivered atomic.Int32
	s := newInterruptBurstSession(t, frames, func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventBackgroundTaskTerminal {
			delivered.Add(1)
			time.Sleep(progressTestTimeout / 3)
		}
	})
	start := time.Now()
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt after %s: %v", time.Since(start), err)
	}
	if elapsed := time.Since(start); elapsed < progressTestTimeout*2 {
		t.Fatalf("the burst took %s; the case needs one longer than the timeout", elapsed)
	}
	if got := delivered.Load(); got != frames {
		t.Fatalf("delivered %d kill frames before the ack, want %d", got, frames)
	}
}

// A CLI that never answers times out after the timeout of silence.
func TestInterrupt_ASilentCLITimesOut(t *testing.T) {
	s := newInterruptBurstSession(t, -1, func(provider.ProviderEvent) {})
	start := time.Now()
	err := s.Interrupt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Interrupt = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*progressTestTimeout {
		t.Fatalf("timed out after %s, want near %s", elapsed, progressTestTimeout)
	}
}

// A consumer that stops taking events stalls the read loop, and the
// request times out even though the CLI has answered behind it.
func TestInterrupt_AWedgedConsumerTimesOut(t *testing.T) {
	release := make(chan struct{})
	s := newInterruptBurstSession(t, 2, func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventBackgroundTaskTerminal {
			<-release
		}
	})
	// Registered after the session's own cleanup, so it runs first: Close
	// waits for the read loop, which waits here.
	t.Cleanup(func() { close(release) })
	start := time.Now()
	err := s.Interrupt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Interrupt = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*progressTestTimeout {
		t.Fatalf("timed out after %s, want near %s", elapsed, progressTestTimeout)
	}
}
