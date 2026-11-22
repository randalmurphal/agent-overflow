package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflow/engine"
)

type provenSession struct {
	started bool
	err     error
}

func (f *workflowResumeFixture) ensureAsync(ctx context.Context) <-chan provenSession {
	proofs := make(chan provenSession, 1)
	go func() {
		started, err := f.app.ensureProvenSession(ctx, f.thread.ID, func() error { return nil })
		proofs <- provenSession{started, err}
	}()
	return proofs
}

// Two proofs of one thread run one after the other: the first starts and
// proves the session, the second proves the session the first registered,
// and a third call does the same.
func TestEnsureProvenSessionSerializesProofsOfOneThread(t *testing.T) {
	t.Parallel()
	f := newWorkflowResumeFixture(t)
	f.setMode("")
	f.useCLI(resumeCLI{loaded: true, gated: true})

	first := f.ensureAsync(t.Context())
	f.waitStdin(`"subtype":"mcp_status"`)
	second := f.ensureAsync(t.Context())
	time.Sleep(heldWindow)
	select {
	case got := <-second:
		t.Fatalf("second proof returned %+v while the first held the thread", got)
	default:
	}
	f.openGate()
	var results []provenSession
	for _, proofs := range []<-chan provenSession{first, second} {
		select {
		case got := <-proofs:
			results = append(results, got)
		case <-time.After(30 * time.Second):
			t.Fatal("a proof did not return")
		}
	}
	if results[0] != (provenSession{started: true}) || results[1] != (provenSession{}) {
		t.Fatalf("proofs = %+v, want the first to start and the second to reuse", results)
	}
	live, ok := f.app.sessionManager().get(f.thread.ID)
	if !ok {
		t.Fatal("no session registered after the proofs")
	}
	if again := <-f.ensureAsync(t.Context()); again != (provenSession{}) {
		t.Fatalf("a repeated proof = %+v, want the registered session reused", again)
	}
	if current, _ := f.app.sessionManager().get(f.thread.ID); current.Token != live.Token {
		t.Fatal("a repeated proof replaced the session")
	}
	if probes := strings.Count(strings.Join(f.stdinLines(), "\n"), `"subtype":"mcp_status"`); probes != 1 {
		t.Fatalf("the CLI was probed %d times, want once: %q", probes, f.stdinLines())
	}
}

// A live Codex session proved its thread at registration, and a live
// claude-tui session reports no start outcome: each is reused as it is,
// with nothing started.
func TestEnsureProvenSessionReusesALiveCodexOrClaudeTUISession(t *testing.T) {
	t.Parallel()
	for name, live := range map[string]session{
		"codex":      {Provider: string(provider.Codex), Token: "live-codex", Codex: &codex.Session{}},
		"claude-tui": {Provider: string(provider.ClaudeTUI), Token: "live-tui", ClaudeTUI: &claudetui.Session{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, _ := setupE2EApp(t)
			threadID := "live-" + name
			app.sessionManager().put(threadID, live)
			t.Cleanup(func() { app.sessionManager().unregister(threadID, live.Token) })

			started, err := app.ensureProvenSession(t.Context(), threadID, func() error {
				return errors.New("a live session must not be started again")
			})
			if started || err != nil {
				t.Fatalf("proof = started %v, err %v; want the live session reused", started, err)
			}
			if current, ok := app.sessionManager().get(threadID); !ok || current.Token != live.Token {
				t.Fatal("the live session is no longer registered")
			}
		})
	}
}

// heldWindow is how long a test watches for an input that must not reach
// the CLI while its proof is pending. An input that is not held arrives in
// milliseconds.
const heldWindow = 500 * time.Millisecond

