package claude

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/google/uuid"

	"agent-overflow/internal/provider"
)

// ErrLiveUpdateRequiresRestart is returned by ApplyLiveUpdate when the
// requested change is expressible on the wire but this particular process
// cannot accept it:
//
//   - escalation to bypassPermissions on a session spawned without
//     --allow-dangerously-skip-permissions (the CLI rejects the
//     set_permission_mode request; verified on claude 2.1.205);
//   - enabling fast mode on a session spawned without the
//     `--settings '{"fastMode":true}'` SDK opt-in (the CLI answers /fast
//     with "not available in the Agent SDK"; verified on 2.1.219) — the
//     respawn is what adds the opt-in;
//   - an /effort or /fast apply on a session that has not advertised that
//     command (older CLI, gated account, or no init yet);
//   - an /effort or /fast apply while the session's transcript needs the
//     --resume-session-at repair before any user message may be written
//     (RequiresResumeAtBeforeUserSend) — the commands ARE user messages,
//     and the restart is what performs the repair;
//   - a system-prompt swap on a session whose CLI has not (yet) proved it
//     is new enough to APPLY set_model.system_prompt, or whose current
//     model is unknown (set_model must carry one, and there is no
//     prompt-only request shape);
//   - a thinking change on a session whose CLI has not (yet) proved it
//     carries the set_max_thinking_tokens handler.
//
// Callers fall back to the restart path.
var ErrLiveUpdateRequiresRestart = errors.New("claude: live update requires a session restart")

// FastModeChange is the fast-mode axis of a LiveUpdate. String-typed rather
// than *bool so LiveUpdate stays comparable and the value doubles as the
// /fast command argument.
type FastModeChange string

const (
	FastModeUnchanged FastModeChange = ""
	FastModeOn        FastModeChange = "on"
	FastModeOff       FastModeChange = "off"
)

// ThinkingUpdate is the extended-thinking axis of a LiveUpdate: one
// `set_max_thinking_tokens` control_request, described by which keys it
// carries rather than by a target state.
//
// It is shaped that way because the CLI's request has no total ordering
// onto states. `max_thinking_tokens` may be sent (0 disables, N pins a
// budget) or OMITTED — and omitted is the only way to say "leave the budget
// alone", since `null` is accepted and does nothing. So SendBudget is not a
// nil-pointer workaround; it is the third value the wire actually has, and
// keeping it a bool leaves LiveUpdate comparable for Empty().
//
// Zero value = this update touches nothing.
type ThinkingUpdate struct {
	// Apply is false when the thinking axis carries no change at all.
	// Every other field is meaningless without it — a display-only update
	// legitimately has SendBudget false and Budget 0.
	Apply bool
	// SendBudget decides whether `max_thinking_tokens` appears in the
	// request. False means the key is omitted (display-only change).
	SendBudget bool
	// Budget is the `max_thinking_tokens` value when SendBudget is true:
	// 0 disables thinking, a positive number pins a fixed budget.
	Budget int
	// Display is the `thinking_display` value ("summarized" | "omitted"),
	// or "" to omit the key. Omitted for a disabling update, because the
	// CLI drops display on a disabled session.
	Display string
}

