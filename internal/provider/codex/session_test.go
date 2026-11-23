package codex

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestCodexThreadIDAccessor(t *testing.T) {
	t.Parallel()
	s, _ := newTestCodexSession(t)
	if got := s.ThreadID(); got != testThread {
		t.Errorf("ThreadID: got %q, want %q", got, testThread)
	}
}

func TestCodexCloseWaitsForDisconnectedHandler(t *testing.T) {
	t.Parallel()
	disconnected := make(chan struct{})
	release := make(chan struct{})
	closeReturned := make(chan struct{})
	s := fixtureSession(t, testThread, "test", func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
			close(disconnected)
			<-release
		}
	})

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
