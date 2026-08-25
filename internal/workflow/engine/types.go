package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"
)

// ErrSetupFailed marks runner startup failures caused by workspace
// provisioning or setup hooks. The engine maps wrapped instances to the
// setup-failed reason instead of reporting an agent failure.
var ErrSetupFailed = errors.New("workflow setup failed")

// ErrWiringFailed marks runner startup failures where the frozen definition and
// the live project profile cannot produce runnable work — a tool phase whose
// binding the profile no longer declares, an argument referencing a variable
// that is not in scope. The engine maps wrapped instances to the wiring-error
// reason, the same reason a gate that matched nothing parks with.
var ErrWiringFailed = errors.New("workflow wiring failed")

// ErrProviderContextUnavailable means a runner proved that a selected prior
// provider context no longer exists. It is not an agent failure: the engine
// supersedes the unsent continuation or warm reuse and reconstructs its logical
// round in a fresh provider session with a full prompt.
var ErrProviderContextUnavailable = errors.New("workflow provider context unavailable")

type State string

// A run has no queued state: admitting an item starts it. Contention is a
// phase waiting on resource capacity while its item stays running.
const (
	StateRunning    State = "running"
	StateNeedsHuman State = "needs-human"
	StateDone       State = "done"
	StateFailed     State = "failed"
	StateCancelled  State = "cancelled"
)

type Reason string

const (
	ReasonGate            Reason = "gate"
	ReasonQuestion        Reason = "question"
	ReasonStuck           Reason = "stuck"
	ReasonStalled         Reason = "stalled"
	ReasonBudgetExhausted Reason = "budget-exhausted"
	// ReasonRetriesExhausted is the legacy, ambiguous spelling used before
	// provider retry exhaustion and workflow loop exhaustion were separated.
	// Existing persisted runs keep it because their original cause cannot be
	// reconstructed reliably; new transitions must use one of the two specific
	// reasons below.
	ReasonRetriesExhausted         Reason = "retries-exhausted"
	ReasonProviderRetriesExhausted Reason = "provider-retries-exhausted"
	ReasonProviderUsageLimited     Reason = "provider-usage-limited"
	ReasonLoopLimitExhausted       Reason = "loop-limit-exhausted"
	ReasonCheckFailedGenuine       Reason = "check-failed-genuine"
	ReasonAgentError               Reason = "agent-error"
	ReasonWiringError              Reason = "wiring-error"
	ReasonDisposition              Reason = "disposition"
	ReasonSetupFailed              Reason = "setup-failed"
	ReasonInterrupted              Reason = "interrupted"
	ReasonTakenOver                Reason = "taken-over"
	ReasonUnitFailed               Reason = "unit-failed"
	ReasonChildFailed              Reason = "child-failed"
	// ReasonPaused is a deliberate stop — the human pause action or the
	// graceful-quit path. It resumes exactly like ReasonInterrupted; the
	// distinct reason is what tells a human whether the run stopped on purpose
	// or because the process died (spec §12, D23).
	ReasonPaused Reason = "paused"
	// ReasonCheckpoint is the soft stop firing: the run reached the call
	// boundary its tree was asked to stop at, and parked instead of invoking
	// the next call (spec §12, D36). Nothing was interrupted — the attempt
	// never started work — so resuming takes the call edge the park skipped.
	ReasonCheckpoint Reason = "checkpoint"
)

// resumableReasons is ResumableReason's whole membership, and continuableReasons
// is ContinuableReason's — derived from it rather than restated, so a resumable
// reason cannot be continuable-by-omission. Both predicates and every refusal
// that has to NAME the set read these, which is what stops a message from
// falling behind the rule it explains.
var (
	resumableReasons   = []Reason{ReasonPaused, ReasonInterrupted, ReasonCheckpoint}
	continuableReasons = append(append([]Reason(nil), resumableReasons...),
		ReasonUnitFailed, ReasonProviderRetriesExhausted, ReasonProviderUsageLimited, ReasonRetriesExhausted)
)

// ResumableReason reports whether a park continues where it stopped rather than
// being re-entered from scratch. Every member stopped an attempt before the
// phase produced a result, so the next step is a continuation — of the provider
// session for a `paused`/`interrupted` turn, of the skipped invocation for a
// `checkpoint` call boundary. The reasons differ for the human reading the run
// list, not for the recovery.
func ResumableReason(reason Reason) bool {
	return slices.Contains(resumableReasons, reason)
}