// LiveUpdate describes the applications that morph a running Claude session
// from one launch config to another without a restart. Zero values mean "no
// change for this axis". Two wire channels are involved: set_model /
// set_permission_mode are control_requests the CLI acks immediately, while
// Effort and FastMode ride the CLI's provider-executed `/effort` and `/fast`
// slash commands (spike-verified 2.1.219: both are `supportsNonInteractive`,
// session-only, zero-cost, and the change is on the next API request —
// captured via a local ANTHROPIC_BASE_URL sink; see claude-wire.md §"Live
// config commands").
type LiveUpdate struct {
	// Model is the wire model string (including any context-window suffix
	// such as "[1m]" — set_model accepts marker-carrying ids and the
	// context-1m beta follows on the next request, verified 2.1.219) to
	// apply via a set_model control_request.
	Model string
	// BasePermissionMode is the permission mode ("default", "acceptEdits",
	// "bypassPermissions") to adopt as the session's base mode via a
	// set_permission_mode control_request.
	BasePermissionMode string
	// Effort is the wire effort tier ("low"…"max") to apply via the /effort
	// command. Applies from the next turn, survives set_model, and never
	// persists outside the session ("this session only").
	Effort string
	// FastMode is the fast-mode state to apply via the /fast command.
	FastMode FastModeChange
	// SystemPrompt is the replacement custom system prompt to apply via
	// `set_model.system_prompt`, effective from the next turn on. Always
	// non-empty: the field has no revert-to-built-in form (the CLI rejects
	// an empty string outright, "system_prompt must be a non-empty string
	// when present"), so an override being turned OFF is a restart. Turning
	// one ON is live — the handler's setter is an unguarded assignment onto
	// the same slot `--system-prompt-file` fills, so populating an empty
	// slot is not a special case. See PlanLiveUpdate.
	//
	// It rides set_model rather than a request of its own, which makes the
	// model the carrier: an update with a prompt and no model change
	// re-sends the model the session is already running.
	SystemPrompt string
	// Thinking is the extended-thinking change to apply via one
	// `set_max_thinking_tokens` control_request. Like the prompt axis it is
	// ASYMMETRIC: turning thinking off or pinning a budget is live, and the
	// return to "whatever Claude Code decides" is not — `max_thinking_tokens:
	// null` is a documented no-op, so only a respawn without the flag
	// restores the CLI's own choice. PlanLiveUpdate refuses that direction.
	Thinking ThinkingUpdate
}

// Empty reports whether the update carries no changes.
func (u LiveUpdate) Empty() bool {
	return u == LiveUpdate{}
}

// liveApplyEffortTiers is the /effort argument vocabulary AO is willing to
// send: exactly the tiers claudeEffortFromOption can produce. The CLI's own
// set is wider ("ultracode", "auto") but those are not thread-config values,
// and an unknown argument is answered with a non-error "Invalid argument"
// text rather than a wire failure — so the validation has to happen here.
// TestLiveEffortTiersMatchOptionCoercion pins this set to the option layer.
var liveApplyEffortTiers = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// IsLiveEffortTier reports whether tier is in the /effort vocabulary AO
// models as thread config. Shared with the app-side confirmation matcher
// (parseEffortSetText) so the send-side and read-side vocabularies cannot
// drift apart.
func IsLiveEffortTier(tier string) bool {
	_, ok := liveApplyEffortTiers[tier]
	return ok
}

