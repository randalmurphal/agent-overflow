package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// session_turn.go — the turn verbs on a live session: `turn/start`,
// `turn/steer` and `turn/interrupt`, the per-turn output-schema binding that
// bridges Send's options to the turn id the wire hands back, and the
// turn-start dedupe ledger.
//
// Send and the ledger are two halves of one rule, which is why they sit
// together: a `turn/started` can reach the read loop before `turn/start`'s
// own response does, so Send CLAIMS the turn before the write
// (beginLocalTurnStart) and clearTurnStart releases what a timed-out claim
// left behind. external_turns.go reads an unclaimed `turn/started` as a turn
// somebody else began.
//
// Both verbs stamp `clientUserMessageId` (SendOptions.ClientUserMessageID).
// It is upstream's `Option<String>` correlation handle on TurnStartParams and
// TurnSteerParams alike, echoed back on the `userMessage` ThreadItem as
// `clientId` — which is how a caller matches an echo to the row it sent
// without relying on ordering. Supported since codex 0.136; AO's provider
// floor is 0.143, so it is unconditional and there is no version gate.

// Send sends a user turn via turn/start.
//
// The JSON-RPC response does not directly drive an EventTurnStart: the
// app-server reliably follows turn/start with a turn/started notification
// that ClassifyNotification turns into the event. Emitting here as well
// produced two EventTurnStart per user send (Bug B6). We still record the
// turn ID locally so Interrupt has something to cancel even if the
// notification has not yet arrived.
func (s *Session) Send(ctx context.Context, content string, opts provider.SendOptions) error {
	input, err := buildTurnInput(content, opts.Attachments)
	if err != nil {
		return err
	}

	cfg := s.snapshotTurnConfig()
	params := map[string]any{
		"threadId":          s.rootThreadID(),
		"input":             input,
		"collaborationMode": codexCollaborationMode(opts.InteractionMode, cfg.Model, cfg.ReasoningEffort),
	}
	if len(opts.OutputSchema) > 0 {
		params["outputSchema"] = opts.OutputSchema
	}
	// The correlation handle. Omitted rather than sent empty when the caller
	// has none: upstream types it `Option<String>` and mints its own id for an
	// absent one, so an explicit empty string would be a value the echo could
	// never match rather than an absence.
	if clientID := strings.TrimSpace(opts.ClientUserMessageID); clientID != "" {
		params["clientUserMessageId"] = clientID
	}
	// Per-turn config overrides — Codex's TurnStartParams takes `model`,
	// `effort`, `serviceTier`, `approvalPolicy`, and `sandboxPolicy` at the
	// top level, each documented upstream as applying "for this turn and
	// subsequent turns". Threading them here (rather than only at
	// thread-start) means a mid-session change from the composer
	// (ApplyLiveUpdate) takes effect on the very next turn without a
	// session restart. Empty means "inherit the thread default set during
	// thread/start".
	if cfg.Model != "" {
		params["model"] = cfg.Model
	}
	if cfg.ReasoningEffort != "" {
		params["effort"] = cfg.ReasoningEffort
	}
	// `serviceTier` is a double option upstream: omitting it means "leave the
	// thread's tier alone", so switching fast mode OFF has to send an
	// explicit null or the tier the previous ON asserted stays in force for
	// the rest of the session. planServiceTierWrite decides which of the
	// three cases (assert / clear / say nothing) this turn is in.
	tierWrite := s.planServiceTierWrite()
	if tierWrite.include {
		params["serviceTier"] = tierWrite.value
	}
	if cfg.ApprovalPolicy != "" {
		// Remapped per connected version for the same reason thread/start is:
		// a mid-session runtime-mode switch rewrites this axis
		// (live_update.go) and must land on the value THIS app-server
		// actually implements. See approvalPolicyForCodexVersion.
		params["approvalPolicy"] = approvalPolicyForCodexVersion(cfg.ApprovalPolicy, s.AppServerVersion())
	}
	// Always sent, for the same reason buildThreadParams always sends it: the
	// reviewer is thread state that persists until something overwrites it, so
	// a turn that omits it inherits whatever the last runtime mode selected.
	// `TurnStartParams.approvals_reviewer` is documented upstream as applying
	// "for this turn and subsequent turns", which is what makes an auto ↔
	// other-tier switch a live update rather than a restart.
	if cfg.ApprovalsReviewer != "" {
		params["approvalsReviewer"] = cfg.ApprovalsReviewer
	}
	if cfg.Sandbox != "" {
		sandboxPolicy, err := turnSandboxPolicy(cfg.Sandbox)
		if err != nil {
			return err
		}
		params["sandboxPolicy"] = sandboxPolicy
	}

	s.setPendingTurnSchema(len(opts.OutputSchema) > 0)
	// Claim BEFORE the write: `turn/started` can reach the read loop before
	// this request's response does, and an unclaimed observation is what
	// external_turns.go reads as "somebody else started this turn".
	s.beginLocalTurnStart()
	resp, err := s.sendRequest(ctx, "turn/start", params)
	if err != nil {
		s.clearPendingTurnSchema()
		if IsAmbiguousTurnStartTimeout(err) {
			// A timeout leaves the turn's existence unknown, so the claim
			// stays outstanding to cover a turn/started that still arrives —
			// but it is now the ONE claim that may describe nothing at all,
			// and it is released as soon as something proves that.
			s.noteAmbiguousLocalTurnStart()
		} else {
			s.abandonLocalTurnStart()
		}
		return fmt.Errorf("codex: turn/start: %w", err)
	}
	s.commitServiceTierWrite(tierWrite)

	turnID := readNestedString(resp, "turn", "id")
	s.bindLocalTurnStart(turnID)
	if turnID != "" {
		s.bindPendingTurnSchema(turnID)
		s.mu.Lock()
		s.turn.activeTurnID = turnID
		s.mu.Unlock()
	}

	return nil
}

