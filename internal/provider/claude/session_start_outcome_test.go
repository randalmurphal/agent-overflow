package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

const outcomeSession = "11111111-2222-4333-8444-555555555555"

func outcomeInit(sessionID string) []byte {
	return []byte(`{"type":"system","subtype":"init","session_id":"` + sessionID + `","model":"opus","cwd":"/tmp","tools":[],"claude_code_version":"2.1.325"}`)
}

// echoCLI echoes every line it reads, so the test writes the CLI's frames
// itself and decides when they arrive. It holds each control request it
// reads and answers it only after a line `answer:<frame>`, writing <frame>
// first when it is not empty, whichever of the two arrives first.
const echoCLI = `#!/bin/sh
pending=""; answer=0; before=""
while IFS= read -r line; do
	case "$line" in
	*'"type":"control_request"'*) pending=$(printf '%s\n' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p') ;;
	answer:*) answer=1; before="${line#answer:}" ;;
	*) printf '%s\n' "$line" ;;
	esac
	if [ "$answer" = 1 ] && [ -n "$pending" ]; then
		[ -n "$before" ] && printf '%s\n' "$before"
		printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"mcpServers":[]}}}\n' "$pending"
		pending=""; answer=0; before=""
	fi
done
`

// startEchoSession starts a session on echoCLI.
func startEchoSession(t *testing.T, cfg Config, onEvent func(provider.ProviderEvent)) *Session {
	t.Helper()
	cfg.Reaper = procutiltest.Reaper()
	cfg.Retention = UserHomeRetention(0)
	cfg.Binary = mockexec.Write(t, t.TempDir()+"/mock-claude", echoCLI)
	s, err := NewSession(context.Background(), testThread, cfg, onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func awaitStartWithin(t *testing.T, s *Session) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := s.AwaitStart(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("AwaitStart did not resolve")
	}
	return err
}

func TestAwaitStartReportsTheInitOnTheResumedSession(t *testing.T) {
	events := make(chan provider.ProviderEvent, 16)
	s := startEchoSession(t, Config{Resume: outcomeSession}, func(evt provider.ProviderEvent) { events <- evt })
	if err := s.proc.WriteLine(outcomeInit(outcomeSession)); err != nil {
		t.Fatal(err)
	}
	// The init is handled before anyone waits; the outcome is kept.
	if evt := waitEvent(t, events); evt.Kind != provider.EventInit {
		t.Fatalf("first event = %s, want init", evt.Kind)
	}
	if err := awaitStartWithin(t, s); err != nil {
		t.Fatalf("AwaitStart = %v, want the start proven", err)
	}
}

// A resumed CLI writes no init before its first input; its answer to a
// control request is what proves the load.
func TestAwaitStartIsProvenByTheCLIAnsweringBeforeAnyInput(t *testing.T) {
	events := make(chan provider.ProviderEvent, 16)
	s := startEchoSession(t, Config{Resume: outcomeSession}, func(evt provider.ProviderEvent) { events <- evt })
	if err := s.proc.WriteLine([]byte("answer:")); err != nil {
		t.Fatal(err)
	}
	if err := awaitStartWithin(t, s); err != nil {
		t.Fatalf("AwaitStart = %v, want the load proven by the answer", err)
	}
	for len(events) > 0 {
		if evt := <-events; evt.Kind == provider.EventInit {
			t.Fatal("the proof needed an init")
		}
	}
}

// A load that fails writes its error result before the CLI answers any
// control request, so the failure stands even when the answer follows it.
func TestAwaitStartKeepsAFailureWrittenBeforeTheAnswer(t *testing.T) {
	result := `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"errors":["No conversation found with session ID: ` + outcomeSession + `"]}`
	for range 20 {
		s := startEchoSession(t, Config{Resume: outcomeSession}, func(provider.ProviderEvent) {})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		done := make(chan error, 1)
		go func() { done <- s.AwaitStart(ctx) }()
		if err := s.proc.WriteLine([]byte("answer:" + result)); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("AwaitStart = %v, want the failure the CLI wrote first", err)
		}
		cancel()
		_ = s.Close()
	}
}

// The outcome is decided before the frame reaches onEvent, so a host that
// cannot handle frames (its event worker waits on a lock the waiter holds)
// does not hold the waiter.
func TestAwaitStartDoesNotWaitOnTheHostsEventHandling(t *testing.T) {
	release := make(chan struct{})
	s := startEchoSession(t, Config{Resume: outcomeSession}, func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventInit {
			<-release
		}
	})
	// Registered after the session's Close, so it runs first and lets the
	// read loop finish.
	t.Cleanup(func() { close(release) })
	if err := s.proc.WriteLine(outcomeInit(outcomeSession)); err != nil {
		t.Fatal(err)
	}
	if err := awaitStartWithin(t, s); err != nil {
		t.Fatalf("AwaitStart = %v, want the start proven", err)
	}
}