// ContinuableReason reports whether a bare resume continues the parked attempt
// instead of re-entering the phase with a fresh one. It is ResumableReason plus
// the two parks whose attempt holds work a fresh entry would throw away:
//
//   - `unit-failed` rests on a fan-out whose finished units — and the call
//     children they ran — are exactly what re-expansion would redo.
//   - `provider-retries-exhausted` rests on a phase whose turn DIED mid-flight,
//     after the runner's transient-retry layer gave up on a provider API
//     failure. Nothing
//     stopped it on purpose, which is why it reads like a fault and was entered
//     fresh for so long — but the session it died in is still there, holding the
//     context of a turn that may have run for many minutes, and both halves of
//     the precedent already exist here: the transient layer itself re-sends into
//     that SAME live session between backoffs, and an `interrupted` park
//     continues a session whose process died outright. Discarding that context
//     bought nothing.
//
// Naming a phase is always the fresh entry, including when the phase named is
// the parked one; that is the whole meaning of `run resume --phase <id>`.
//
// Legacy `retries-exhausted` rows remain continuable because that was their
// shipped recovery contract and their original cause is not reconstructible.
// A new `loop-limit-exhausted` park is intentionally not in this set: it has no
// dead provider turn to continue. Re-entering an earlier phase with `--phase`
// is what enters the cycle from outside and refills its bound.
//
// It is deliberately NOT what decides whether a resume cascades into a parked
// DESCENDANT. A child resting `unit-failed` needs a human's judgment about its
// units, so those sites stay on ResumableReason and the child keeps its park.
func ContinuableReason(reason Reason) bool {
	return slices.Contains(continuableReasons, reason)
}

// ContinuableReasons is ContinuableReason's membership, for a caller OUTSIDE
// this package whose refusal has to name the set — the same rule the internal
// refusals follow, so an app-side message cannot fall behind a new member.
func ContinuableReasons() []Reason { return slices.Clone(continuableReasons) }

// continuableReasonList renders ContinuableReason's membership for a refusal
// that has to say which parks it applies to.
func continuableReasonList() string {
	names := make([]string, len(continuableReasons))
	for index, reason := range continuableReasons {
		names[index] = string(reason)
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}

type OutcomeKind string

const (
	OutcomeDone                 OutcomeKind = "done"
	OutcomeQuestion             OutcomeKind = "question"
	OutcomeStuck                OutcomeKind = "stuck"
	OutcomeStalled              OutcomeKind = "stalled"
	OutcomeTransientExhausted   OutcomeKind = "transient-exhausted"
	OutcomeProviderUsageLimited OutcomeKind = "provider-usage-limited"
	OutcomeExecutionFailure     OutcomeKind = "execution-failure"
	OutcomeStopped              OutcomeKind = "stopped"
	// OutcomeSetupFailure is the completion twin of the `ErrSetupFailed` start
	// sentinel: work that never became runnable, reported after the start's own
	// reply is already gone. The runner's start watchdog is what needs it — when
	// a wedged start will not return even after its context is cancelled, the
	// attempt is reported dead through this path instead, and a deadline and its
	// grace fallback must park a run the SAME way rather than leaving the reason
	// to depend on which of the two noticed.
	OutcomeSetupFailure OutcomeKind = "setup-failure"
)

// Outcome is a runner completion. Envelope has already passed provider-facing
// post-validation; execution failures may omit it.
//
// Detail is the RUNNER's account of an outcome the element never authored one
// for — the provider error a turn died on, the usage limit that stopped the
// retry ladder, the send that failed. It exists because the envelope is
// normally the account, and the outcomes that carry none used to reach a park
// with nothing at all: `execution-failure` with an empty envelope left no
// cause, no envelope, and nothing to diagnose from. It is used ONLY where the
// envelope is empty (`outcomeDetailCause`, `fsm.go`); an envelope with content
// stays the sole account, so nothing is ever double-written.
type Outcome struct {
	Kind                 OutcomeKind                        `json:"kind"`
	Envelope             json.RawMessage                    `json:"envelope,omitempty"`
	Detail               string                             `json:"detail,omitempty"`
	ProviderUsageScopeID store.WorkflowProviderUsageScopeID `json:"providerUsageScopeId,omitempty"`
}

// RunKey uniquely identifies one running piece of work: a phase attempt, or
// one unit of a fan-out phase attempt. An empty UnitID is the phase's own
// single attempt, so every existing key keeps meaning exactly what it meant.
type RunKey struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
	UnitID  string `json:"unitId,omitempty"`
}

// UnitKind separates a fan-out's parallel workers from the join that runs after
// they rest. The join is an ordinary unit whose envelope becomes the phase's.
type UnitKind string