// PlanLiveUpdate diffs two option bundles and reports whether the delta can
// be applied to a live session. ok is false when any axis outside the
// live-appliable set differs. Live-appliable: model (set_model, including
// the [1m] context marker), permission mode (set_permission_mode), thinking
// (set_max_thinking_tokens), effort (/effort), and fast mode (/fast). Still
// spawn-time-only:
//
//   - the read-only tool removal (`--disallowedTools`; no control_request
//     can add or drop a tool mid-session),
//   - autocompact settings and, when an autocompact override is active, the
//     context window (both ride the spawn-only `--settings` env block —
//     CLAUDE_CODE_AUTO_COMPACT_WINDOW must match the live window),
//   - the output schema and workdir.
//
// An effort transition where either side is empty (a model that declares no
// reasoning effort) is NOT live-appliable: there is no /effort argument that
// restores "send no effort at all". The system prompt has HALF that hole:
// `set_model.system_prompt` must be non-empty, so turning an override OFF is
// a restart — but its setter assigns unconditionally onto the slot
// `--system-prompt-file` fills, so turning one ON is live, exactly like
// swapping one for another.
//
// Resume, ResumeAt, and ForkSession are session lifecycle state, not
// config — a live session's resume cursor legitimately drifts from the
// thread row — so they are excluded from the comparison.
//
// The comparison basis is ConfigFromOptions(SessionOptions) — config axes
// the spawn layer sets on Config OUTSIDE that derivation (a workflow's
// OutputSchema, Env, MCP servers) are invisible here and are reconciled by
// their own mechanisms (or not at all); this function can only answer for
// what SessionOptions carries.
func PlanLiveUpdate(prev, next provider.SessionOptions) (LiveUpdate, bool) {
	prevCfg := ConfigFromOptions(prev)
	nextCfg := ConfigFromOptions(next)

	var update LiveUpdate
	if prevCfg.Model != nextCfg.Model {
		update.Model = nextCfg.Model
	}
	if prevCfg.BasePermissionMode != nextCfg.BasePermissionMode {
		update.BasePermissionMode = nextCfg.BasePermissionMode
	}
	if prevCfg.ReasoningEffort != nextCfg.ReasoningEffort &&
		prevCfg.ReasoningEffort != "" && nextCfg.ReasoningEffort != "" {
		update.Effort = nextCfg.ReasoningEffort
		// Carried by the update — take the axis out of the equality check.
		// An empty-sided transition is deliberately NOT blanked, so it
		// falls through to the DeepEqual below and demands a restart.
		prevCfg.ReasoningEffort, nextCfg.ReasoningEffort = "", ""
	}
	// The prompt swap rides update.Model's set_model (or a re-send of the
	// current model when the model itself is unchanged), so it is blanked
	// out of the equality check once carried. Unlike the effort axis this
	// is NOT symmetric: only the empty NEXT side falls through to the
	// DeepEqual below and demands a restart, because set_model's
	// system_prompt has no revert-to-built-in form. Turning an override ON
	// is carried live — the CLI's setter assigns unconditionally onto the
	// slot `--system-prompt-file` would otherwise have filled at spawn, so
	// an empty prior value is not a special case on the wire.
	if prevCfg.SystemPrompt != nextCfg.SystemPrompt && nextCfg.SystemPrompt != "" {
		update.SystemPrompt = nextCfg.SystemPrompt
		prevCfg.SystemPrompt, nextCfg.SystemPrompt = "", ""
	}
	// The thinking axis rides a request of its own. A refused transition
	// (the return to ThinkingDefault) is deliberately NOT blanked, so it
	// falls through to the DeepEqual below and demands a restart.
	if prevCfg.Thinking != nextCfg.Thinking {
		if plan, ok := planThinkingUpdate(prevCfg.Thinking, nextCfg.Thinking); ok {
			update.Thinking = plan
			prevCfg.Thinking, nextCfg.Thinking = ThinkingConfig{}, ThinkingConfig{}
		}
	}
	if prevCfg.FastMode != nextCfg.FastMode {
		if nextCfg.FastMode {
			update.FastMode = FastModeOn
		} else {
			update.FastMode = FastModeOff
		}
		prevCfg.FastMode, nextCfg.FastMode = false, false
	}
	// The context window's only spawn materialization besides the model
	// marker (already carried by update.Model) is the
	// CLAUDE_CODE_AUTO_COMPACT_WINDOW env var, rendered only when an
	// autocompact override is active. With no override on either side the
	// int is spawn-inert, so a 200k↔1m switch is just a set_model.
	if prevCfg.AutoCompactPercent == 0 && nextCfg.AutoCompactPercent == 0 {
		prevCfg.ContextWindow, nextCfg.ContextWindow = 0, 0
	}

	// Blank the live-appliable axes (PermissionFlags travel with
	// BasePermissionMode) plus lifecycle fields, then require everything
	// left — the spawn-only config — to be identical.
	//
	// DisallowedTools is deliberately NOT blanked: `--disallowedTools`
	// removes tools from the process's toolset at spawn and no control
	// request can put them back, so any transition into or out of the
	// read-only runtime mode falls through to the DeepEqual below and
	// demands a restart. Blanking it would let a read-only → auto-accept-edits
	// switch ack a bare set_permission_mode while Write/Edit stayed missing —
	// a session claiming a mode it cannot honour.
	prevCfg.Model, nextCfg.Model = "", ""
	prevCfg.BasePermissionMode, nextCfg.BasePermissionMode = "", ""
	prevCfg.PermissionFlags, nextCfg.PermissionFlags = nil, nil
	prevCfg.Resume, nextCfg.Resume = "", ""
	prevCfg.ResumeAt, nextCfg.ResumeAt = "", ""
	prevCfg.ForkSession, nextCfg.ForkSession = false, false
	if !reflect.DeepEqual(prevCfg, nextCfg) {
		return LiveUpdate{}, false
	}
	return update, true
}