// requireNoStdinFor fails if the CLI reads a line containing part within
// heldWindow.
func (f *workflowResumeFixture) requireNoStdinFor(part, why string) {
	f.t.Helper()
	deadline := time.Now().Add(heldWindow)
	for time.Now().Before(deadline) {
		if f.stdinIndex(part) >= 0 {
			f.t.Fatalf("%s reached the CLI before its proof returned: %q", why, f.stdinLines())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *workflowResumeFixture) startAsync(ctx context.Context) <-chan error {
	errs := make(chan error, 1)
	go func() { errs <- f.start(ctx) }()
	return errs
}

func requirePending(t *testing.T, errs <-chan error, why string) {
	t.Helper()
	select {
	case err := <-errs:
		t.Fatalf("continuation start returned %v while %s", err, why)
	default:
	}
}

func awaitStart(t *testing.T, errs <-chan error) error {
	t.Helper()
	select {
	case err := <-errs:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the continuation start did not return")
		return nil
	}
}

// A message queued for the thread is flushed when its session starts. The
// proof holds the thread until it returns, so the flush cannot reach the CLI
// first and make an empty conversation count a user message.
func TestWorkflowContinuationHoldsAQueuedFlushOffTheClaudeProof(t *testing.T) {
	t.Parallel()
	const queued = "queued during the proof"
	for name, resumedUsers := range map[string]int{"conversation holds a user message": 1, "empty conversation": 0} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newWorkflowResumeFixture(t)
			users := resumedUsers
			f.useCLI(resumeCLI{loaded: true, gated: true, conversationUsers: &users})
			f.app.configureTriageQueueCallbacks()
			f.app.triage.RegisterQueueItem(f.thread.ID, triage.QueuedFlushItem{
				ID: "queue:held", Message: queued, Payload: json.RawMessage(`{}`), EnqueuedAt: 10, NoRow: true,
			})

			errs := f.startAsync(t.Context())
			f.waitStdin(`"subtype":"mcp_status"`)
			f.requireNoStdinFor(queued, "the queued message")
			requirePending(t, errs, "the CLI had not answered")
			f.openGate()
			err := awaitStart(t, errs)

			if resumedUsers == 0 {
				if !errors.Is(err, engine.ErrProviderContextUnavailable) {
					t.Fatalf("continuation start = %v, want the provider context unavailable", err)
				}
				f.requireNothingLeft()
				if f.stdinIndex(queued) >= 0 {
					t.Fatal("the queued message reached a CLI whose conversation was empty")
				}
				draft, _, err := f.app.store.GetThreadDraft(f.thread.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(draft.Content, queued) {
					t.Fatalf("draft = %q, want the queued message back in the composer", draft.Content)
				}
				return
			}
			if err != nil {
				t.Fatalf("continuation start = %v, want the resumed session kept", err)
			}
			f.waitStdin(queued)
			if usage, sent := f.stdinIndex(`"subtype":"get_context_usage"`), f.stdinIndex(queued); usage < 0 || usage > sent {
				t.Fatalf("stdin = %q, want the content read before the queued message", f.stdinLines())
			}
		})
	}
}

// A workflow send issued while the proof is pending waits for it.
func TestWorkflowContinuationHoldsASendOffTheClaudeProof(t *testing.T) {
	t.Parallel()
	const sent = "sent during the proof"
	f := newWorkflowResumeFixture(t)
	users := 1
	f.useCLI(resumeCLI{loaded: true, gated: true, conversationUsers: &users})

	errs := f.startAsync(t.Context())
	f.waitStdin(`"subtype":"mcp_status"`)
	sendErrs := make(chan error, 1)
	go func() {
		sendErrs <- f.app.sendWorkflowMessage(t.Context(), f.thread.ID, sent, json.RawMessage(`{"type":"object"}`), nil)
	}()
	f.requireNoStdinFor(sent, "the send")
	f.openGate()
	if err := awaitStart(t, errs); err != nil {
		t.Fatalf("continuation start = %v, want the resumed session kept", err)
	}
	if err := <-sendErrs; err != nil {
		t.Fatalf("send = %v", err)
	}
	if usage, at := f.stdinIndex(`"subtype":"get_context_usage"`), f.stdinIndex(sent); usage < 0 || usage > at {
		t.Fatalf("stdin = %q, want the content read before the send", f.stdinLines())
	}
}