const (
	UnitWork UnitKind = "unit"
	UnitJoin UnitKind = "join"
)

type turnLaunchKind uint8

const (
	turnLaunchFresh turnLaunchKind = iota
	turnLaunchReuse
	turnLaunchContinue
	turnLaunchFinalize
)

// TurnLaunch is the complete provider-context decision for one runner start.
// Its fields are private so a caller cannot express the invalid combinations
// the former PriorThreadID + PromptMode + FinalizeTakeover fields allowed.
//
// The zero value is a fresh provider thread with a full prompt. ReuseThread
// keeps a provider thread for a new logical task and therefore still sends a
// full prompt. ContinueThread resumes the task already in that provider thread
// and sends only the continuation delta. FinalizeThread is the same-context
// takeover completion whose dedicated prompt and schema reattachment must not
// be expressible on any other launch.
type TurnLaunch struct {
	kind     turnLaunchKind
	threadID string
}

func FreshTurn() TurnLaunch { return TurnLaunch{} }

func ReuseThread(threadID string) (TurnLaunch, error) {
	return turnLaunch(turnLaunchReuse, threadID)
}

func ContinueThread(threadID string) (TurnLaunch, error) {
	return turnLaunch(turnLaunchContinue, threadID)
}

func FinalizeThread(threadID string) (TurnLaunch, error) {
	return turnLaunch(turnLaunchFinalize, threadID)
}

func turnLaunch(kind turnLaunchKind, threadID string) (TurnLaunch, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return TurnLaunch{}, errors.New("workflow turn launch: reused thread id is required")
	}
	return TurnLaunch{kind: kind, threadID: threadID}, nil
}

func (l TurnLaunch) ThreadID() string { return l.threadID }

func (l TurnLaunch) ReusesThread() bool { return l.kind != turnLaunchFresh }

func (l TurnLaunch) ContinuesThread() bool {
	return l.kind == turnLaunchContinue || l.kind == turnLaunchFinalize
}

func (l TurnLaunch) FinalizesTakeover() bool { return l.kind == turnLaunchFinalize }

func (l TurnLaunch) Validate() error {
	switch l.kind {
	case turnLaunchFresh:
		if l.threadID != "" {
			return errors.New("workflow turn launch: a fresh turn cannot name a prior thread")
		}
	case turnLaunchReuse, turnLaunchContinue, turnLaunchFinalize:
		if strings.TrimSpace(l.threadID) == "" {
			return errors.New("workflow turn launch: reused thread id is required")
		}
	default:
		return fmt.Errorf("workflow turn launch: unknown kind %d", l.kind)
	}
	return nil
}

// RunRequest contains the immutable workflow snapshot plus phase-local input.
// Unit, UnitIndex, UnitKind, and UnitAttempt are set exactly when Key.UnitID
// is: they carry the stamped unit definition a fan-out attempt expanded and the
// try number it is on, and Vars already includes the element binding a dynamic
// expansion bound to it (or, for a join, the units it consolidates).
type RunRequest struct {
	Key      RunKey         `json:"key"`
	Item     store.WorkItem `json:"item"`
	Workflow def.Workflow   `json:"workflow"`
	// Guidance is the operator guidance this turn must render. It is empty for a
	// same-context continuation, inherited for a reconstructed round, and newly
	// delivered on a fresh phase boundary. It rides the request rather
	// than the feedback because it is a different thing said by a different
	// author: feedback is the gate's own words to the next round, and this is a
	// person's, quoted as untrusted data by the prompt that renders it. A
	// fan-out's units and its join carry the same entries their phase entry
	// delivered — the block is part of prompt assembly, which every element of
	// the attempt goes through.
	Guidance []GuidanceEntry `json:"guidance,omitempty"`
	// WorkspaceNeed is the run's frozen workspace decision (§9) — derived at
	// start with write-need propagated through the call graph, so a read-only
	// root that calls a writing child still provisions the worktree its whole
	// tree shares. The runner provisions against this, never against a fresh
	// derivation from Workflow alone, which cannot see the child.
	WorkspaceNeed def.WorkspaceNeed `json:"workspaceNeed,omitempty"`
	// Phase is the frozen phase this work belongs to, with ONE field resolved
	// rather than copied: `Prompt` carries the body this attempt renders, which
	// is the phase's own unless the loop route that created the attempt declared
	// a `prompt:` override. The substitution happens here, at the single point
	// the request is built, so no consumer can render a phase's body while a
	// route override was in force — and every other field is the snapshot's.
	Phase       def.Phase      `json:"phase"`
	Unit        *def.Unit      `json:"unit,omitempty"`
	UnitIndex   int            `json:"unitIndex,omitempty"`
	UnitKind    UnitKind       `json:"unitKind,omitempty"`
	UnitAttempt int            `json:"unitAttempt,omitempty"`
	Vars        map[string]any `json:"vars"`
	// Feedback is the prior round's words to this one — an answered question's
	// answer, a gate reject's note with its declared values, a reopened unit's
	// retry diagnosis, the engine's own provenance sentences. It rides beside
	// Guidance because it has a different author: the gate's (or the engine's)
	// words rather than a person's. The note half is a durable debt: persisted
	// on the attempt row before this request is built, and owed until the send
	// door acks a prompt that rendered it (`AckFeedbackRendered`), so a start
	// whose opening send is dropped leaves it to be redelivered, never lost.
	Feedback *Feedback `json:"feedback,omitempty"`
	Launch   TurnLaunch
}

