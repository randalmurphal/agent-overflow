package codex

import (
	"encoding/json"
	"strings"
)

// ThreadSettings is Codex's authoritative view of a thread's live
// configuration, decoded from the `thread/settings/updated` notification
// (`#[experimental("thread/settings/updated")]`, so it only arrives
// because every AO handshake sets `capabilities.experimentalApi`).
//
// This is deliberately NOT the same thing as the session's turn config
// (`LiveUpdate`). Those two answer different questions and must not be
// collapsed:
//
//   - LiveUpdate / s.model, s.serviceTier, … = what AO will ASK FOR on
//     the next turn/start. The app layer owns it through ApplyLiveUpdate.
//   - ThreadSettings = what Codex IS running right now. Codex owns it,
//     and it can change without AO asking (model reroute, guardian
//     downgrade, a config reload, another client on the same thread).
//
// Merging them would let a settings echo from turn N silently overwrite a
// model the user picked for turn N+1. Keeping them apart is Core Principle
// 2 applied to config: the provider process is the source of truth for what
// happened, the app is the source of truth for what to do next.
//
// Sandbox is normalized into AO's vocabulary (`read-only` /
// `workspace-write` / `danger-full-access`) so callers never have to know
// Codex spells it `workspaceWrite`. An unrecognized wire value leaves the
// field empty rather than guessing a tier — silently reporting a sandbox
// that isn't the one enforced would be worse than reporting none.
type ThreadSettings struct {
	Cwd               string `json:"cwd,omitempty"`
	Model             string `json:"model,omitempty"`
	ModelProvider     string `json:"modelProvider,omitempty"`
	ReasoningEffort   string `json:"reasoningEffort,omitempty"`
	ServiceTier       string `json:"serviceTier,omitempty"`
	ApprovalPolicy    string `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer string `json:"approvalsReviewer,omitempty"`
	Sandbox           string `json:"sandbox,omitempty"`
}

// threadSettingsNotification is the wire shape of
// `thread/settings/updated`. Field set verified against a live capture on
// codex-cli 0.146.0 (scratchpad spike-codex, captures2/notifications-exp)
// and typed upstream as v2::ThreadSettingsUpdatedNotification /
// v2::ThreadSettings.
type threadSettingsNotification struct {
	ThreadID       string `json:"threadId"`
	ThreadSettings struct {
		Cwd               string          `json:"cwd"`
		Model             string          `json:"model"`
		ModelProvider     string          `json:"modelProvider"`
		Effort            *string         `json:"effort"`
		ServiceTier       *string         `json:"serviceTier"`
		ApprovalPolicy    string          `json:"approvalPolicy"`
		ApprovalsReviewer string          `json:"approvalsReviewer"`
		SandboxPolicy     json.RawMessage `json:"sandboxPolicy"`
	} `json:"threadSettings"`
}

// sandboxFromWirePolicy inverts turnSandboxPolicy: Codex's camelCase
// SandboxPolicy tag back into AO's hyphenated sandbox vocabulary. Unknown
// tags return "" so a caller can tell "Codex reported a tier we don't
// model" apart from any specific tier.
func sandboxFromWirePolicy(policy json.RawMessage) string {
	switch readTopLevelString(policy, "type") {
	case "readOnly":
		return "read-only"
	case "workspaceWrite":
		return "workspace-write"
	case "dangerFullAccess":
		return "danger-full-access"
	default:
		return ""
	}
}

func parseThreadSettingsNotification(params json.RawMessage) (string, ThreadSettings, bool) {
	var wire threadSettingsNotification
	if json.Unmarshal(params, &wire) != nil {
		return "", ThreadSettings{}, false
	}
	settings := ThreadSettings{
		Cwd:               wire.ThreadSettings.Cwd,
		Model:             wire.ThreadSettings.Model,
		ModelProvider:     wire.ThreadSettings.ModelProvider,
		ApprovalPolicy:    wire.ThreadSettings.ApprovalPolicy,
		ApprovalsReviewer: wire.ThreadSettings.ApprovalsReviewer,
		Sandbox:           sandboxFromWirePolicy(wire.ThreadSettings.SandboxPolicy),
	}
	// effort and serviceTier are nullable upstream; null means "no
	// override in force", which is the empty string here.
	if wire.ThreadSettings.Effort != nil {
		settings.ReasoningEffort = *wire.ThreadSettings.Effort
	}
	if wire.ThreadSettings.ServiceTier != nil {
		settings.ServiceTier = *wire.ThreadSettings.ServiceTier
	}
	return strings.TrimSpace(wire.ThreadID), settings, true
}

// reconcileThreadSettings folds a `thread/settings/updated` notification
// into the session's observed-config snapshot. Called from the read loop
// for the root thread only.
//
// The threadId guard is not redundant with the child-routing suppression
// upstream of it: routing decides which AO thread an event is projected
// onto, while this decides whose configuration we are recording. A child
// agent runs its own model and effort, so letting a child's settings land
// here would misattribute the parent's token usage to the child's model.
func (s *Session) reconcileThreadSettings(params json.RawMessage) {
	threadID, settings, ok := parseThreadSettingsNotification(params)
	if !ok {
		return
	}
	if threadID != "" && threadID != strings.TrimSpace(s.rootThreadID()) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observedSettings = settings
	s.observedSettingsKnown = true
}

// ObservedThreadSettings returns Codex's last reported configuration for
// this thread and whether any has been reported yet. The false case is
// normal: a session that has never seen a `thread/settings/updated`
// (nothing has changed since thread/start) has nothing observed, and
// callers must fall back to the config they asked for rather than
// rendering an empty settings block.
func (s *Session) ObservedThreadSettings() (ThreadSettings, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observedSettings, s.observedSettingsKnown
}