// planThinkingUpdate decides how (or whether) a thinking transition reaches
// a live session. Callers reach it only for a real diff.
//
// The one refused direction is the return to ThinkingDefault from a state
// AO asserted. It has no wire form — `max_thinking_tokens: null` is
// accepted and does nothing, and there is no "unset" value for the
// display either — so the honest answer is the restart, where the spawn
// rebuilds argv without `--thinking` / `--max-thinking-tokens`. Planning it
// as live would leave the session running the old budget while launchOpts
// claimed the CLI's default, which is silent by construction: the ack would
// succeed and nothing on the wire would disagree.
//
// Display rides every non-disabling apply rather than only a display diff.
// It is normalized (never empty), re-asserting it is idempotent, and it
// closes the one gap a diff-only rule leaves: a session spawned
// `--thinking disabled` never applied a display at all, so enabling
// thinking live has to state one.
func planThinkingUpdate(prev, next ThinkingConfig) (ThinkingUpdate, bool) {
	switch next.Mode {
	case ThinkingOff:
		return ThinkingUpdate{Apply: true, SendBudget: true, Budget: 0}, true
	case ThinkingBudget:
		return ThinkingUpdate{
			Apply:      true,
			SendBudget: true,
			Budget:     next.BudgetTokens,
			Display:    normalizeThinkingDisplay(next.Display),
		}, true
	default:
		if prev.Mode != ThinkingDefault {
			return ThinkingUpdate{}, false
		}
		// Display-only change on a session AO never took the mode of.
		// `thinking_display` alone is an accepted request (2.1.237).
		return ThinkingUpdate{Apply: true, Display: normalizeThinkingDisplay(next.Display)}, true
	}
}

// LiveApplyReceipt reports the client-minted uuids of the slash-command
// sends an ApplyLiveUpdate will write, so the caller can match the
// commands' eventual output (EventCommandResult,
// CommandResultMeta.CommandUUID) against what was requested.
// Control-request applications (model, permission mode) are acked
// synchronously and need no receipt.
type LiveApplyReceipt struct {
	EffortCommandUUID string
	FastCommandUUID   string
}