func TestAwaitStartAcceptsAnySessionForAFreshStartOrAFork(t *testing.T) {
	for name, cfg := range map[string]Config{
		"fresh": {},
		"fork":  {Resume: outcomeSession, ForkSession: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := startEchoSession(t, cfg, func(provider.ProviderEvent) {})
			if err := s.proc.WriteLine(outcomeInit("aaaaaaaa-0000-4000-8000-000000000000")); err != nil {
				t.Fatal(err)
			}
			if err := awaitStartWithin(t, s); err != nil {
				t.Fatalf("AwaitStart = %v, want any session accepted", err)
			}
		})
	}
}

func TestAwaitStartRefusesAnInitOnAnotherSession(t *testing.T) {
	s := startEchoSession(t, Config{Resume: outcomeSession}, func(provider.ProviderEvent) {})
	if err := s.proc.WriteLine(outcomeInit("aaaaaaaa-0000-4000-8000-000000000000")); err != nil {
		t.Fatal(err)
	}
	err := awaitStartWithin(t, s)
	if !errors.Is(err, ErrResumedOtherSession) || errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("AwaitStart = %v, want ErrResumedOtherSession", err)
	}
}

// Each pre-init error result is a startup failure, marked on the frame the
// host reads; only the CLI's "no such conversation" proves it gone.
func TestAwaitStartClassifiesAPreInitErrorResult(t *testing.T) {
	cases := map[string]struct {
		message  string
		notFound bool
	}{
		"no conversation":       {message: "No conversation found with session ID: " + outcomeSession, notFound: true},
		"not a session id":      {message: "Error: --resume requires a valid session ID or session title when used with --print. Usage: claude -p --resume <session-id|title>", notFound: true},
		"cursor the CLI drops":  {message: "No message found with message.uuid of: abc"},
		"some other start fail": {message: "Error: worker_state_unread"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			events := make(chan provider.ProviderEvent, 16)
			s := startEchoSession(t, Config{Resume: outcomeSession}, func(evt provider.ProviderEvent) { events <- evt })
			line := `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"errors":["` + strings.ReplaceAll(tc.message, `"`, `\"`) + `"]}`
			if err := s.proc.WriteLine([]byte(line)); err != nil {
				t.Fatal(err)
			}
			err := awaitStartWithin(t, s)
			if err == nil || errors.Is(err, ErrSessionNotFound) != tc.notFound || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("AwaitStart = %v, want not-found %v naming %q", err, tc.notFound, tc.message)
			}
			for {
				evt := waitEvent(t, events)
				if evt.Kind == provider.EventTurnComplete {
					if !provider.IsStartupFailure(evt) {
						t.Fatalf("pre-init error result not marked a startup failure: %+v", evt.Failure)
					}
					return
				}
			}
		})
	}
}

// An error result after init is a turn's, not the start's.
func TestAnErrorResultAfterInitIsNoStartupFailure(t *testing.T) {
	events := make(chan provider.ProviderEvent, 16)
	s := startEchoSession(t, Config{Resume: outcomeSession}, func(evt provider.ProviderEvent) { events <- evt })
	for _, line := range [][]byte{
		outcomeInit(outcomeSession),
		[]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1,"errors":["No conversation found with session ID: x"]}`),
	} {
		if err := s.proc.WriteLine(line); err != nil {
			t.Fatal(err)
		}
	}
	for {
		evt := waitEvent(t, events)
		if evt.Kind == provider.EventTurnComplete {
			if provider.IsStartupFailure(evt) {
				t.Fatal("an error result after init was marked a startup failure")
			}
			break
		}
	}
	if err := awaitStartWithin(t, s); err != nil {
		t.Fatalf("AwaitStart = %v, want the init's outcome kept", err)
	}
}

func TestAwaitStartReportsAProcessThatExitsBeforeInit(t *testing.T) {
	binary := mockexec.Write(t, t.TempDir()+"/mock-claude", "#!/bin/sh\necho 'bad flag --x' >&2\nexit 3\n")
	s, err := NewSession(context.Background(), testThread, Config{
		Reaper: procutiltest.Reaper(), Retention: UserHomeRetention(0), Binary: binary, Resume: outcomeSession,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	err = awaitStartWithin(t, s)
	if err == nil || errors.Is(err, ErrSessionNotFound) ||
		!strings.Contains(err.Error(), "exited before it initialized") || !strings.Contains(err.Error(), "bad flag --x") {
		t.Fatalf("AwaitStart = %v, want the exit with its stderr", err)
	}
}

func TestAwaitStartReportsACloseBeforeInit(t *testing.T) {
	s := startEchoSession(t, Config{Resume: outcomeSession}, func(provider.ProviderEvent) {})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	err := awaitStartWithin(t, s)
	if err == nil || !strings.Contains(err.Error(), "closed before it initialized") {
		t.Fatalf("AwaitStart = %v, want the close", err)
	}
}

func TestAwaitStartEndsWithItsContext(t *testing.T) {
	s := startEchoSession(t, Config{Resume: outcomeSession}, func(provider.ProviderEvent) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.AwaitStart(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("AwaitStart = %v, want the context's cancellation", err)
	}
}