type Feedback struct {
	Values map[string]any `json:"values,omitempty"`
	Note   string         `json:"note,omitempty"`
}

// PhaseInput is the frozen input of one phase attempt. Args is set only for a
// call phase: it is the evaluated argument map the child run was seeded with,
// which is what makes an invocation reproducible from the run record alone.
type PhaseInput struct {
	Vars     map[string]any `json:"vars"`
	Feedback *Feedback      `json:"feedback,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
	// PromptRoute names the loop route whose `prompt:` override this attempt
	// rendered, and is absent for every attempt that rendered its phase's own
	// body. It is a COORDINATE rather than the body: the frozen snapshot already
	// holds every route's inlined prompt, so recording the reference makes the
	// override recoverable — by a resume that continues this round, and by a
	// human asking what an attempt actually ran — without copying a prompt file
	// into every attempt row.
	PromptRoute *PromptRoute `json:"promptRoute,omitempty"`
	// Guidance is the operator guidance belonging to this logical round. A
	// same-context continuation preserves it here for provenance and possible
	// reconstruction without rendering the block again; a fresh or reconstructed
	// turn does render it. Once the pending slot is cleared, this is the durable
	// record of the instruction the round ran under.
	Guidance []GuidanceEntry `json:"guidance,omitempty"`
}

// PromptRoute is the gate route an attempt took its prompt body from: the phase
// whose gate it is, and the route's index in that gate. Together with the frozen
// snapshot it resolves to the inlined override body.
type PromptRoute struct {
	PhaseID    string `json:"phaseId"`
	RouteIndex int    `json:"routeIndex"`
}

// GuidanceEntry is one piece of operator guidance waiting for a run's next fresh
// phase entry — the thread→run direction, mirroring `notify:`'s run→thread one.
//
// At and By are stamped by the engine from the authenticated caller, never by
// the author: guidance is quoted into a prompt as data, and an entry that could
// claim to come from a human when a phase wrote it would be exactly the claim
// worth forging.
type GuidanceEntry struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
	// By is `human` for an interactive session and `phase` for a scoped workflow
	// phase session.
	By GuidanceAuthor `json:"by"`
	// ByRun is the run the authoring phase belongs to, empty for a human.
	ByRun string `json:"byRun,omitempty"`
}

// GuidanceAuthor is the closed vocabulary of who left a guidance entry.
type GuidanceAuthor string

const (
	GuidanceByHuman GuidanceAuthor = "human"
	GuidanceByPhase GuidanceAuthor = "phase"
)

// GuidanceDraft is what a caller may say: the text, and the identity the app
// resolved from the caller's credential. The timestamp is the engine's.
type GuidanceDraft struct {
	Text  string
	By    GuidanceAuthor
	ByRun string
}

// GuidanceState is the slot after an append: every entry still pending, oldest
// first, plus where the run is, so the caller can say when it will be read.
type GuidanceState struct {
	ItemID  string          `json:"itemId"`
	Pending []GuidanceEntry `json:"pending"`
	State   State           `json:"state"`
	Reason  Reason          `json:"reason,omitempty"`
	PhaseID string          `json:"phaseId,omitempty"`
	// Quarantined is set when THIS call's read found a slot that would not
	// decode and healed it (`healGuidanceSlot`). It is a fact about this append
	// and is stored nowhere: the caller's entry is on the slot, and whatever the
	// column held before it is not, so the one person who can act on that — the
	// operator who just wrote here — has to be told in the same answer.
	Quarantined *GuidanceQuarantine `json:"quarantined,omitempty"`
}

// GuidanceQuarantine describes a pending-guidance slot the engine discarded
// because it would not decode. It carries facts rather than a sentence: the
// surfaces that render it (`run guide`'s block, the desktop) write their own
// prose, exactly as they do for where a run reads its guidance.
type GuidanceQuarantine struct {
	// Bytes is the size of the discarded column. There is deliberately no entry
	// COUNT: counting them would mean decoding them, which is the thing that
	// could not be done.
	Bytes int `json:"bytes"`
	// Reason is the decode failure, so the record says what was wrong with it.
	Reason string `json:"reason"`
	// LogEvent names the engine-log event whose line holds the raw content —
	// the only surviving copy, and the whole reason the discard is recoverable
	// by a human even though it is not recoverable by the run.
	LogEvent string `json:"logEvent"`
}

// Guidance bounds. Both are refusals rather than trims: guidance is rendered
// verbatim into a prompt, and silently dropping half of what an operator wrote
// would be worse than telling them it did not fit.
const (
	// MaxGuidanceEntryBytes bounds one entry. It is a steering instruction, not
	// a specification — a phase's prompt file is where a long one belongs.
	MaxGuidanceEntryBytes = 4 * 1024
	// MaxGuidanceEntries bounds how many wait at once. Past this the slot has
	// stopped being "steer the next boundary" and become a backlog nobody
	// deliberately assembled.
	MaxGuidanceEntries = 8
)

type HumanDecision string

const (
	HumanApprove HumanDecision = "approve"
	HumanReject  HumanDecision = "reject"
)

type HumanIntervention struct {
	Decision HumanDecision `json:"decision"`
	Note     string        `json:"note,omitempty"`
}

// TakeoverInterventionKind is the persisted marker that distinguishes an
// attempt detached for human steering from a human gate decision, which shares
// the intervention column.
const TakeoverInterventionKind = "taken-over"

type TakeoverIntervention struct {
	Kind string `json:"kind"`
	At   int64  `json:"at"`
}

// Runner starts provider work on an engine-owned worker goroutine. Start must
// call entered exactly once, immediately on entry and before any blocking work.
// Start may then block while provisioning; its result is serialized back
// through the engine command loop. Stop is idempotent and returns any partial
// control envelope. Both stops are called ON that command loop — the sole
// owner of every run's FSM state — so an implementation must bound any wait it
// takes there: a send wedged on provider IO must not hold the loop, and work
// that outlives the bound belongs to a goroutine of the implementation's own.
type Runner interface {
	Start(context.Context, RunRequest, func(), func(Outcome)) error
	Stop(context.Context, RunKey) (json.RawMessage, error)
	StopForTakeover(context.Context, RunKey) (json.RawMessage, error)
}

// Emitter is implemented by the later app/channel wiring packet. Emit runs on
// the engine owner goroutine and must return promptly; it must not call back
// into Engine synchronously.
type Emitter interface {
	Emit(channel eventchan.Channel, payload any)
}

// ResolvedDefinition is one validated workflow plus the two facts only a
// resolver can answer about it: which scope it came from (§8 project-over-shared
// precedence), and the workspace it needs once write-need has been propagated
// through its call graph (§9). `def` stays pure — it derives the single
// definition's need — so the propagated answer is produced where the loading is.
type ResolvedDefinition struct {
	Workflow      def.Workflow      `json:"workflow"`
	Scope         def.Scope         `json:"scope"`
	WorkspaceNeed def.WorkspaceNeed `json:"workspaceNeed"`
}

// DefinitionSource resolves the validated workflow frozen into a run record.
// Resolve answers for an item at start and is not consulted after that item's
// Snapshot is persisted. ResolveCall answers a call phase's static target by id
// at call time, under §8 scoping, so every invocation freezes the definition
// that was on disk when it was invoked.
type DefinitionSource interface {
	Resolve(context.Context, store.WorkItem) (ResolvedDefinition, error)
	ResolveCall(ctx context.Context, projectID, workflowID string) (ResolvedDefinition, error)
}

// ProfileSource returns the live project profile at each resource acquisition.
type ProfileSource interface {
	Profile(context.Context, string) (*profile.Profile, error)
}

// Spend is the attributed provider spend accumulated by one work item.
//
// Tokens is exact: every settled turn's usage lands in the ledger whatever the
// provider reports about cost. USD is composed — wire-reported where the
// provider priced its own turn, rate-table estimated where it reported tokens
// only (Codex reports no cost anywhere on its wire). Estimated says some of USD
// came from the rate table, and Unpriced counts ledger rows whose model has no
// rate at all: their tokens are in Tokens, their dollars are in nothing, so a
// USD figure carrying them is a lower bound rather than a total.
type Spend struct {
	Tokens    int64   `json:"tokens"`
	USD       float64 `json:"usd"`
	Estimated bool    `json:"estimated,omitempty"`
	Unpriced  int64   `json:"unpriced,omitempty"`
}

// SpendSource supplies token and composed wire-plus-estimated USD spend for a
// whole run tree. Budgets are enforced against the root item across every run
// it called (§12), so the aggregate — not one item's rows — is the number a
// ceiling is compared against.
type SpendSource interface {
	TreeSpend(ctx context.Context, rootItemID string) (Spend, error)
}

// Config supplies the persisted engine state restored at startup. Bounded
// parallelism is a resource fact (project profile capacities), not a config
// knob, so the global pause kill switch is all that survives a restart.
type Config struct {
	Paused bool
	// Log receives the run-lifecycle record. It is optional: a nil sink falls
	// back to the standard logger, which is exactly where these lines went
	// before there was a sink, so an engine wired without one loses nothing.
	Log LogSink
}

// Engine log event kinds. They name what happened to a RUN, not what the code
// did, because the reader is someone asking why a run is where it is.
const (
	LogEventPark              = "park"
	LogEventCancel            = "cancel"
	LogEventResume            = "resume"
	LogEventDefinitionRefresh = "definition-refresh"
	LogEventSeedAmend         = "seed-amend"
	LogEventRebuild           = "rebuild"
	LogEventCapacity          = "capacity"
	// LogEventGuide is one `run guide` appending to the pending slot, and
	// LogEventGuidanceDeliver the phase entry that rendered it and cleared it.
	// Both are logged because the pair is what answers "an operator says they
	// steered this run — did it ever read it, and where".
	LogEventGuide           = "guide"
	LogEventGuidanceDeliver = "guidance-delivered"
	// LogEventFeedbackRedeliver is a phase entry that carried feedback forward
	// from an attempt no provider session ever rendered — an answered question
	// whose continuation never started, a gate note whose round was parked before
	// its turn. It is logged for the same reason the guidance pair is: an operator
	// who answered a question and then sees the run ask a second time needs the
	// record to say whether their answer was ever read, and this line is the only
	// place that fact is stated outside the attempt's own input envelope.
	LogEventFeedbackRedeliver = "feedback-redelivered"
	// LogEventGuidanceUndecodable is the quarantine record: the raw bytes of a
	// pending-guidance column that would not decode, written whole because the
	// heal that follows is what removes them from the run. It is the only
	// surviving copy, so this line is never truncated.
	LogEventGuidanceUndecodable = "guidance-undecodable"
	// LogEventLoopSession is a `session: continue` loop route that could not
	// continue: the target phase's previous session is gone, so the re-entry
	// started a cold one. It is a degraded continuation rather than an error —
	// the run keeps going — which is exactly why it has to be said somewhere.
	LogEventLoopSession = "loop-session"
	// LogEventBudgetUnread is the reserved `budget` read left unbound because the
	// ceiling or the tree's spend could not be read at that moment. It is a
	// prompt-surface degradation, never a park — enforcement refuses loudly on its
	// own path — so this line is the only record that an element rendered
	// "(not provided)" for a run that does have a ceiling.
	LogEventBudgetUnread = "budget-unread"
	// LogEventEmitTimeMissing is an emit site that reached `emitUnitStateAt`
	// without the time its store write persisted. It is an ENGINE BUG rather than
	// a runtime condition — every legitimate value comes from `timestamp()` — and
	// the line exists because the alternatives are both worse: dropping the event
	// leaves a live view holding a node that never moves again, and silently
	// stamping the clock lets a wrong stamp look exactly like a right one.
	LogEventEmitTimeMissing = "emit-time-missing"
	// LogEventAnswer is one accepted `Answer`, and LogEventTakeoverComplete one
	// accepted `CompleteTakeover`. They are logged for the reason a resume is:
	// each continues a parked attempt, each leaves no durable record of its own
	// beyond the attempt the continuation creates, and the operator who just
	// acted is the reader most likely to ask what happened next. An engine log
	// that goes silent from the moment a human answers a question is one a wedged
	// run cannot be diagnosed from — the run looks identical to one nobody
	// touched.
	LogEventAnswer           = "answer"
	LogEventTakeoverComplete = "takeover-complete"
	// LogEventRunnerStart is a runner start that reported SUCCESS. Every failure
	// already parks through teardown, which logs; success said nothing at all, so
	// the dispatch line above it was the last word about a turn that might never
	// have begun. That is what makes it worth a line of its own: it turns silence
	// into a fact. A dispatch with no start after it is a start that never
	// reported, not a start nobody logged.
	LogEventRunnerStart = "runner-start"
)

// LogEvent is one engine-significant record: a run parked with its cause, a
// cancel, a resume, a re-read definition, a rebuild decision, or a wave wider
// than the capacity it will contend on.
//
// It is a flat value rather than a formatted string so the app-side sink can
// write NDJSON a later reader can filter, and it carries the run coordinate on
// every line because "which run" is the first question asked of any of them.
type LogEvent struct {
	Event     string `json:"event"`
	ItemID    string `json:"itemId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	PhaseID   string `json:"phaseId,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	State     State  `json:"state,omitempty"`
	Reason    Reason `json:"reason,omitempty"`
	// ThreadID is the provider session a continuation was dispatched ONTO, on the
	// lines that have one: the thread an answer, a takeover finalize, or a warm
	// runner start is continuing. It is a field rather than prose because
	// correlating "the operator answered on this session" with "the runner
	// started on this session" is the whole question a stalled continuation
	// raises. A cold start carries none — its thread is created by the runner and
	// recorded on the attempt row, which is the authority for it either way.
	ThreadID string `json:"threadId,omitempty"`
	Message  string `json:"message,omitempty"`
}