// validateLiveUpdate answers ONE question for the whole update: may every
// axis it carries be applied to this live session, or must the caller fall
// back to a restart?
//
// It is separated from ApplyLiveUpdate so that "every axis is validated
// before ANY side effect" is readable off the SHAPE rather than off the
// ordering of a long function body — the property is load-bearing, because a
// restart-bound update that half-applied would leave the process running a
// config neither the row nor the caller believes in.
//
// Two error kinds, and the difference matters to the caller:
// ErrLiveUpdateRequiresRestart is a routine "not on this process" (an old
// CLI, a spawn-only flag, a transcript needing repair) that the reconciler
// answers with a deferred restart, while a described error is a MALFORMED
// update — a bug in the planner — that no restart would fix.
func (s *Session) validateLiveUpdate(update LiveUpdate) error {
	if update.BasePermissionMode == "bypassPermissions" && !s.allowsBypassPermissions {
		return ErrLiveUpdateRequiresRestart
	}
	if update.Effort != "" {
		if !IsLiveEffortTier(update.Effort) {
			return fmt.Errorf("claude: effort tier %q is not live-appliable", update.Effort)
		}
		if model := s.effectiveConfigModel(update); provider.ModelDeclaresNoReasoningEffort(string(provider.Claude), model) {
			return fmt.Errorf("claude: model %q declares no reasoning effort; /effort %s cannot apply", model, update.Effort)
		}
		if !s.supportsSlashCommand("effort") {
			return ErrLiveUpdateRequiresRestart
		}
	}
	if update.FastMode != FastModeUnchanged {
		if update.FastMode != FastModeOn && update.FastMode != FastModeOff {
			return fmt.Errorf("claude: fast mode change %q is not a valid /fast argument", update.FastMode)
		}
		if update.FastMode == FastModeOn && !s.spawnedWithFastModeOptIn {
			return ErrLiveUpdateRequiresRestart
		}
		if !s.supportsSlashCommand("fast") {
			return ErrLiveUpdateRequiresRestart
		}
	}
	if update.SystemPrompt != "" {
		if !s.supportsLiveSystemPrompt() {
			return ErrLiveUpdateRequiresRestart
		}
		// set_model has no prompt-only form: the request must name a model,
		// and a session whose model we cannot state (no Config.Model, no
		// acked set_model) has nothing to re-send. Restarting is what
		// grounds it.
		if s.effectiveConfigModel(update) == "" {
			return ErrLiveUpdateRequiresRestart
		}
	}
	if update.Thinking.Apply {
		if !s.supportsLiveThinking() {
			return ErrLiveUpdateRequiresRestart
		}
		if update.Thinking.SendBudget && update.Thinking.Budget < 0 {
			return fmt.Errorf("claude: thinking budget %d is not a valid max_thinking_tokens", update.Thinking.Budget)
		}
		switch update.Thinking.Display {
		case "", ThinkingDisplaySummarized, ThinkingDisplayOmitted:
		default:
			return fmt.Errorf("claude: thinking display %q is not a valid thinking_display", update.Thinking.Display)
		}
		if !update.Thinking.SendBudget && update.Thinking.Display == "" {
			// An apply that would send neither key: the CLI would accept
			// it and change nothing, and the caller would then commit
			// launchOpts as if something had happened.
			return fmt.Errorf("claude: thinking update carries neither a budget nor a display")
		}
	}
	// The command axes are user messages on the wire. A transcript that
	// needs the --resume-session-at repair before any user send (an
	// unresolved server-side tool_use after a completed turn) must not
	// receive one — it would attach at the wrong parent. The restart
	// fallback performs exactly that repair.
	if (update.Effort != "" || update.FastMode != FastModeUnchanged) && s.RequiresResumeAtBeforeUserSend() {
		return ErrLiveUpdateRequiresRestart
	}
	return nil
}

