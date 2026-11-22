package claude

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// acceptingCLI answers every control request at once, get_context_usage with
// userTokens user-message tokens; "0" fails a proof that reads the count. A
// line `init:` makes it write the init the CLI writes when it takes an
// input, and a line `exit:` makes it exit. initOnPrompt writes that init for
// every prompt it reads.
func acceptingCLI(userTokens string, initOnPrompt bool) string {
	onPrompt := ":"
	if initOnPrompt {
		onPrompt = `printf '%s\n' '` + string(outcomeInit(outcomeSession)) + `'`
	}
	return `#!/bin/sh
while IFS= read -r line; do
	reqid=$(printf '%s\n' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
	case "$line" in
	*'"subtype":"get_context_usage"'*) printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"messageBreakdown":{"userMessageTokens":` + userTokens + `}}}}\n' "$reqid" ;;
	*'"type":"control_request"'*) printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid" ;;
	*'"type":"user"'*) ` + onPrompt + ` ;;
	init:) printf '%s\n' '` + string(outcomeInit(outcomeSession)) + `' ;;
	exit:) echo 'stdin closed' >&2; exit 3 ;;
	esac
done
`
}

// startLoaded starts a resumed session on an acceptingCLI and waits until
// its answer proved the load, before anything is written.
func startLoaded(t *testing.T, userTokens string, initOnPrompt bool) *Session {
	t.Helper()
	return startLoadedObserving(t, userTokens, initOnPrompt, func(provider.ProviderEvent) {})
}

func startLoadedObserving(t *testing.T, userTokens string, initOnPrompt bool, onEvent func(provider.ProviderEvent)) *Session {
	t.Helper()
	s, err := NewSession(context.Background(), testThread, Config{
		Reaper: procutiltest.Reaper(), Retention: UserHomeRetention(0), Resume: outcomeSession,
		Binary: mockexec.Write(t, t.TempDir()+"/mock-claude", acceptingCLI(userTokens, initOnPrompt)),
	}, onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.awaitLoad(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	return s
}

func sendPrompt(s *Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.Send(ctx, "continue", provider.SendOptions{})
}

func awaitStartAsync(s *Session, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- s.AwaitStart(ctx) }()
	return done
}

func requireStillWaiting(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("AwaitStart = %v before the CLI took the prompt, want it waiting", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func resultWithin(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("AwaitStart did not return")
		return nil
	}
}

// A prompt whose write failed proves nothing, and the failure stays: stdin
// is broken after it, so no later write or init makes the content proven.
func TestAwaitStartRefusesAProcessWhosePromptWriteFailed(t *testing.T) {
	s := startLoaded(t, "0", false)
	if err := s.proc.WriteLine([]byte("exit:")); err != nil {
		t.Fatal(err)
	}
	<-s.readDone
	if err := sendPrompt(s); err == nil {
		t.Fatal("Send to an exited process succeeded")
	}
	err := awaitStartWithin(t, s)
	if !errors.Is(err, ErrContentUnproven) || !strings.Contains(err.Error(), "process already exited") {
		t.Fatalf("AwaitStart = %v, want the content unproven by the failed write", err)
	}
}

// A prompt still being written proves nothing: the CLI may not have it.
func TestAwaitStartRefusesAPromptStillBeingWritten(t *testing.T) {
	// The CLI answers the load's probe and then stops reading stdin, so a
	// prompt larger than the pipe's buffer stays in its write.
	s, err := NewSession(context.Background(), testThread, Config{
		Reaper: procutiltest.Reaper(), Retention: UserHomeRetention(0), Resume: outcomeSession,
		Binary: mockexec.Write(t, t.TempDir()+"/mock-claude", `#!/bin/sh
IFS= read -r line
reqid=$(printf '%s\n' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
exec sleep 600
`),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.awaitLoad(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	go func() { _ = s.Send(context.Background(), strings.Repeat("x", 1<<20), provider.SendOptions{}) }()
	deadline := time.Now().Add(10 * time.Second)
	for s.start.inputs().promptsInFlight == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the prompt write never began")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A proof that read the count would block on stdin; the session's close
	// at cleanup ends that write.
	if err := resultWithin(t, awaitStartAsync(s, context.Background())); !errors.Is(err, ErrContentUnproven) || !strings.Contains(err.Error(), "being written") {
		t.Fatalf("AwaitStart = %v, want the content unproven by the write in flight", err)
	}
}

// A completed prompt write proves the content only once the CLI writes an
// init after the write began, the one it writes for the input it took; an
// init before the prompt is not that.
func TestAwaitStartWaitsForTheCLIToTakeAWrittenPrompt(t *testing.T) {
	for name, initFirst := range map[string]bool{"no init before the prompt": false, "an init before the prompt": true} {
		t.Run(name, func(t *testing.T) {
			inits := make(chan provider.ProviderEvent, 4)
			s := startLoadedObserving(t, "0", false, func(evt provider.ProviderEvent) {
				if evt.Kind == provider.EventInit {
					inits <- evt
				}
			})
			if initFirst {
				if err := s.proc.WriteLine([]byte("init:")); err != nil {
					t.Fatal(err)
				}
				waitEvent(t, inits)
			}
			if err := sendPrompt(s); err != nil {
				t.Fatal(err)
			}
			done := awaitStartAsync(s, context.Background())
			requireStillWaiting(t, done)
			if err := s.proc.WriteLine([]byte("init:")); err != nil {
				t.Fatal(err)
			}
			if err := resultWithin(t, done); err != nil {
				t.Fatalf("AwaitStart = %v, want the prompt taken", err)
			}
		})
	}
}

func TestAwaitStartReportsAnExitBeforeTheCLITookThePrompt(t *testing.T) {
	s := startLoaded(t, "0", false)
	if err := sendPrompt(s); err != nil {
		t.Fatal(err)
	}
	done := awaitStartAsync(s, context.Background())
	requireStillWaiting(t, done)
	if err := s.proc.WriteLine([]byte("exit:")); err != nil {
		t.Fatal(err)
	}
	err := resultWithin(t, done)
	if err == nil || !strings.Contains(err.Error(), "exited before the CLI took the prompt") || !strings.Contains(err.Error(), "stdin closed") {
		t.Fatalf("AwaitStart = %v, want the exit named", err)
	}
}

func TestAwaitStartWaitForThePromptEndsWithItsContext(t *testing.T) {
	s := startLoaded(t, "0", false)
	if err := sendPrompt(s); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := awaitStartAsync(s, ctx)
	requireStillWaiting(t, done)
	cancel()
	if err := resultWithin(t, done); !errors.Is(err, context.Canceled) || IsConversationUnproven(err) {
		t.Fatalf("AwaitStart = %v, want its context's end", err)
	}
}

// Sends racing the proof leave it unproven while a write is in flight, or
// proven once the CLI took a prompt; never the empty count the CLI answers.
func TestAwaitStartConcurrentWithSends(t *testing.T) {
	for range 20 {
		s := startLoaded(t, "8", true)
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				if err := sendPrompt(s); err != nil {
					t.Errorf("Send: %v", err)
				}
			})
		}
		err := awaitStartWithin(t, s)
		wg.Wait()
		if err != nil && !errors.Is(err, ErrContentUnproven) {
			t.Fatalf("AwaitStart = %v, want proven or unproven by a write in flight", err)
		}
	}
}