// NotifyEvent announces that a gate took a route its author decorated with
// `notify:` and the run CONTINUED (K1). It is the engine's whole contribution
// to a progress wake: composing and delivering one is app wiring, exactly as it
// is for a resting run's wake, and the engine neither waits for it nor learns
// whether it landed.
//
// The coordinate is the phase the gate belongs to and the attempt it consumed,
// captured before the run moves on, so the app reads the outputs the gate
// actually decided on rather than whatever the run produced by the time the
// message is composed.
type NotifyEvent struct {
	ItemID     string `json:"itemId"`
	ProjectID  string `json:"projectId,omitempty"`
	PhaseID    string `json:"phaseId"`
	Attempt    int    `json:"attempt"`
	Decision   string `json:"decision"`
	Target     string `json:"target,omitempty"`
	RouteIndex int    `json:"routeIndex"`
}

// LogSink writes the engine's run-lifecycle log. It is called on the command
// goroutine, so an implementation must return promptly and must never call back
// into Engine.
type LogSink interface {
	LogEngineEvent(LogEvent)
}

// MaxParkCauseBytes bounds the engine text one park persists. A cause is a
// sentence — the resolved width, the argument that named no input, the call
// chain of a depth refusal — and the deepest of those (a call chain at
// MaxCallDepth) is what this is sized for. A cause past the bound is truncated
// rather than dropped: a shortened statement of what went wrong still beats
// none.
const MaxParkCauseBytes = 8 * 1024