// ApplyLiveUpdate applies a planned update to the running session. Every
// axis is validated before any side effect so a rejected update never
// half-applies. Timing: the CLI acks set_model immediately (even mid-turn)
// but the in-flight turn finishes on the previous model; /effort and /fast
// written mid-turn are drained into the running turn like any stdin user
// message (claude-wire.md §command_lifecycle) — either way the new tier is
// on the next API request, never the in-flight one (spike-verified
// 2.1.219).
//
// preSend, when non-nil, is invoked exactly once — after every validation
// has passed and before ANY byte reaches the wire — with the receipt whose
// uuids the command sends will carry. This inversion is deliberate: the
// commands are confirmed asynchronously (their output arrives later as an
// EventCommandResult carrying the receipt uuid), so the caller must have
// its pending-confirmation state registered BEFORE the CLI can possibly
// answer. A receipt returned after the sends would race its own
// confirmation.
//
// An error return after preSend ran means a send failed mid-sequence:
// earlier axes ARE applied, later ones are not. The returned
// LiveApplyOutcome names which — the axes are separate wire operations
// (set_model, set_permission_mode, set_max_thinking_tokens, then two
// stdin commands), so a set_max_thinking_tokens that times out against a
// busy CLI leaves a session genuinely running the new prompt on the old
// thinking budget.
//
// Callers MUST fold the applied axes into whatever they record as the
// session's live config (CommitLiveUpdate) before falling back to the
// restart path. Recording the pre-apply options wholesale would claim the
// session is running a config it is not — the restart is deferred until
// the thread is quiet, which a busy thread can postpone indefinitely, and
// every turn in between runs under the hybrid. It would also make the
// live-first retry re-send axes that already landed instead of the one
// that failed.
//
// The alternative — escalating to an IMMEDIATE restart so no hybrid ever
// runs a turn — is deliberately not taken: a restart kills the in-flight
// turn and every live backgrounded task, which is the exact cost the
// deferred-restart policy exists to avoid (app_session_config.go). A
// hybrid config is a wrong setting; an immediate restart is lost work.
func (s *Session) ApplyLiveUpdate(ctx context.Context, update LiveUpdate, preSend func(LiveApplyReceipt)) (LiveApplyOutcome, error) {
	var applied LiveApplyOutcome
	if err := s.validateLiveUpdate(update); err != nil {
		return applied, err
	}

	var receipt LiveApplyReceipt
	if update.Effort != "" {
		receipt.EffortCommandUUID = uuid.NewString()
	}
	if update.FastMode != FastModeUnchanged {
		receipt.FastCommandUUID = uuid.NewString()
	}
	if preSend != nil {
		preSend(receipt)
	}

	// One set_model carries both the model and the prompt swap. When only
	// the prompt changed it re-sends the model EXACTLY as last accepted
	// (marker and all) — the CLI applies the prompt only when it accepts
	// the model, so a prompt-only update still has to win the model check.
	if update.Model != "" || update.SystemPrompt != "" {
		if err := s.setModel(ctx, s.effectiveConfigModel(update), update.SystemPrompt); err != nil {
			return applied, err
		}
		// One request, both axes: the CLI applies the prompt only when it
		// accepts the model, so there is no half of this to report.
		applied.Model = update.Model != ""
		applied.SystemPrompt = update.SystemPrompt != ""
	}
	if update.BasePermissionMode != "" {
		if err := s.setBasePermissionMode(ctx, update.BasePermissionMode); err != nil {
			return applied, err
		}
		applied.BasePermissionMode = true
	}
	// Thinking is a control_request like the two above and shares nothing
	// with them on the CLI side, so it sits with them — ahead of the
	// command axes, which are user messages and can drain into a running
	// turn.
	if update.Thinking.Apply {
		if err := s.setMaxThinkingTokens(ctx, update.Thinking); err != nil {
			return applied, err
		}
		applied.Thinking = true
	}
	if update.Effort != "" {
		if err := s.sendConfigCommand(ctx, "/effort "+update.Effort, receipt.EffortCommandUUID); err != nil {
			return applied, err
		}
		applied.Effort = true
		// Send-side intent, not a confirmation — the command answers
		// asynchronously. Its only reader is the get_settings read-back's
		// "is a project settings file overriding what AO asked for" check,
		// which wants exactly the requested value.
		s.configModelMu.Lock()
		s.requestedEffort = update.Effort
		s.configModelMu.Unlock()
	}
	// Fast mode last: /fast on can implicitly switch the model when the
	// current one lacks fast support, so the set_model above must already
	// have landed. (AO's option coercion only requests fast on
	// fast-capable models, making that CLI behavior unreachable — the
	// ordering keeps it that way by construction.)
	if update.FastMode != FastModeUnchanged {
		if err := s.sendConfigCommand(ctx, "/fast "+string(update.FastMode), receipt.FastCommandUUID); err != nil {
			return applied, err
		}
		applied.FastMode = true
	}
	return applied, nil
}

