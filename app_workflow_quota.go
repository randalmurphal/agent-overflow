package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/workflow/engine"
)

// A quota refusal is the one transient provider failure a backoff ladder cannot
// fix. The ladder exists for failures that clear on their own within seconds —
// an overloaded server, a dropped stream — and it burns its whole schedule in
// under ten minutes. A spent usage allowance clears when the provider says it
// does, which is hours or days away, so retrying against it wastes every attempt
// and then parks a generic `retries-exhausted` that reads like a fault. The run
// then sat there until a human noticed, which for a five-day weekly limit meant
// a five-day stall behind a human alarm clock.
//
// So a recognized quota refusal that carries a reset boundary stops the ladder
// where it stands, parks through the same `OutcomeTransientExhausted` path with
// a cause that states the boundary and the self-resume, and arms the timer that
// brings the run back. A refusal that carries NO boundary is left exactly as it
// was: the ladder runs, and the park is the generic one. There is nothing to
// come back at, and inventing a delay would be a guess dressed as a schedule.

// workflowQuotaExhaustedPercent is the band a rate-limit window has to be in
// before it is read as the one that refused the turn.
//
// The refusal itself is the proof that an allowance is spent — this number only
// decides WHICH window, among the several a provider reports, the boundary
// should be taken from. It is a band rather than an equality because both
// providers report a computed percentage (Claude's `utilization` is a float
// fraction; Codex's `usedPercent` comes off a response header), and an equality
// against 100 would silently disable the whole mechanism on a server that
// rounded down.
const workflowQuotaExhaustedPercent = 99.0

// The jitter added to a stated reset boundary. Limits routinely lag their own
// stated reset by a few seconds, so resuming exactly on the boundary re-park
// costs a provider turn; and a campaign whose whole wave parked on one limit
// would otherwise resume every one of its runs in the same second, which is the
// burst the provider just refused.
//
// It is derived from the run id rather than drawn at random so the same park
// always resumes at the same moment — a scheduled time an operator reads in a
// park cause and in `run status` has to be the time the timer actually holds.
const (
	workflowQuotaResumeJitterMin  = 1 * time.Minute
	workflowQuotaResumeJitterSpan = 2 * time.Minute
)

// maxWorkflowFailureDetailRunes bounds one runner-authored park cause. The
// engine bounds what it PERSISTS at `MaxParkCauseBytes`, but a provider error
// message is prose of unknown length that lands on a `run status` line and in a
// wake, and a cause nobody can read at a glance is a cause nobody reads.
const maxWorkflowFailureDetailRunes = 400

// workflowQuotaRefusal reports whether a provider error event is the provider
// declining the turn because the account's usage allowance is spent — as
// opposed to a transient failure a retry can clear.
//
// Both signals are the providers' own typed enums, never message text:
//
//   - Claude: the `assistant.error` enum `rate_limit`, normalized onto
//     `api_error_enum` by `internal/provider/claude/parse_assistant.go`. Every
//     429 branch of claude-code's `src/services/api/errors.ts` sets it,
//     including the capacity and entitlement 429s that are NOT quota — which is
//     why the reset boundary, not this predicate, is what decides whether a
//     refusal is one this mechanism acts on.
//   - Codex: `codexErrorInfo: "usageLimitExceeded"`
//     (`codex-rs/protocol/src/protocol.rs` `CodexErrorInfo::UsageLimitExceeded`,
//     produced from `CodexErr::UsageLimitReached`).
//
// It deliberately does not widen `workflowTransientError`: both values are
// already transient there, and a quota refusal that produces no boundary must
// keep taking exactly the path it takes today.
func workflowQuotaRefusal(providerName string, event provider.ProviderEvent) bool {
	if event.Kind != provider.EventError || len(event.Meta) == 0 {
		return false
	}
	var meta struct {
		APIErrorEnum string `json:"api_error_enum"`
		Wire         struct {
			Error struct {
				CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		} `json:"wire"`
	}
	if json.Unmarshal(event.Meta, &meta) != nil {
		return false
	}
	switch providerName {
	case string(provider.Claude):
		return meta.APIErrorEnum == "rate_limit"
	case string(provider.Codex):
		var scalar string
		return json.Unmarshal(meta.Wire.Error.CodexErrorInfo, &scalar) == nil && scalar == "usageLimitExceeded"
	}
	return false
}

// workflowQuotaReset picks the moment a refused turn can be retried from the
// rate-limit windows the session itself reported.
//
// The windows are the structured half of the refusal, and both providers put
// them on the wire at the moment they refuse:
//
//   - Codex emits `account/rateLimits/updated` from the usage-limit error
//     itself — `codex-rs/core/src/session/turn.rs` calls `update_rate_limits`
//     with the snapshot attached to `CodexErr::UsageLimitReached` before it
//     returns the error, and `codex-rs/app-server/src/bespoke_event_handling.rs`
//     turns that into the notification. The reset boundary reaches us as
//     `resetsAt`; the human-formatted "Try again at …" in the error message is
//     the same fact rendered in the CLI's local time zone, and is deliberately
//     not parsed.
//   - Claude emits `rate_limit_event` with `status: "rejected"` from its own 429
//     handler (`extractQuotaStatusFromError` → `emitStatusChange`), carrying
//     `resetsAt` from the `anthropic-ratelimit-unified-reset` header.
//
// Selection is the EARLIEST future boundary among the exhausted windows, which
// is the self-correcting direction. Picking the latest would be silently
// unrecoverable: over-sleeping a five-hour window because a weekly one also
// read full costs a day nobody can get back. Resuming too early costs one
// provider turn and re-parks with whatever boundary is then still in force —
// and once the short window has cleared, only the long one is still exhausted,
// so the choice converges on the true unblock.
func workflowQuotaReset(limits []provider.RateLimitEntry, now time.Time) (time.Time, bool) {
	var earliest time.Time
	for _, entry := range limits {
		if entry.UsedPercent < workflowQuotaExhaustedPercent || entry.ResetsAt <= 0 {
			continue
		}
		resetsAt := time.Unix(entry.ResetsAt, 0)
		if !resetsAt.After(now) {
			continue
		}
		if earliest.IsZero() || resetsAt.Before(earliest) {
			earliest = resetsAt
		}
	}
	return earliest, !earliest.IsZero()
}

// workflowQuotaResumeJitter spreads one run's self-resume past the stated
// boundary, deterministically from its id. See the constants above.
func workflowQuotaResumeJitter(itemID string) time.Duration {
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(itemID))
	return workflowQuotaResumeJitterMin +
		time.Duration(uint64(digest.Sum32())%uint64(workflowQuotaResumeJitterSpan))
}