const MaxSeedBytes = 64 * 1024
const MaxSnapshotBytes = 4 * 1024 * 1024

// DefaultProviderCapacity bounds concurrent agent phases and fan-out units per
// provider when the project profile does not declare a `provider:<name>`
// capacity. The value is def's so the scheduler and the dry-run's width report
// can never disagree about the bound a run actually gets.
const DefaultProviderCapacity = def.DefaultProviderCapacity

// ProviderResource is the implicit resource every agent-driver phase and every
// agent-driver fan-out unit acquires in addition to the phase's declared
// resources. Capacity comes from the live project profile like any other
// resource, defaulting to DefaultProviderCapacity.
func ProviderResource(provider string) string { return def.ProviderResource(provider) }

// Snapshot is the persisted, immutable run definition. WorkspaceNeed is frozen
// with it because the answer depends on definitions outside this one (§9: write
// need propagates through call edges) — re-deriving it later from the frozen
// graph alone would silently drop a called workflow's writes. A snapshot frozen
// before this field existed leaves it empty, and the runner falls back to the
// single-definition derivation, which is exactly what it did then.
type Snapshot struct {
	Workflow      def.Workflow      `json:"workflow"`
	WorkspaceNeed def.WorkspaceNeed `json:"workspaceNeed,omitempty"`
}

