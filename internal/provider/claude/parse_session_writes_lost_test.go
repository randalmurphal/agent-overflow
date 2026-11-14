package claude

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// system/session_writes_lost (2.1.317+) says the CLI could not save its
// transcript, ~/.claude.json, or both, because the disk was full or over
// quota. The parser emits no event and records one error text.
func TestParseSystem_SessionWritesLostRecordsOneErrorText(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		contains    []string
		notContains []string
	}{
		{
			name:     "transcript and config on a full disk",
			line:     `{"type":"system","subtype":"session_writes_lost","error_code":"ENOSPC","lost":["transcript","global_config"],"transcript_session_ids":["s-1"],"uuid":"u-1","session_id":"s-1"}`,
			contains: []string{"Claude could not save the conversation transcript and its settings file (~/.claude.json) because the disk is full.", "Resuming"},
		},
		{
			name:        "transcript only over quota",
			line:        `{"type":"system","subtype":"session_writes_lost","error_code":"EDQUOT","lost":["transcript"],"transcript_session_ids":["s-0","s-1"],"uuid":"u-1","session_id":"s-1"}`,
			contains:    []string{"could not save the conversation transcript because the disk quota is used up.", "Resuming"},
			notContains: []string{"~/.claude.json"},
		},
		{
			name:        "config only",
			line:        `{"type":"system","subtype":"session_writes_lost","error_code":"ENOSPC","lost":["global_config"],"uuid":"u-1","session_id":"s-1"}`,
			contains:    []string{"could not save its settings file (~/.claude.json) because the disk is full."},
			notContains: []string{"transcript", "Resuming"},
		},
		{
			name:        "absent error code and lost list",
			line:        `{"type":"system","subtype":"session_writes_lost","uuid":"u-1","session_id":"s-1"}`,
			contains:    []string{"could not save some of its session data because the disk is full or over quota."},
			notContains: []string{"Resuming"},
		},
		{
			name:        "only an unknown lost value",
			line:        `{"type":"system","subtype":"session_writes_lost","error_code":"ENOSPC","lost":["plugin_cache"],"uuid":"u-1","session_id":"s-1"}`,
			contains:    []string{"could not save some of its session data because the disk is full."},
			notContains: []string{"plugin_cache", "Resuming"},
		},
		{
			name:     "an unknown code beside a known and an unknown lost value",
			line:     `{"type":"system","subtype":"session_writes_lost","error_code":"EIO","lost":["transcript","plugin_cache"],"uuid":"u-1","session_id":"s-1"}`,
			contains: []string{"could not save the conversation transcript and other session data because a disk error (EIO).", "Resuming"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewParser()
			events, err := p.ParseLine(testThread, []byte(tt.line))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("events = %+v, want none", events)
			}
			// A repeat in the same process keeps the first text.
			if _, err := p.ParseLine(testThread, []byte(`{"type":"system","subtype":"session_writes_lost","error_code":"EDQUOT","lost":["global_config"]}`)); err != nil {
				t.Fatalf("parse repeat: %v", err)
			}
			text := p.takeSessionWritesLost()
			for _, want := range tt.contains {
				if !strings.Contains(text, want) {
					t.Fatalf("text = %q, want it to contain %q", text, want)
				}
			}
			for _, unwanted := range tt.notContains {
				if strings.Contains(text, unwanted) {
					t.Fatalf("text = %q, must not contain %q", text, unwanted)
				}
			}
			if again := p.takeSessionWritesLost(); again != "" {
				t.Fatalf("second take = %q, want empty", again)
			}
		})
	}
}

// The CLI writes the envelope as it exits, after its stdin closes. The text
// is on the Session once Close returns, and is handed out once.
func TestSessionLostWritesAreReadyWhenCloseReturns(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/fake-claude"
	mockexec.Write(t, path, `#!/bin/sh
while IFS= read -r _; do :; done
sleep 0.2
printf '%s\n' '{"type":"system","subtype":"session_writes_lost","error_code":"ENOSPC","lost":["transcript"],"transcript_session_ids":["s-1"],"uuid":"u-1","session_id":"s-1"}'
exit 0
`)
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: path})
	if err != nil {
		cancel()
		t.Fatalf("spawn: %v", err)
	}
	var emitted []provider.ProviderEvent
	s := &Session{
		proc:     proc,
		threadID: testThread,
		parser:   NewParser(),
		onEvent: func(evt provider.ProviderEvent) {
			if evt.Kind != provider.EventSessionStatus {
				emitted = append(emitted, evt)
			}
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()
	if got := s.TakeLostWritesError(); got != "" {
		t.Fatalf("before close = %q, want empty", got)
	}
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("close took %s", time.Since(start))
	}
	text := s.TakeLostWritesError()
	if !strings.Contains(text, "the conversation transcript because the disk is full") {
		t.Fatalf("after close = %q, want the transcript text", text)
	}
	if again := s.TakeLostWritesError(); again != "" {
		t.Fatalf("second take = %q, want empty", again)
	}
	if len(emitted) != 0 {
		t.Fatalf("emitted %+v, want no provider event for the envelope", emitted)
	}
}