// LiveApplyOutcome names the axes of a LiveUpdate that actually reached the
// session. Every field is false on a validation refusal (nothing was sent);
// on a mid-sequence wire failure the fields ahead of the failure are true.
//
// "Applied" means the wire operation SUCCEEDED, which for the two command
// axes means the send did — their semantic verdict arrives later as an
// EventCommandResult and is the caller's own pending-apply machinery to
// settle. That asymmetry is why this reports axes rather than a state:
// a command that was written but answered "Invalid argument" is applied
// here and rejected there, and both are true statements about it.
type LiveApplyOutcome struct {
	Model              bool
	SystemPrompt       bool
	BasePermissionMode bool
	Thinking           bool
	Effort             bool
	FastMode           bool
}

// CommitLiveUpdate folds the axes that landed into the options a live
// session is recorded as running: prev with each applied axis taken from
// next. A fully applied update yields options a re-plan sees as converged;
// a partial one yields exactly the failed axes as the remaining diff, which
// is what makes the live-first retry re-send only those.
//
// The per-axis field lists are the inverse of ConfigFromOptions and are
// pinned by TestCommitLiveUpdateCoversEveryLiveAppliableAxis — a new live
// axis that forgets its entry here shows up as a diff that never converges.
func CommitLiveUpdate(prev, next provider.SessionOptions, applied LiveApplyOutcome) provider.SessionOptions {
	committed := prev
	if applied.Model {
		// The context window rides the model: it picks the "[1m]" marker
		// and the autocompact tier. PlanLiveUpdate only lets a window
		// change through as a bare set_model when no autocompact override
		// is active on either side, so there is no spawn-only residue to
		// leave behind here.
		committed.Model = next.Model
		committed.ContextWindow = next.ContextWindow
	}
	if applied.SystemPrompt {
		committed.SystemPrompt = next.SystemPrompt
		committed.SystemPromptOverrideSource = next.SystemPromptOverrideSource
	}
	if applied.BasePermissionMode {
		// The runtime mode is the single option behind the permission mode,
		// the permission flags, and the read-only tool strip. The strip is
		// spawn-only and NOT blanked by PlanLiveUpdate, so any transition
		// that would move it demanded a restart and never reached here.
		committed.RuntimeMode = next.RuntimeMode
	}
	if applied.Thinking {
		committed.ClaudeThinking = next.ClaudeThinking
	}
	if applied.Effort {
		committed.ReasoningEffort = next.ReasoningEffort
	}
	if applied.FastMode {
		committed.FastMode = next.FastMode
	}
	return committed
}

// effectiveConfigModel is the model the session will be running once the
// update lands: the update's own model when it carries one, the session's
// current config model otherwise.
func (s *Session) effectiveConfigModel(update LiveUpdate) string {
	if update.Model != "" {
		return update.Model
	}
	s.configModelMu.Lock()
	defer s.configModelMu.Unlock()
	return s.configModel
}

// sendConfigCommand writes one provider-executed slash command to the
// session's stdin under the pre-minted uuid the CLI will echo on the
// command's lifecycle acks and (via the parser's correlation) on the
// command output's CommandResultMeta.
func (s *Session) sendConfigCommand(ctx context.Context, command, id string) error {
	err := s.Send(ctx, command, provider.SendOptions{
		UserMessageUUID:         id,
		AllowClaudeSlashCommand: true,
		// AO issued this, not the user. The flag is what buys unconditional
		// row suppression: the CLI's refusals here are surfaced by the
		// live-config reconciler, and a text-matched suppression would put
		// AO's own bookkeeping back into the transcript the first time the
		// CLI's copy moved (command_result_suppression.go).
		InternalCommand: true,
	})
	if err != nil {
		return fmt.Errorf("claude: send %q: %w", command, err)
	}
	return nil
}