// A session another path registered is proven before the continuation runs
// on it, not accepted because it is registered.
func TestWorkflowContinuationProvesARegisteredClaudeSession(t *testing.T) {
	t.Parallel()
	t.Run("still loading", func(t *testing.T) {
		t.Parallel()
		f := newWorkflowResumeFixture(t)
		f.useCLI(resumeCLI{loaded: true, gated: true})
		f.registerSessionOutsideTheRunner("")

		errs := f.startAsync(t.Context())
		f.waitStdin(`"subtype":"mcp_status"`)
		f.requireNoStdinFor(`"type":"user"`, "the continuation prompt")
		requirePending(t, errs, "the registered session had not loaded")
		f.openGate()
		if err := awaitStart(t, errs); err != nil {
			t.Fatalf("continuation start = %v, want the registered session proven", err)
		}
		if _, registered := f.runner.SessionSchemaForThread(f.thread.ID); !registered {
			t.Fatal("the attempt's schema is not registered")
		}
	})
	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		f := newWorkflowResumeFixture(t)
		notFound := `{"type":"result","subtype":"error_during_execution","duration_ms":0,"is_error":true,"num_turns":0,"errors":["No conversation found with session ID: ` + workflowResumeSession + `"]}`
		f.useCLI(resumeCLI{loaded: true, onProbe: notFound})
		f.registerSessionOutsideTheRunner("")

		err := f.start(t.Context())
		if !errors.Is(err, engine.ErrProviderContextUnavailable) {
			t.Fatalf("continuation start = %v, want the provider context unavailable", err)
		}
		f.requireNothingLeft()
	})
}

// A live session that already took an input is proof on its own: the
// continuation reuses it without reading its content, which says nothing
// once an input was accepted. That holds for a start an init resolved and
// for one a proof resolved before the input, as a warm reuse between phases
// finds it.
func TestWorkflowContinuationReusesAnInitializedClaudeSessionWithoutReadingContent(t *testing.T) {
	t.Parallel()
	contentReads := func(f *workflowResumeFixture) int {
		return strings.Count(strings.Join(f.stdinLines(), "\n"), `"subtype":"get_context_usage"`)
	}
	t.Run("start resolved by the init", func(t *testing.T) {
		t.Parallel()
		f := newWorkflowResumeFixture(t)
		f.useCLI(resumeCLI{loaded: true, initOnInput: true})
		f.registerSessionOutsideTheRunner("an earlier turn")

		if err := f.start(t.Context()); err != nil {
			t.Fatalf("continuation start = %v, want the live session reused", err)
		}
		waitFor(t, "the continuation prompt to reach the CLI", func() bool {
			return strings.Count(strings.Join(f.stdinLines(), "\n"), `"type":"user"`) >= 2
		})
		if reads := contentReads(f); reads != 0 {
			t.Fatalf("stdin = %q, want no content read", f.stdinLines())
		}
	})
	t.Run("start resolved by a proof", func(t *testing.T) {
		t.Parallel()
		f := newWorkflowResumeFixture(t)
		f.useCLI(resumeCLI{loaded: true, initOnInput: true})
		f.setMode("")
		if got := <-f.ensureAsync(t.Context()); got != (provenSession{started: true}) {
			t.Fatalf("first proof = %+v, want the session started and proven", got)
		}
		if err := f.app.sendMessage(f.thread.ID, "an earlier phase", nil); err != nil {
			t.Fatal(err)
		}
		f.waitStdin("an earlier phase")
		if got := <-f.ensureAsync(t.Context()); got != (provenSession{}) {
			t.Fatalf("second proof = %+v, want the live session reused", got)
		}
		if reads := contentReads(f); reads != 1 {
			t.Fatalf("stdin = %q, want the content read by the first proof alone", f.stdinLines())
		}
	})
}
