package claude

import (
	"context"
	"os"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestNewSessionWithMock(t *testing.T) {
	// NewSession passes CLI flags (--input-format, --output-format, --verbose)
	// that real `cat` rejects. Use a bash one-liner that ignores args and echoes.
	ctx := context.Background()
	eventCh := make(chan provider.ProviderEvent, 100)
	s, err := NewSession(ctx, testThread, Config{
		Binary: "bash",
		Model:  "", // keep args minimal
	}, func(evt provider.ProviderEvent) {
		eventCh <- evt
	})

	// NewSession spawns bash with args: --input-format stream-json --output-format stream-json --verbose
	// bash doesn't understand these either. Use a different approach:
	// override the binary to a script that ignores args.
	if err != nil {
		// Expected: bash doesn't understand Claude CLI flags.
		// Test NewSession more directly via the helper instead.
		t.Skipf("NewSession with bash fails as expected: %v", err)
	}
	defer s.Close()

	if s.threadID != testThread {
		t.Errorf("threadID: got %q, want %q", s.threadID, testThread)
	}
}

func TestNewSessionSpawnsAndRunsReadLoop(t *testing.T) {
	// Create a script that ignores args and acts like cat.
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/mock-claude"
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\nexec cat\n"), 0755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	ctx := context.Background()
	eventCh := make(chan provider.ProviderEvent, 100)
	s, err := NewSession(ctx, testThread, Config{Binary: scriptPath}, func(evt provider.ProviderEvent) {
		eventCh <- evt
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	if s.threadID != testThread {
		t.Errorf("threadID: got %q, want %q", s.threadID, testThread)
	}
	if s.proc == nil {
		t.Fatal("proc is nil")
	}

	// readLoop should be running — verify by writing an init event.
	initLine := []byte(`{"type":"system","subtype":"init","session_id":"cat-sess","model":"opus","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`)
	if err := s.proc.WriteLine(initLine); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := waitEvent(t, eventCh)
	if evt.Kind != provider.EventInit {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventInit)
	}
	if s.SessionID() != "cat-sess" {
		t.Errorf("sessionID: got %q, want %q", s.SessionID(), "cat-sess")
	}
}

func TestSessionIDAccessor(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	// Before init, session ID should be empty.
	if s.SessionID() != "" {
		t.Errorf("SessionID should be empty before init, got %q", s.SessionID())
	}

	// Write init to set it.
	initLine := []byte(`{"type":"system","subtype":"init","session_id":"test-sid","model":"opus","cwd":"/","tools":[],"claude_code_version":"1.0"}`)
	if err := s.proc.WriteLine(initLine); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitEvent(t, eventCh) // wait for init event to be processed

	if s.SessionID() != "test-sid" {
		t.Errorf("SessionID: got %q, want %q", s.SessionID(), "test-sid")
	}
}

func TestCloseWaitsForDisconnectedHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	disconnected := make(chan struct{})
	release := make(chan struct{})
	closeReturned := make(chan struct{})
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				close(disconnected)
				<-release
			}
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	go func() {
		_ = s.Close()
		close(closeReturned)
	}()

	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for disconnected handler")
	}

	select {
	case <-closeReturned:
		t.Fatal("Close returned before disconnected handler completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Close to return")
	}
}
