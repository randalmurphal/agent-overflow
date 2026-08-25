package main

import (
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/workflow/engine"
)

// A provider usage refusal is the one transient-looking failure the retry
// ladder must never retry. Provider adapters normalize their native wire enums
// onto FailureReasonUsageLimit; this layer deliberately does not parse prose or
// infer a model/bucket from rate-limit windows. The typed refusal is enough to
// park immediately, and a normal explicit resume always makes another real
// provider attempt regardless of any recorded outage.

// maxWorkflowFailureDetailRunes bounds one runner-authored park cause. The
// engine bounds what it persists at MaxParkCauseBytes, but provider prose also
// lands on one run-status line and in a wake.
const maxWorkflowFailureDetailRunes = 400

func workflowUsageLimitRefusal(event provider.ProviderEvent) bool {
	return event.Kind == provider.EventError && event.Failure != nil &&
		event.Failure.Reason == provider.FailureReasonUsageLimit
}

// parkForUsageLimit synchronously detaches the refused attempt, then records
// the provider/account identity captured at the send boundary and parks off the
// provider event goroutine. Losing correlation metadata never turns the
// refusal back into a retry: the run still parks under the typed reason and
// receives an ordinary per-run wake. That fail-open fallback can duplicate
// notifications, but cannot hide or hammer a spent account.
func (r *workflowAppRunner) parkForUsageLimit(
	runKey, itemID string,
	identity providerDispatchIdentity,
	identified bool,
	detail string,
) {
	attempt, ok := r.detach(runKey)
	if !ok {
		return
	}
	go func() {
		usageScopeID := store.WorkflowProviderUsageScopeID(0)
		if identified {
			var err error
			usageScopeID, err = r.store.OpenWorkflowProviderUsageScope(
				identity.Provider, identity.AccountID, identity.CredentialGeneration, r.now().UnixMilli(),
			)
			if err != nil {
				log.Printf("workflow runner: record provider usage scope for %s: %v; parking without cross-run notification coalescing", itemID, err)
				r.host.emit(eventchan.WorkflowError, engine.ErrorEvent{
					ItemID: itemID,
					Error:  "the provider usage limit was recognized, but its notification correlation could not be recorded; the run was still parked without retries",
				})
			}
		} else {
			log.Printf("workflow runner: provider usage limit for %s arrived without a captured dispatch identity; parking without cross-run notification coalescing", itemID)
		}
		r.stopDetachedAttempt(runKey, attempt, engine.Outcome{
			Kind: engine.OutcomeProviderUsageLimited, Detail: detail,
			ProviderUsageScopeID: usageScopeID,
		})
	}()
}

// workflowTurnCompleteFailureDetail renders the error a turn closed with, for
// the turn-complete path that has no error event to quote.
func workflowTurnCompleteFailureDetail(event provider.ProviderEvent) string {
	meta, ok := event.TurnComplete.(*provider.WireTurnCompleteMeta)
	if !ok || meta == nil {
		return "the turn completed with an error and no envelope"
	}
	if message := strings.TrimSpace(meta.ErrorMessage); message != "" {
		return "the turn completed with an error: " + message
	}
	return "the turn completed with an error and no envelope"
}

// workflowProviderErrorDetail renders one provider error event as the runner's
// account of a failure the element authored no envelope for. Content is the
// provider's own summary line; the normalized typed code is appended when
// there is one.
func workflowProviderErrorDetail(event provider.ProviderEvent) string {
	summary := strings.TrimSpace(event.Content)
	code := ""
	if event.Failure != nil {
		code = strings.TrimSpace(event.Failure.Code)
	}
	switch {
	case summary != "" && code != "":
		return workflowFailureDetail(fmt.Sprintf("provider error %s: %s", code, summary))
	case summary != "":
		return workflowFailureDetail("provider error: " + summary)
	case code != "":
		return workflowFailureDetail("provider error " + code)
	default:
		return ""
	}
}

// workflowFailureDetail is the one bound every runner-authored detail passes
// through, so no path can put an unbounded provider string on a park cause.
func workflowFailureDetail(detail string) string {
	return textgen.CapRunesWithEllipsis(strings.TrimSpace(detail), maxWorkflowFailureDetailRunes)
}
