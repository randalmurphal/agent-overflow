package claude

import (
	"context"
	"errors"
	"fmt"
	"reflect"

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
//     and the restart is what performs the repair.
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
// the [1m] context marker), permission mode (set_permission_mode), effort
// (/effort), and fast mode (/fast). Still spawn-time-only:
//
//   - the read-only tool removal (`--disallowedTools`; no control_request
//     can add or drop a tool mid-session),
//   - autocompact settings and, when an autocompact override is active, the
//     context window (both ride the spawn-only `--settings` env block —
//     CLAUDE_CODE_AUTO_COMPACT_WINDOW must match the live window),
//   - the system prompt, output schema, and workdir.
//
// An effort transition where either side is empty (a model that declares no
// reasoning effort) is NOT live-appliable: there is no /effort argument that
// restores "send no effort at all".
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
// earlier axes may be applied, later ones are not. The caller must undo
// whatever preSend registered and fall back to the restart path, which
// re-grounds the session from spawn config.
func (s *Session) ApplyLiveUpdate(ctx context.Context, update LiveUpdate, preSend func(LiveApplyReceipt)) error {
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
	// The command axes are user messages on the wire. A transcript that
	// needs the --resume-session-at repair before any user send (an
	// unresolved server-side tool_use after a completed turn) must not
	// receive one — it would attach at the wrong parent. The restart
	// fallback performs exactly that repair.
	if (update.Effort != "" || update.FastMode != FastModeUnchanged) && s.RequiresResumeAtBeforeUserSend() {
		return ErrLiveUpdateRequiresRestart
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

	if update.Model != "" {
		if err := s.setModel(ctx, update.Model); err != nil {
			return err
		}
	}
	if update.BasePermissionMode != "" {
		if err := s.setBasePermissionMode(ctx, update.BasePermissionMode); err != nil {
			return err
		}
	}
	if update.Effort != "" {
		if err := s.sendConfigCommand(ctx, "/effort "+update.Effort, receipt.EffortCommandUUID); err != nil {
			return err
		}
	}
	// Fast mode last: /fast on can implicitly switch the model when the
	// current one lacks fast support, so the set_model above must already
	// have landed. (AO's option coercion only requests fast on
	// fast-capable models, making that CLI behavior unreachable — the
	// ordering keeps it that way by construction.)
	if update.FastMode != FastModeUnchanged {
		if err := s.sendConfigCommand(ctx, "/fast "+string(update.FastMode), receipt.FastCommandUUID); err != nil {
			return err
		}
	}
	return nil
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
	})
	if err != nil {
		return fmt.Errorf("claude: send %q: %w", command, err)
	}
	return nil
}

// setModel switches the live session's active model via the CLI's set_model
// control_request.
func (s *Session) setModel(ctx context.Context, model string) error {
	opName := "set model " + model
	res, err := s.sendControlRequest(ctx, opName, map[string]any{
		"subtype": "set_model",
		"model":   model,
	})
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