func (s *Session) setPendingTurnSchema(schemaed bool) {
	s.mu.Lock()
	s.turn.pendingTurnSchemaKnown = true
	s.turn.pendingTurnSchemaed = schemaed
	s.mu.Unlock()
}

func (s *Session) clearPendingTurnSchema() {
	s.mu.Lock()
	s.turn.pendingTurnSchemaKnown = false
	s.turn.pendingTurnSchemaed = false
	s.mu.Unlock()
}

func (s *Session) bindPendingTurnSchema(turnID string) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.turn.pendingTurnSchemaKnown {
		return
	}
	if s.turn.pendingTurnSchemaed {
		if s.turn.schemaedTurnIDs == nil {
			s.turn.schemaedTurnIDs = make(map[string]struct{})
		}
		s.turn.schemaedTurnIDs[turnID] = struct{}{}
	} else {
		delete(s.turn.schemaedTurnIDs, turnID)
	}
	s.turn.pendingTurnSchemaKnown = false
	s.turn.pendingTurnSchemaed = false
}

// Steer injects user input into the currently-active turn's
// pending_input queue via Codex's `turn/steer` JSON-RPC. Mid-turn
// injection lets the user "steer" the model without spawning a new
// turn — Codex drains pending_input on the next iteration of its
// run_turn loop, and the app-server confirms the inject by emitting
// a wire-typed `item/completed userMessage` inside the same active
// turn (which our triage handleUserText path correlates with the
// pending-send marker).
//
// REQUIRES an active turn — returns ErrNoActiveTurn if no turn is
// currently in flight, so the caller can fall back to Send rather
// than racing the wire. `expectedTurnId` is upstream's own precondition
// and is REQUIRED to be non-empty (turn_processor.rs refuses an empty
// one with a plain invalid_request before it reaches the session), so
// the guard above is what keeps AO from writing a request that cannot
// succeed.
//
// DOES NOT take effort / approvalPolicy / sandboxPolicy /
// collaborationMode — those are turn-creation params for turn/start,
// not steer. Steer's contract is "inject input into an existing
// turn's input queue"; per-turn settings are fixed at the turn's
// creation.
//
// Wire shape per
// codex-rs/app-server-protocol/src/protocol/v2/turn.rs (TurnSteerParams:
// `{threadId, clientUserMessageId?, input, expectedTurnId}`). Server-side
// reference: codex-rs/app-server/src/request_processors/turn_processor.rs
// and codex-rs/core/src/session/mod.rs — see classifySteerRejection for
// the three refusals it can answer with.
func (s *Session) Steer(ctx context.Context, content string, opts provider.SendOptions) error {
	// Closed-session check MUST precede the activeTurnID read: Close zeroes
	// s.turn, so without it a post-Close Steer would answer ErrNoActiveTurn
	// and send the caller into the live-race retry (see ErrSessionClosed).
	if s.closing.Load() {
		return fmt.Errorf("codex: turn/steer: %w", ErrSessionClosed)
	}
	s.mu.Lock()
	expectedTurnID := s.turn.activeTurnID
	s.mu.Unlock()
	if expectedTurnID == "" {
		return ErrNoActiveTurn
	}

	input, err := buildTurnInput(content, opts.Attachments)
	if err != nil {
		return fmt.Errorf("codex: turn/steer: %w", err)
	}

	params := map[string]any{
		"threadId":       s.rootThreadID(),
		"input":          input,
		"expectedTurnId": expectedTurnID,
	}
	if clientID := strings.TrimSpace(opts.ClientUserMessageID); clientID != "" {
		params["clientUserMessageId"] = clientID
	}

	if _, err := s.sendRequest(ctx, "turn/steer", params); err != nil {
		return fmt.Errorf("codex: turn/steer: %w", classifySteerRejection(err))
	}
	return nil
}

