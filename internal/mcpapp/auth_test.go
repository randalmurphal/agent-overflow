package mcpapp

import (
	"context"
	"testing"
	"time"
)

func TestStartClaudeMCPOAuthPollCancelsPriorRegistration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := New(Deps{Context: func() context.Context { return ctx }})

	peek := func(name string) *claudeMCPOAuthPoll {
		service.claudeOAuthPollsMu.Lock()
		defer service.claudeOAuthPollsMu.Unlock()
		return service.claudeOAuthPolls[name]
	}

	service.startClaudeMCPOAuthPoll("thread-1", "linear")
	first := peek("linear")
	if first == nil {
		t.Fatal("first call did not register a poll")
	}

	firstCancelFired := make(chan struct{})
	originalCancel := first.cancel
	service.claudeOAuthPollsMu.Lock()
	first.cancel = func() {
		close(firstCancelFired)
		originalCancel()
	}
	service.claudeOAuthPollsMu.Unlock()

	service.startClaudeMCPOAuthPoll("thread-1", "linear")
	select {
	case <-firstCancelFired:
	case <-time.After(time.Second):
		t.Fatal("superseding a poll did not cancel the prior registration")
	}

	second := peek("linear")
	if second == nil {
		t.Fatal("second call did not register a poll")
	}
	if second == first {
		t.Fatal("second call retained the prior poll identity")
	}
}