type StateEvent struct {
	ItemID    string `json:"itemId"`
	ProjectID string `json:"projectId"`
	From      State  `json:"from"`
	To        State  `json:"to"`
	Reason    Reason `json:"reason,omitempty"`
	// PhaseID and Attempt are where the run WAS when the transition happened —
	// the attempt a park rests on, which is the coordinate its cause and its
	// narrative are filed under. They are captured here rather than looked up by
	// a consumer because only the transition knows: by the time an observer
	// reads the row the run may have entered the next phase. A run that has not
	// reached a phase yet carries neither.
	PhaseID string `json:"phaseId,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

// EngineState is both the Paused query result and the `workflow:engine-state`
// event payload, so the live flag has exactly one wire shape.
type EngineState struct {
	Paused bool `json:"paused"`
}

// PhaseEvent reports one phase attempt's status, or — when UnitID is set — one
// fan-out unit's status inside that attempt. Units ride the phase channel
// rather than a parallel one: a unit is a piece of the attempt, and a consumer
// that ignores UnitID still sees exactly the phase timeline it saw before.
type PhaseEvent struct {
	ItemID    string   `json:"itemId"`
	PhaseID   string   `json:"phaseId"`
	Attempt   int      `json:"attempt"`
	Status    string   `json:"status"`
	UnitID    string   `json:"unitId,omitempty"`
	UnitIndex int      `json:"unitIndex,omitempty"`
	UnitKind  UnitKind `json:"unitKind,omitempty"`
	// OccurredAt is the engine's own event time in Unix milliseconds, guaranteed
	// by `emitPhaseState` — the only construction path — so no emit site can
	// forget it. Where the transition wrote a row, it is EXACTLY that row's
	// `started_at` / `ended_at`, so a patched view and a refetched one agree;
	// where nothing recorded a time, the emitter's clock fills in. A consumer
	// patching a live view reads it as the transition's moment: a `running`
	// status starts the attempt, a terminal one ends it. Without it a frontend
	// has to stamp its own clock, which drifts across reconnects and replay,
	// where an event's arrival says nothing about when it happened.
	OccurredAt int64 `json:"occurredAt"`
}

type ErrorEvent struct {
	ItemID          string `json:"itemId,omitempty"`
	Error           string `json:"error"`
	Spend           *Spend `json:"spend,omitempty"`
	WallClockMillis int64  `json:"wallClockMillis,omitempty"`
	detail          error
}

func (e ErrorEvent) Cause() error { return e.detail }

type persistence interface {
	CreateWorkItem(store.WorkItem) error
	GetWorkItem(string) (store.WorkItem, error)
	ListWorkItems(store.WorkItemListFilter) ([]store.WorkItem, error)
	UpdateWorkItemState(string, string, string, int64) error
	SetWorkItemSoftStop(string, bool) error
	UpdateWorkItemRunStart(string, json.RawMessage, string, string, string, int64) error
	UpdateWorkItemSeeds(string, json.RawMessage) error
	WorkItemPendingGuidance(string) (json.RawMessage, error)
	SetWorkItemPendingGuidance(string, json.RawMessage) error
	CreateWorkItemPhase(store.WorkItemPhase) error
	CompleteWorkItemPhase(string, string, int, json.RawMessage, json.RawMessage, string, string, store.WorkflowProviderUsageScopeID, int64) error
	ReopenWorkItemPhase(string, string, int) error
	MarkWorkItemPhaseFeedbackDelivered(string, string, int, int64) error
	ListUndeliveredWorkItemPhaseFeedback(string, string, int) ([]store.WorkItemPhaseFeedback, error)
	ListWorkItemPhases(string) ([]store.WorkItemPhase, error)
	ListWorkItemPhaseContexts(string) ([]store.WorkItemPhaseContext, error)
	UpdateWorkItemPhaseIntervention(string, string, int, json.RawMessage) error
	ListWorkItemChildren(string) ([]store.WorkItem, error)
	ListWorkItemCallChildren(string, string, int) ([]store.WorkItem, error)
	ListWorkItemUnitCallChildren(string, string, int, string) ([]store.WorkItem, error)
	CreateWorkItemUnits([]store.WorkItemUnit) error
	StartWorkItemUnit(string, string, int, string, int, string, int64) error
	CompleteWorkItemUnit(string, string, int, string, string, json.RawMessage, string, store.WorkflowProviderUsageScopeID, int64) error
	RetryWorkItemUnit(string, string, int, string, int, string) error
	FailRunningWorkItemUnits(string, string, int, string, int64) (int64, error)
	ListWorkItemPhaseUnits(string, string, int) ([]store.WorkItemUnit, error)
	ListProjects() ([]store.Project, error)
	// ThreadExists is only the cheap persisted target check for reuse. The runner
	// proves provider context from either a live process or a durable cursor
	// before it sends anything.
	ThreadExists(string) (bool, error)
}