// classifySteerRejection turns the app-server's `turn/steer` refusals into the
// two answers a caller can act on. All three arrive as the SAME JSON-RPC code
// (-32600 invalid_request, `invalid_request(message)` in
// request_processors/turn_processor.rs), so the code alone says nothing and the
// discrimination has to come from the payload.
//
// Two of them mean "the turn you addressed is not the one running":
//
//   - SteerInputError::NoActiveTurn → "no active turn to steer";
//   - SteerInputError::ExpectedTurnMismatch → "expected active turn id `X` but
//     found `Y`".
//
// Both map onto ErrNoActiveTurn, which is what the app layer already falls
// back on (IsNoActiveTurnRace → re-dispatch as a fresh turn). The mismatch
// message NAMES the turn id upstream found, and AO deliberately does not parse
// it out for a retry: by the time the answer is read that id can already have
// rolled again, and a steer aimed at a turn the user's message was not written
// for is worse than opening a turn of its own.
//
// The third is a different state entirely: SteerInputError::ActiveTurnNotSteerable
// means a turn IS running and simply cannot take input — a review or a
// compaction. Retrying as a fresh turn/start would be wrong too (it would
// interleave the user's message with a running review), so it gets its own
// sentinel. It is the one refusal upstream attaches structured data to:
// `error.data` carries a TurnError whose `codexErrorInfo` is
// `{"activeTurnNotSteerable":{"turnKind":…}}`, so this reads the typed field
// rather than the two English sentences that describe it.
func classifySteerRejection(err error) error {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return err
	}
	if steerDataReportsNotSteerable(rpcErr.Data) {
		return fmt.Errorf("%s: %w", rpcErr.Message, ErrTurnNotSteerable)
	}
	if isSteerTurnPreconditionMessage(rpcErr.Message) {
		return fmt.Errorf("%s: %w", rpcErr.Message, ErrNoActiveTurn)
	}
	return err
}

// steerNoActiveTurnMessage / steerExpectedTurnMismatchPrefix are upstream's own
// strings for the two precondition refusals
// (request_processors/turn_processor.rs, the `SteerInputError` match). Matched
// as text because the JSON-RPC code is shared with every other invalid request
// and neither refusal carries a `codexErrorInfo` — same posture as
// IsThreadNotFound and the writer-conflict markers next door.
const (
	steerNoActiveTurnMessage       = "no active turn to steer"
	steerExpectedTurnMismatchStart = "expected active turn id "
)

