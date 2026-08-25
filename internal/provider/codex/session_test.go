package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestCodexThreadIDAccessor(t *testing.T) {
	s, _ := newTestCodexSession(t)
	if got := s.ThreadID(); got != testThread {
		t.Errorf("ThreadID: got %q, want %q", got, testThread)
	}
}

func TestCodexCloseWaitsForDisconnectedHandler(t *testing.T) {
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
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				close(disconnected)
				<-release
			}
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	s.setRootThreadID("test")
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
