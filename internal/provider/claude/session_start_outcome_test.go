package claude

import (
	"context"
	"errors"
	"os"
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
// itself and decides when they arrive. It holds each other control request
// it reads and answers it only after a line `answer:<frame>`, writing <frame>
// first when it is not empty, whichever of the two arrives first. It answers
// get_context_usage at once with contextUsage, a control_response whose %s
// is the request id, or never when contextUsage is empty.
func echoCLI(contextUsage string) string {
	return `#!/bin/sh
usage='` + contextUsage + `'
pending=""; answer=0; before=""
while IFS= read -r line; do
	case "$line" in
	*'"subtype":"get_context_usage"'*)
		[ -n "$usage" ] && printf "$usage\n" "$(printf '%s\n' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')" ;;
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
}

// contextUsageAnswer is a get_context_usage answer counting userTokens
// user-message tokens.
func contextUsageAnswer(userTokens string) string {
	return `{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"totalTokens":100,"maxTokens":200000,"messageBreakdown":{"toolCallTokens":0,"toolResultTokens":0,"attachmentTokens":61,"assistantMessageTokens":12,"userMessageTokens":` + userTokens + `}}}}`
}

// startEchoSession starts a session on an echoCLI whose conversation holds a
// user message.
func startEchoSession(t *testing.T, cfg Config, onEvent func(provider.ProviderEvent)) *Session {
	t.Helper()
	return startEchoSessionAnswering(t, cfg, onEvent, contextUsageAnswer("8"))
}

func startEchoSessionAnswering(t *testing.T, cfg Config, onEvent func(provider.ProviderEvent), contextUsage string) *Session {
	t.Helper()
	cfg.Reaper = procutiltest.Reaper()
	cfg.Retention = UserHomeRetention(0)
	cfg.Binary = mockexec.Write(t, t.TempDir()+"/mock-claude", echoCLI(contextUsage))
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

// The load is decided before the frame reaches onEvent, so a host whose
// onEvent stalls does not hold it. A fresh start, so no content check
// follows: that answer is read by the same read loop.
func TestAwaitStartDoesNotWaitOnTheHostsEventHandling(t *testing.T) {
	release := make(chan struct{})
	s := startEchoSession(t, Config{}, func(evt provider.ProviderEvent) {
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

// A loaded resume is proven only when get_context_usage counts user-message
// tokens. Zero is an empty conversation; every other answer leaves the
// content unproven and names why.
func TestAwaitStartChecksTheResumedConversationHoldsAUserMessage(t *testing.T) {
	cases := map[string]struct {
		answer string
		// want is the sentinel the outcome wraps, nil for a proven start.
		want  error
		cause string
	}{
		"no user message":   {answer: contextUsageAnswer("0"), want: ErrConversationEmpty, cause: "0 user-message tokens"},
		"a user message":    {answer: contextUsageAnswer("4")},
		"negative count":    {answer: contextUsageAnswer("-4"), want: ErrContentUnproven, cause: "-4 user-message tokens"},
		"no breakdown":      {answer: `{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"totalTokens":100}}}`, want: ErrContentUnproven, cause: "no messageBreakdown"},
		"breakdown no user": {answer: `{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"messageBreakdown":{"assistantMessageTokens":12}}}}`, want: ErrContentUnproven, cause: "no userMessageTokens"},
		"malformed":         {answer: `{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"messageBreakdown":{"userMessageTokens":"zero"}}}}`, want: ErrContentUnproven, cause: "unreadable"},
		"refused":           {answer: `{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"Unsupported control request subtype: get_context_usage"}}`, want: ErrContentUnproven, cause: "refused: Unsupported control request subtype: get_context_usage"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := startEchoSessionAnswering(t, Config{Resume: outcomeSession}, func(provider.ProviderEvent) {}, tc.answer)
			if err := s.proc.WriteLine([]byte("answer:")); err != nil {
				t.Fatal(err)
			}
			err := awaitStartWithin(t, s)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("AwaitStart = %v, want the content proven", err)
				}
				return
			}
			if !errors.Is(err, tc.want) || !IsConversationUnproven(err) || !strings.Contains(err.Error(), tc.cause) {
				t.Fatalf("AwaitStart = %v, want %v naming %q", err, tc.want, tc.cause)
			}
		})
	}
}

// heldContentCLI answers every control request at once except
// get_context_usage, which it answers with a user message only after it has
// read another input. It creates marker when it reads that request.
func heldContentCLI(marker string) string {
	return `#!/bin/sh
held=""
while IFS= read -r line; do
	reqid=$(printf '%s\n' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
	case "$line" in
	*'"subtype":"get_context_usage"'*) held=$reqid; : > '` + marker + `' ;;
	*'"type":"control_request"'*) printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid" ;;
	*) [ -n "$held" ] && printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"messageBreakdown":{"userMessageTokens":8}}}}\n' "$held"; held="" ;;
	esac