// isSteerTurnPreconditionMessage matches on CONTAINS rather than equality
// because the same test runs over a raw wire message and over an error this
// package has already wrapped with its `codex: turn/steer: ` prefix.
func isSteerTurnPreconditionMessage(message string) bool {
	return strings.Contains(message, steerNoActiveTurnMessage) ||
		strings.Contains(message, steerExpectedTurnMismatchStart)
}

// steerDataReportsNotSteerable reads the `activeTurnNotSteerable` discriminant
// out of a refusal's `error.data`.
//
// The shape is a serialized TurnError: `{message, codexErrorInfo,
// additionalDetails}` where `codexErrorInfo` is the externally tagged
// `{"activeTurnNotSteerable":{"turnKind":"review"|"compact"}}`
// (app-server-protocol/src/protocol/v2/shared.rs). codexErrorInfoKind already
// owns the "string variant or single-key object" decoding for that enum, so
// this only has to navigate to the field.
func steerDataReportsNotSteerable(data json.RawMessage) bool {
	if len(data) == 0 {
		return false
	}
	var body struct {
		Info json.RawMessage `json:"codexErrorInfo"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return false
	}
	return codexErrorInfoKind(body.Info) == "activeTurnNotSteerable"
}

// Interrupt sends turn/interrupt to abort whatever the thread is
// currently doing. We pass `turnId: activeTurnID` when a turn is in
// flight and `turnId: ""` (the empty string) when the user pressed
// stop during the dispatch window before `turn/started` arrived —
// the upstream app-server treats an empty turn_id as a "startup
// interrupt" and submits Op::Interrupt to the core anyway, then
// responds immediately with `{}` because startup cancellation has
// no TurnAborted event to wait on. See
// codex-rs/app-server/src/codex_message_processor.rs:7790-7849
// (`is_startup_interrupt = turn_id.is_empty()`) and the README
// summary at codex-rs/app-server/README.md:167.
//
// On success, drains pending approvals and user-input requests so
// the frontend clears its prompt panel and Codex's pending JSON-RPC
// requests resolve. Drain on failure too: the user pressed stop and
// expects the prompt to disappear even if the JSON-RPC errored.
// This is a deliberate fix beyond t3-code's
// CodexSessionRuntime.interruptTurn (CodexSessionRuntime.ts:1238–1250),
// which only sends the JSON-RPC and leaks the local Deferreds.
func (s *Session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	turnID := s.turn.activeTurnID
	s.mu.Unlock()

	_, err := s.sendRequest(ctx, "turn/interrupt", map[string]any{
		"threadId": s.rootThreadID(),
		"turnId":   turnID,
	})

	s.drainPendingApprovals("cancel", false, true)
	return err
}

// claimTurnStart records the first observation of a turnID, returning
// true. A second observation returns false so dispatchLine can skip the
// duplicate EventTurnStart. The map is bounded by the number of live
// turns — cleared on EventTurnComplete or session Close.
func (s *Session) claimTurnStart(turnID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn.seenTurnStarts == nil {
		s.turn.seenTurnStarts = make(map[string]struct{})
	}
	if _, ok := s.turn.seenTurnStarts[turnID]; ok {
		return false
	}
	s.turn.seenTurnStarts[turnID] = struct{}{}
	return true
}

// clearTurnStart drops the recorded turnID on completion so a follow-up
// turn with the same ID (rare, but possible across resumed sessions)
// can fire fresh.
func (s *Session) clearTurnStart(turnID string) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	delete(s.turn.seenTurnStarts, turnID)
	delete(s.origins.byTurn, turnID)
	// A turn just ENDED, and upstream runs at most one turn at a time on a
	// thread: any turn a timed-out `turn/start` created would have had to
	// start (and consume its claim) before this one could finish. So a claim
	// still held for that ambiguity describes nothing and is released here.
	s.dropAmbiguousLocalTurnStartsLocked("a turn completed")
	s.mu.Unlock()
}
