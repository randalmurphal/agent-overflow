package claude

import (
	"context"
	"errors"
	"reflect"

	"agent-overflow/internal/provider"
)

// ErrLiveUpdateRequiresRestart is returned by ApplyLiveUpdate when the
// requested change is expressible on the wire but this particular process
// cannot accept it — currently only escalation to bypassPermissions on a
// session spawned without --allow-dangerously-skip-permissions (the CLI
// rejects the set_permission_mode request; verified on claude 2.1.205).
// Callers fall back to the restart path.
var ErrLiveUpdateRequiresRestart = errors.New("claude: live update requires a session restart")

// LiveUpdate describes the control-request applications that morph a running
// Claude session from one launch config to another without a restart. Zero
// values mean "no change for this axis".
type LiveUpdate struct {
	// Model is the wire model string (including any context-window suffix
	// such as "[1m]") to apply via a set_model control_request.
	Model string
	// BasePermissionMode is the permission mode ("default", "acceptEdits",
	// "bypassPermissions") to adopt as the session's base mode via a
	// set_permission_mode control_request.
	BasePermissionMode string
}

// Empty reports whether the update carries no changes.
func (u LiveUpdate) Empty() bool {
	return u == LiveUpdate{}
}

// PlanLiveUpdate diffs two option bundles and reports whether the delta can
// be applied to a live session over the stdio control channel. ok is false
// when any axis outside the live-appliable set (model, permission mode)
// differs — reasoning effort, fast mode, context window, autocompact
// settings, system prompt, and workdir are all spawn-time-only on the
// Claude CLI (verified against 2.1.205: no set_effort / set_fast_mode /
// set_context_window control subtypes exist) and require a restart.
//
// Resume, ResumeAt, and ForkSession are session lifecycle state, not
// config — a live session's resume cursor legitimately drifts from the
// thread row — so they are excluded from the comparison.
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

// ApplyLiveUpdate applies a planned update to the running session. The
// permission-mode capability is validated before any side effect so a
// rejected update never half-applies. A model change takes effect on the
// next turn: the CLI acks set_model immediately (even mid-turn) but the
// in-flight turn finishes on the previous model (verified on 2.1.205).
func (s *Session) ApplyLiveUpdate(ctx context.Context, update LiveUpdate) error {
	if update.BasePermissionMode == "bypassPermissions" && !s.allowsBypassPermissions {
		return ErrLiveUpdateRequiresRestart
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