done
`
}

// The count is the resumed conversation's only before the first input. A
// process whose start an init resolved, or that took a prompt and wrote the
// init for it, is proven and its content is not read; a command AO wrote on
// its own, before the count is read or while it is out, leaves the content
// unproven.
func TestAwaitStartReadsContentOnlyBeforeTheFirstInput(t *testing.T) {
	start := func(t *testing.T, script string) *Session {
		t.Helper()
		s, err := NewSession(context.Background(), testThread, Config{
			Reaper: procutiltest.Reaper(), Retention: UserHomeRetention(0), Resume: outcomeSession,
			Binary: mockexec.Write(t, t.TempDir()+"/mock-claude", script),
		}, func(provider.ProviderEvent) {})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	send := func(t *testing.T, s *Session, internal bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Send(ctx, "/effort high", provider.SendOptions{InternalCommand: internal}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	t.Run("init", func(t *testing.T) {
		// get_context_usage is never answered: a check would not return.
		s := startEchoSessionAnswering(t, Config{Resume: outcomeSession}, func(provider.ProviderEvent) {}, "")
		if err := s.proc.WriteLine(outcomeInit(outcomeSession)); err != nil {
			t.Fatal(err)
		}
		if err := awaitStartWithin(t, s); err != nil {
			t.Fatalf("AwaitStart = %v, want the init's outcome", err)
		}
	})
	t.Run("prompt", func(t *testing.T) {
		s := startEchoSessionAnswering(t, Config{Resume: outcomeSession}, func(provider.ProviderEvent) {}, "")
		if err := s.proc.WriteLine([]byte("answer:")); err != nil {
			t.Fatal(err)
		}
		send(t, s, false)
		if err := s.proc.WriteLine(outcomeInit(outcomeSession)); err != nil {
			t.Fatal(err)
		}
		if err := awaitStartWithin(t, s); err != nil {
			t.Fatalf("AwaitStart = %v, want the loaded process that took a prompt", err)
		}
	})
	t.Run("command before the count", func(t *testing.T) {
		s := startEchoSessionAnswering(t, Config{Resume: outcomeSession}, func(provider.ProviderEvent) {}, contextUsageAnswer("8"))
		send(t, s, true)
		if err := s.proc.WriteLine(outcomeInit(outcomeSession)); err != nil {
			t.Fatal(err)
		}
		if err := awaitStartWithin(t, s); !errors.Is(err, ErrContentUnproven) {
			t.Fatalf("AwaitStart = %v, want the content unproven", err)
		}
	})
	t.Run("command while the count is out", func(t *testing.T) {
		marker := t.TempDir() + "/asked"
		s := start(t, heldContentCLI(marker))
		done := make(chan error, 1)
		go func() { done <- awaitStartWithin(t, s) }()
		deadline := time.Now().Add(10 * time.Second)
		for _, err := os.Stat(marker); err != nil; _, err = os.Stat(marker) {
			if time.Now().After(deadline) {
				t.Fatal("the content was never asked for")
			}
			time.Sleep(10 * time.Millisecond)
		}
		send(t, s, true)
		if err := <-done; !errors.Is(err, ErrContentUnproven) {
			t.Fatalf("AwaitStart = %v, want the content unproven", err)
		}
	})
	t.Run("nothing written", func(t *testing.T) {
		marker := t.TempDir() + "/asked"
		s := start(t, heldContentCLI(marker))
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := s.AwaitStart(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("AwaitStart = %v, want it waiting on the count", err)
		}
	})
}

// The content check waits on ctx alone, and a fresh start or a fork is not
// checked at all: neither has a conversation to continue.
func TestAwaitStartContentCheckEndsWithItsContextAndSkipsAFreshStart(t *testing.T) {
	s := startEchoSessionAnswering(t, Config{Resume: outcomeSession}, func(provider.ProviderEvent) {}, "")
	if err := s.proc.WriteLine([]byte("answer:")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := s.AwaitStart(ctx); !errors.Is(err, context.DeadlineExceeded) || IsConversationUnproven(err) {
		t.Fatalf("AwaitStart = %v, want its context's end", err)
	}

	for name, cfg := range map[string]Config{"fresh": {}, "fork": {Resume: outcomeSession, ForkSession: true}} {
		t.Run(name, func(t *testing.T) {
			s := startEchoSessionAnswering(t, cfg, func(provider.ProviderEvent) {}, "")
			if err := s.proc.WriteLine([]byte("answer:")); err != nil {
				t.Fatal(err)
			}
			if err := awaitStartWithin(t, s); err != nil {
				t.Fatalf("AwaitStart = %v, want no content check", err)
			}
		})
	}
}

// The not-found frame decides, not the exit that follows it at once: stdout
// is drained to EOF before the read loop ends, a last line without a newline
// included.
func TestAwaitStartReadsTheNotFoundFrameOfACLIThatExitsAfterIt(t *testing.T) {
	frame := `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"errors":["No conversation found with session ID: ` + outcomeSession + `"]}`
	for name, format := range map[string]string{"newline": `%s\n`, "no newline": `%s`} {
		t.Run(name, func(t *testing.T) {
			for range 10 {
				binary := mockexec.Write(t, t.TempDir()+"/mock-claude",
					"#!/bin/sh\necho 'No conversation found' >&2\nprintf '"+format+"' '"+frame+"'\nexit 1\n")
				s, err := NewSession(context.Background(), testThread, Config{
					Reaper: procutiltest.Reaper(), Retention: UserHomeRetention(0), Binary: binary, Resume: outcomeSession,
				}, func(provider.ProviderEvent) {})
				if err != nil {
					t.Fatalf("NewSession: %v", err)
				}
				err = awaitStartWithin(t, s)
				_ = s.Close()
				if !errors.Is(err, ErrSessionNotFound) {
					t.Fatalf("AwaitStart = %v, want the frame's not-found", err)
				}
			}
		})
	}
}