// setModel switches the live session's active model via the CLI's set_model
// control_request, optionally replacing the custom system prompt in the same
// request.
//
// The two are ONE decision on the CLI side: the prompt is stored only after
// the model passes the recognized / allowed checks, and a rejected model
// answers `{ok:false, error}` with the prompt untouched (2.1.237 handler,
// telemetry `system_prompt_switch: "model_switch_rejected"`). So an error
// return here means NEITHER axis applied — never "the model failed but the
// prompt landed" — and the caller's restart fallback re-grounds both.
//
// The success payload is a bare `{}`: there is no schema for a set_model
// success body and the handler returns `{ok:true}` with nothing else, so the
// applied model is NOT readable from this round-trip. When the CLI steps a
// family alias down to a concrete model it still answers ok (telemetry
// `model_switch: "family_alias_stepped_down"`), which is why the structured
// read-back (get_settings.go) is what records what is actually running.
func (s *Session) setModel(ctx context.Context, model, systemPrompt string) error {
	opName := "set model " + model
	request := map[string]any{
		"subtype": "set_model",
		"model":   model,
	}
	if systemPrompt != "" {
		opName += " with system prompt"
		request["system_prompt"] = systemPrompt
	}
	res, err := s.sendControlRequest(ctx, opName, request)
	if err != nil {
		return err
	}
	// The parser's model (used to attribute usage) is not touched here: it
	// is read-loop-goroutine state, and the CLI re-emits system/init with
	// the new model at the next turn boundary, which updates it on the
	// correct goroutine — the same boundary at which the change takes
	// effect.
	if err := interpretControlResponse(res, opName); err != nil {
		return err
	}
	s.configModelMu.Lock()
	s.configModel = model
	s.configModelMu.Unlock()
	return nil
}

// setMaxThinkingTokens applies the thinking axis to the live session via the
// CLI's `set_max_thinking_tokens` control_request.
//
// The request has two independent optional keys and the OMISSIONS carry
// meaning, so this builds the map key by key rather than always sending both
// (spike-verified against 2.1.237 with a local API sink):
//
//   - `max_thinking_tokens: 0` disables thinking outright, and the CLI then
//     drops display entirely — so a disabling request carries no
//     `thinking_display`.
//   - `max_thinking_tokens: N` sets a fixed budget. It applies only on
//     models that take an explicit budget (sonnet-4-5 moved 31999 → 2048);
//     on an adaptive-thinking model the budget is ignored and only display
//     lands.
//   - `max_thinking_tokens: null` is ACCEPTED AND DOES NOTHING. There is no
//     reset-to-default form, which is why ThinkingUpdate.SendBudget exists:
//     "leave the budget alone" has to be expressed by OMITTING the key, and
//     the return to the CLI's own choice is a restart (PlanLiveUpdate).
//   - `thinking_display` alone, with no budget key at all, is accepted and
//     applies.
//
// The handler validates both keys before acting and answers a bare
// `{subtype:"success"}`, so — exactly like set_model — the ack proves the
// request was accepted, never what the model ended up running. The next
// turn's API request is where the change is observable.
func (s *Session) setMaxThinkingTokens(ctx context.Context, update ThinkingUpdate) error {
	request := map[string]any{"subtype": "set_max_thinking_tokens"}
	opName := "set thinking"
	if update.SendBudget {
		request["max_thinking_tokens"] = update.Budget
		if update.Budget == 0 {
			opName = "disable thinking"
		} else {
			opName = "set thinking budget " + strconv.Itoa(update.Budget)
		}
	}
	if update.Display != "" {
		request["thinking_display"] = update.Display
		opName += " (display " + update.Display + ")"
	}
	res, err := s.sendControlRequest(ctx, opName, request)
	if err != nil {
		return err
	}
	return interpretControlResponse(res, opName)
}

// setBasePermissionMode adopts a new base permission mode (the mode restored
// outside plan turns) and immediately applies the effective mode for the
// session's current interaction mode. A session mid-plan-turn keeps "plan"
// until the next Send restores the base, matching SetInteractionMode.
func (s *Session) setBasePermissionMode(ctx context.Context, mode string) error {
	mode = normalizeClaudePermissionMode(mode)
	s.permissionModeMu.Lock()
	s.basePermissionMode = mode
	s.permissionModeMu.Unlock()
	return s.setPermissionMode(ctx, s.desiredPermissionModeForTurn(s.interactionMode))
}
