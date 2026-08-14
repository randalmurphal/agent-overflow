package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// A failure the element authored no envelope for used to park with no account
// at all: the reason said `agent-error` and every surface that renders a cause —
// `run status`, `run inspect`, the wake — had nothing to show. `Outcome.Detail`
// is the runner's own statement of what it saw (the provider error, the process
// exit, the schedule that ran out), and `outcomeDetailCause` is the one rule
// that decides when it becomes the attempt's `park_cause`.
//
// The rule is about the ENVELOPE, not about the outcome kind: an envelope with
// content is the agent's own account and stays the sole one, because a cause
// written beside it would be a second, engine-authored answer to a question the
// element already answered.

func TestOutcomeDetailBecomesTheCauseOnlyWhenTheEnvelopeIsEmpty(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    Outcome
		wantsCause string
	}{
		{
			name:       "empty envelope",
			outcome:    Outcome{Kind: OutcomeExecutionFailure, Detail: "provider error rate_limit: usage limit reached"},
			wantsCause: "provider error rate_limit: usage limit reached",
		},
		{
			name: "whitespace is not content",
			outcome: Outcome{
				Kind: OutcomeExecutionFailure, Envelope: json.RawMessage("  \n\t"),
				Detail: "the provider session disconnected before the phase produced an envelope",
			},
			wantsCause: "the provider session disconnected",
		},
		{
			name: "an envelope with content is the sole account",
			outcome: Outcome{
				Kind: OutcomeExecutionFailure, Envelope: json.RawMessage(`{"status":"failed"}`),
				Detail: "provider error rate_limit",
			},
		},
		{
			name:    "no detail, nothing to say",
			outcome: Outcome{Kind: OutcomeExecutionFailure},
		},
		{
			name:    "blank detail is not a cause",
			outcome: Outcome{Kind: OutcomeExecutionFailure, Detail: "   "},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := outcomeDetailCause(test.outcome)
			if test.wantsCause == "" {
				if cause != nil {
					t.Fatalf("cause = %v, want none", cause)
				}
				return
			}
			if cause == nil || !strings.Contains(cause.Error(), test.wantsCause) {
				t.Fatalf("cause = %v, want one naming %q", cause, test.wantsCause)
			}
		})
	}
}

// The end-to-end half of the rule for the park a run actually rests on: an
// execution failure with an empty envelope now names the provider error, and
// the `agent-error` reason it parks under is unchanged.
func TestExecutionFailureWithNoEnvelopeParksWithTheProviderErrorNamed(t *testing.T) {
	h := newPauseHarness(t)
	item := startPausableItem(t, h, "item", "thread-one")

	h.runner.complete(t, item, Outcome{
		Kind:   OutcomeExecutionFailure,
		Detail: "provider error rate_limit: Claude usage limit reached",
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, item, StateNeedsHuman, ReasonAgentError)
	requireParkCause(t, h.phaseAttempt(t, item, "work", 1),
		"provider error rate_limit", "Claude usage limit reached")
}

// The other side of it: a failure the element DID author an envelope for keeps
// that envelope as its only account, so the two never disagree.
func TestExecutionFailureWithAnEnvelopeCarriesNoEngineCause(t *testing.T) {
	h := newPauseHarness(t)
	item := startPausableItem(t, h, "item", "thread-one")

	h.runner.complete(t, item, Outcome{
		Kind:     OutcomeExecutionFailure,
		Envelope: json.RawMessage(`{"status":"failed","reason":"the build never converged"}`),
		Detail:   "provider error rate_limit: Claude usage limit reached",
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, item, StateNeedsHuman, ReasonAgentError)
	attempt := h.phaseAttempt(t, item, "work", 1)
	if attempt.ParkCause != "" {
		t.Fatalf("park cause = %q, want none: the envelope is the account", attempt.ParkCause)
	}
	if !strings.Contains(string(attempt.OutputEnvelope), "the build never converged") {
		t.Fatalf("output envelope = %s, want the element's own account", attempt.OutputEnvelope)
	}
}

// The same rule carries the runner's account onto a
// `provider-retries-exhausted` park: the transient ladder's terminal diagnosis
// is persisted even though no agent authored an envelope.
func TestTransientExhaustionCarriesItsDetailOntoTheParkedAttempt(t *testing.T) {
	h := newPauseHarness(t)
	item := startPausableItem(t, h, "item", "thread-one")

	h.runner.complete(t, item, Outcome{
		Kind:   OutcomeTransientExhausted,
		Detail: "the provider stayed overloaded through every retry",
	})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, item, StateNeedsHuman, ReasonProviderRetriesExhausted)
	requireParkCause(t, h.phaseAttempt(t, item, "work", 1),
		"provider stayed overloaded", "every retry")
}

// A unit's note follows the same rule, for the same reason: the wave's record of
// why one lane failed is either the lane's envelope or the runner's account of
// the failure, never both.
func TestUnitOutcomeNoteFollowsTheSameEnvelopeRule(t *testing.T) {
	failed := unitOutcomeNote(Outcome{
		Kind: OutcomeExecutionFailure, Detail: "provider error rate_limit",
	})
	if !strings.Contains(failed, "unit outcome execution-failure") ||
		!strings.Contains(failed, "provider error rate_limit") {
		t.Fatalf("note = %q, want the kind and the runner's account", failed)
	}
	authored := unitOutcomeNote(Outcome{
		Kind:     OutcomeExecutionFailure,
		Envelope: json.RawMessage(`{"status":"failed"}`),
		Detail:   "provider error rate_limit",
	})
	if strings.Contains(authored, "provider error") {
		t.Fatalf("note = %q, want the envelope left as the sole account", authored)
	}
	if note := unitOutcomeNote(Outcome{Kind: OutcomeDone, Detail: "ignored"}); note != "" {
		t.Fatalf("a done unit carried a note %q", note)
	}
}

// ContinuableReasons is what an app-side refusal names, so it cannot fall
// behind a reason this package later admits — and handing out the backing array
// would let a caller edit the rule it is only reading.
func TestContinuableReasonsIsTheMembershipAndACopy(t *testing.T) {
	reported := ContinuableReasons()
	if len(reported) != len(continuableReasons) {
		t.Fatalf("ContinuableReasons() = %+v, want %+v", reported, continuableReasons)
	}
	for index, reason := range continuableReasons {
		if reported[index] != reason {
			t.Fatalf("ContinuableReasons()[%d] = %q, want %q", index, reported[index], reason)
		}
		if !ContinuableReason(reason) {
			t.Fatalf("%q is reported but not continuable", reason)
		}
	}
	reported[0] = "not-a-reason"
	if continuableReasons[0] == "not-a-reason" {
		t.Fatal("ContinuableReasons() handed out the rule itself")
	}
}
