package triage

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"sync"
	"testing"
)

// Two screens rendering one prompt is the normal case once a backend serves
// more than one client, so the second answer must be a refusal rather than a
// second write to the provider.
func TestOnlyTheFirstAnswerToAPromptIsForwarded(t *testing.T) {
	r, _, _ := newTestRouter(t)

	if !claimed(r.ClaimPrompt("thr-1", "req-1", 0)) {
		t.Fatal("the first answer was refused")
	}
	if claimed(r.ClaimPrompt("thr-1", "req-1", 0)) {
		t.Fatal("a second answer to the same prompt was forwarded")
	}
}

// The record is per thread and per request: one refusal must not silence an
// unrelated prompt.
func TestAnswerRecordsAreScopedToTheirRequest(t *testing.T) {
	r, _, _ := newTestRouter(t)
	r.ClaimPrompt("thr-1", "req-1", 0)

	if !claimed(r.ClaimPrompt("thr-1", "req-2", 0)) {
		t.Error("a different prompt on the same thread was refused")
	}
	if !claimed(r.ClaimPrompt("thr-2", "req-1", 0)) {
		t.Error("the same request id on a different thread was refused")
	}
	if !claimed(r.ClaimPrompt("thr-1", "req-3", 0)) {
		t.Error("a structured-input form was refused")
	}
}

// The router refuses only on evidence it has: an answer it forwarded. A
// request it has no record of goes through, so the provider's own staleness
// check stays the authority on whether the id is answerable — reporting
// "someone else answered" about a request nobody answered would be a worse
// report than the one this closes.
func TestAnUnknownRequestIsPassedThroughRatherThanRefused(t *testing.T) {
	r, _, _ := newTestRouter(t)

	if !claimed(r.ClaimPrompt("thr-never-seen", "req-never-seen", 0)) {
		t.Fatal("a request the router has no record of was refused")
	}
}

// A malformed call is not a race. Refusing it would report "someone else
// answered" for a call that named no prompt at all.
func TestAClaimWithNoRequestIdIsNotArbitrated(t *testing.T) {
	r, _, _ := newTestRouter(t)

	if !claimed(r.ClaimPrompt("thr-1", "", 0)) {
		t.Error("an empty request id was refused")
	}
	if !claimed(r.ClaimPrompt("", "req-1", 0)) {
		t.Error("an empty thread id was refused")
	}
	var nilRouter *Router
	if !claimed(nilRouter.ClaimPrompt("thr-1", "req-1", 0)) {
		t.Error("a nil router refused a claim")
	}
	nilRouter.ReleasePrompt(PromptClaim{Ref: PromptRef{ThreadID: "thr-1", RequestID: "req-1"}})
}

// A write that never reached the provider leaves the prompt open. If the
// record survived, the prompt would be wedged: still rendered, and every
// later attempt — including a retry from the client that just failed —
// refused as already handled.
func TestAReleasedClaimCanBeRetried(t *testing.T) {
	r, _, _ := newTestRouter(t)
	r.ClaimPrompt("thr-1", "req-1", 0)

	r.ReleasePrompt(PromptClaim{Ref: PromptRef{ThreadID: "thr-1", RequestID: "req-1"}})

	if !claimed(r.ClaimPrompt("thr-1", "req-1", 0)) {
		t.Fatal("a retry after a failed write was refused")
	}
	// Idempotent: a release for something never claimed is not an error and
	// must not create state.
	r.ReleasePrompt(PromptClaim{Ref: PromptRef{ThreadID: "thr-1", RequestID: "req-unknown"}})
	r.ReleasePrompt(PromptClaim{Ref: PromptRef{ThreadID: "thr-never-seen", RequestID: "req-1"}})
}

// The records must not outlive the prompts they arbitrate, or a later request
// that reused an id would be refused for a decision made in a dead session.
// Both sweeps that drop the pending maps drop these with them: the turn
// boundary (turn_lifecycle.go, beside `clear(st.pendingApprovals)`) and the
// thread teardown, which drops the whole threadState.
func TestAnswerRecordsDoNotOutliveTheirThread(t *testing.T) {
	r, _, _ := newTestRouter(t)
	r.ClaimPrompt("thr-1", "req-1", 0)

	r.CleanupThread("thr-1")

	if !claimed(r.ClaimPrompt("thr-1", "req-1", 0)) {
		t.Fatal("the id stayed refused after its thread was torn down")
	}
}

// Two clients answering in the same instant is the case the record exists for,
// and the arbitration has to hold when both calls are genuinely concurrent —
// exactly one may win.
func TestConcurrentAnswersProduceExactlyOneWinner(t *testing.T) {
	r, _, _ := newTestRouter(t)
	const callers = 32

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(callers)
	won := make([]bool, callers)
	for i := range callers {
		go func() {
			defer done.Done()
			start.Wait()
			won[i] = claimed(r.ClaimPrompt("thr-1", "req-1", 0))
		}()
	}
	start.Done()
	done.Wait()

	winners := 0
	for _, w := range won {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d callers were told to forward the answer, want exactly 1", winners)
	}
}

