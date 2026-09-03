package triage

import (
	"sync"
	"testing"
)

// Two screens rendering one prompt is the normal case once a backend serves
// more than one client, so the second answer must be a refusal rather than a
// second write to the provider.
func TestOnlyTheFirstAnswerToAPromptIsForwarded(t *testing.T) {
	r, _, _ := newTestRouter(t)

	if !r.ClaimApprovalResponse("thr-1", "req-1") {
		t.Fatal("the first answer was refused")
	}
	if r.ClaimApprovalResponse("thr-1", "req-1") {
		t.Fatal("a second answer to the same prompt was forwarded")
	}
}

// The record is per thread and per request: one refusal must not silence an
// unrelated prompt.
func TestAnswerRecordsAreScopedToTheirRequest(t *testing.T) {
	r, _, _ := newTestRouter(t)
	r.ClaimApprovalResponse("thr-1", "req-1")

	if !r.ClaimApprovalResponse("thr-1", "req-2") {
		t.Error("a different prompt on the same thread was refused")
	}
	if !r.ClaimApprovalResponse("thr-2", "req-1") {
		t.Error("the same request id on a different thread was refused")
	}
	if !r.ClaimUserInputResponse("thr-1", "req-3") {
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

	if !r.ClaimApprovalResponse("thr-never-seen", "req-never-seen") {
		t.Fatal("a request the router has no record of was refused")
	}
}

// A malformed call is not a race. Refusing it would report "someone else
// answered" for a call that named no prompt at all.
func TestAClaimWithNoRequestIdIsNotArbitrated(t *testing.T) {
	r, _, _ := newTestRouter(t)

	if !r.ClaimApprovalResponse("thr-1", "") {
		t.Error("an empty request id was refused")
	}
	if !r.ClaimApprovalResponse("", "req-1") {
		t.Error("an empty thread id was refused")
	}
	var nilRouter *Router
	if !nilRouter.ClaimApprovalResponse("thr-1", "req-1") {
		t.Error("a nil router refused a claim")
	}
	nilRouter.ReleaseInteractiveResponse("thr-1", "req-1")
}

// A write that never reached the provider leaves the prompt open. If the
// record survived, the prompt would be wedged: still rendered, and every
// later attempt — including a retry from the client that just failed —
// refused as already handled.
func TestAReleasedClaimCanBeRetried(t *testing.T) {
	r, _, _ := newTestRouter(t)
	r.ClaimApprovalResponse("thr-1", "req-1")

	r.ReleaseInteractiveResponse("thr-1", "req-1")

	if !r.ClaimApprovalResponse("thr-1", "req-1") {
		t.Fatal("a retry after a failed write was refused")
	}
	// Idempotent: a release for something never claimed is not an error and
	// must not create state.
	r.ReleaseInteractiveResponse("thr-1", "req-unknown")
	r.ReleaseInteractiveResponse("thr-never-seen", "req-1")
}

// The records must not outlive the prompts they arbitrate, or a later request
// that reused an id would be refused for a decision made in a dead session.
// Both sweeps that drop the pending maps drop these with them: the turn
// boundary (turn_lifecycle.go, beside `clear(st.pendingApprovals)`) and the
// thread teardown, which drops the whole threadState.
func TestAnswerRecordsDoNotOutliveTheirThread(t *testing.T) {
	r, _, _ := newTestRouter(t)
	r.ClaimApprovalResponse("thr-1", "req-1")

	r.CleanupThread("thr-1")

	if !r.ClaimApprovalResponse("thr-1", "req-1") {
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
			won[i] = r.ClaimApprovalResponse("thr-1", "req-1")
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