// workflowQuotaScheduledCause is what the park says once the self-resume is
// actually armed. It states both moments because they are different answers to
// different questions — when the provider says the allowance returns, and when
// this run will act on that — and every surface that renders a park cause
// (`run status`, `run inspect`, the wake) gets both for free.
func workflowQuotaScheduledCause(resetsAt, resumeAt time.Time) string {
	return fmt.Sprintf(
		"provider usage limit reached; the limit resets %s, and this run resumes itself at %s",
		resetsAt.Local().Format(time.RFC3339), resumeAt.Local().Format(time.RFC3339),
	)
}

// workflowQuotaUnscheduledCause is the same park when the schedule could NOT be
// written. It keeps the reset boundary, which is true whatever the store did,
// and drops the self-resume clause, which would otherwise be a promise nothing
// armed — the run is waiting for a human now, and the cause has to say so by
// not saying otherwise.
func workflowQuotaUnscheduledCause(resetsAt time.Time) string {
	return fmt.Sprintf(
		"provider usage limit reached; the limit resets %s, and this run could not schedule its own return — resume it once the limit is back",
		resetsAt.Local().Format(time.RFC3339),
	)
}

// quotaParkLocked answers whether this error is a quota refusal this run can
// schedule its own return from, and when. It is called with r.mu held and
// reads only the attempt.
//
// Both halves are required. A refusal with no reset boundary — a capacity 429,
// a spend cap, an account whose windows the session never reported — falls
// through to the ordinary retry ladder, because there is nothing to come back
// at and a fabricated delay would be a guess presented as a schedule.
//
// It deliberately returns the two MOMENTS rather than a rendered cause: what the
// park may claim depends on whether the schedule was written, which only
// `parkForQuotaLimit` finds out.
func (r *workflowAppRunner) quotaParkLocked(
	attempt *workflowAttempt, event provider.ProviderEvent,
) (resetsAt, resumeAt time.Time, ok bool) {
	if !workflowQuotaRefusal(attempt.provider, event) {
		return time.Time{}, time.Time{}, false
	}
	resetsAt, found := workflowQuotaReset(attempt.rateLimits, r.now())
	if !found {
		return time.Time{}, time.Time{}, false
	}
	return resetsAt, resetsAt.Add(workflowQuotaResumeJitter(attempt.key.ItemID)), true
}

// parkForQuotaLimit persists the self-resume BEFORE the park that carries it.
// The order is what the state listener depends on: parking is the one
// transition that does not clear a schedule, and a schedule written after the
// park would race the wake and the CLI reads that quote the cause.
//
// A failed write is not a failed park — but it IS a different park, so the cause
// is composed from what the write actually did rather than from what it was
// asked to do. A park that promises "this run resumes itself at 19:58" when
// nothing armed is a run that never comes back and a human who was told not to
// look at it.
//
// The stop goes off the wire (`stopAndFinishOffWire`) because this decision is
// taken on the provider event path with the process still alive to be
// interrupted — see that helper for why interrupting from here deadlocks.
func (r *workflowAppRunner) parkForQuotaLimit(runKey, itemID string, resetsAt, resumeAt time.Time) {
	cause := workflowQuotaScheduledCause(resetsAt, resumeAt)
	if err := r.app.setWorkflowAutoResume(itemID, resumeAt); err != nil {
		log.Printf("workflow runner: schedule self-resume for %s: %v", itemID, err)
		cause = workflowQuotaUnscheduledCause(resetsAt)
	}
	r.stopAndFinishOffWire(runKey, engine.Outcome{Kind: engine.OutcomeTransientExhausted, Detail: cause})
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
// provider's own summary line; the typed enum is appended when there is one,
// because "API error" alone does not distinguish an expired login from a
// refused prompt.
func workflowProviderErrorDetail(event provider.ProviderEvent) string {
	summary := strings.TrimSpace(event.Content)
	var meta struct {
		APIErrorEnum string `json:"api_error_enum"`
		Wire         struct {
			Error struct {
				CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		} `json:"wire"`
	}
	code := ""
	if len(event.Meta) > 0 && json.Unmarshal(event.Meta, &meta) == nil {
		switch {
		case meta.APIErrorEnum != "":
			code = meta.APIErrorEnum
		case len(meta.Wire.Error.CodexErrorInfo) > 0:
			code = strings.Trim(string(meta.Wire.Error.CodexErrorInfo), `"`)
		}
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