func claimed(_ PromptClaim, ok bool) bool { return ok }

// A claim names the prompt occurrence its caller was shown. Once the session
// asks again under the same request id, a claim for the earlier prompt is
// refused, and the release of a claim the earlier prompt took leaves the
// later prompt's claim in place.
func TestAClaimAnswersOnlyTheOccurrenceItNames(t *testing.T) {
	t.Parallel()
	f := newPromptRouteFixture(t)
	f.router.MarkThreadActive("worker", "s1")
	f.approval(t, "worker", "req", "s1")
	first := f.open(t, "worker", "req")
	early, ok := f.router.ClaimPrompt("worker", "req", first.Occurrence)
	if !ok || early.Ref != first || early.SessionToken != "s1" {
		t.Fatalf("claim of the open prompt = %+v %v, want %+v in s1", early, ok, first)
	}
	f.router.MarkThreadActive("worker", "s2")
	f.approval(t, "worker", "req", "s2")
	second := f.open(t, "worker", "req")

	if claimed(f.router.ClaimPrompt("worker", "req", first.Occurrence)) {
		t.Fatal("a claim for the earlier prompt was let through to the later one")
	}
	late, ok := f.router.ClaimPrompt("worker", "req", 0)
	if !ok || late.Ref != second || late.SessionToken != "s2" {
		t.Fatalf("claim of the prompt open now = %+v %v, want %+v in s2", late, ok, second)
	}
	f.router.ReleasePrompt(early)
	if claimed(f.router.ClaimPrompt("worker", "req", second.Occurrence)) {
		t.Fatal("the earlier prompt's release dropped the later prompt's claim")
	}
	f.router.ReleasePrompt(late)
	if !claimed(f.router.ClaimPrompt("worker", "req", second.Occurrence)) {
		t.Fatal("the later prompt's own release did not hand it back")
	}
}

// This app's resolution of a prompt that ended leaves the prompt asked since
// under the same request id open and unannounced; one that names the open
// prompt resolves it. Approvals and questions alike.
func TestAResolutionOfAnEndedPromptLeavesTheOneAskedSince(t *testing.T) {
	t.Parallel()
	for _, family := range []struct {
		name    string
		channel eventchan.Channel
		resolve provider.EventKind
		ask     func(t *testing.T, r *Router, toolUseID string)
		open    func(r *Router) []uint64
	}{
		{
			name: "approval", channel: eventchan.ProviderApproval, resolve: provider.EventApprovalResolved,
			ask: func(t *testing.T, r *Router, toolUseID string) { agentApproval(t, r, "t1", "req", toolUseID, "") },
			open: func(r *Router) (out []uint64) {
				for _, request := range r.PendingInteractiveRequests("t1").Approvals {
					out = append(out, request.Occurrence)
				}
				return out
			},
		},
		{
			name: "question", channel: eventchan.ProviderUserInput, resolve: provider.EventUserInputResolved,
			ask: func(t *testing.T, r *Router, toolUseID string) { agentQuestion(t, r, "t1", "req", toolUseID, "") },
			open: func(r *Router) (out []uint64) {
				for _, request := range r.PendingInteractiveRequests("t1").UserInputs {
					out = append(out, request.Occurrence)
				}
				return out
			},
		},
	} {
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()
			r, st, emits := newTestRouter(t)
			createTestThread(t, st, "t1")
			resolve := func(occurrence uint64) {
				t.Helper()
				parkHandle(t, r, provider.ProviderEvent{
					Kind: family.resolve, ThreadID: "t1", ItemID: "req",
					Meta: parkMeta(t, map[string]any{"requestId": "req", "decision": "approved"}), PromptOccurrence: occurrence,
				})
			}
			announced := func() int { return len(filterEmissions(emits.snapshot(), family.channel.String())) }
			family.ask(t, r, "tu-1")
			first := family.open(r)[0]
			resolve(0)
			family.ask(t, r, "tu-2")
			second := family.open(r)[0]
			before := announced()

			resolve(first)
			if open := family.open(r); len(open) != 1 || open[0] != second {
				t.Fatalf("the ended prompt's resolution closed the one asked since: open %v", open)
			}
			if got := announced(); got != before {
				t.Fatalf("the ended prompt's resolution announced %d events", got-before)
			}
			resolve(second)
			if open := family.open(r); len(open) != 0 {
				t.Fatalf("the open prompt's resolution left it open: %v", open)
			}
		})
	}
}
