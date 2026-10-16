package claude

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// The CLI reports every change of its permission mode on `system/status`
// (`{"status":null,"permissionMode":"default"}`, 2.1.300 bundle), including
// the ones AO asked for. Most of those reports confirm what the session
// already believes. The one that
// does not is the CLI changing mode on its own: since 2.1.299 it leaves auto
// mode when `set_model` moves the session to a model without auto support,
// before it acks the switch. Without reading that report the session keeps
// `auto` cached as its base, and the next turn's set_permission_mode asks
// for a mode the CLI now refuses.
//
// A report is AO's own when a set_permission_mode for that mode is in
// flight (the CLI writes the report around its ack) or when it matches the
// cached mode. Anything else is the CLI acting alone, and only one such
// change is adopted: `default` while the base is `auto`, the drop above. The
// session takes it as both current and base and forwards
// EventPermissionModeChanged so the app moves the thread's runtime mode with
// it. Every other report is recorded as the current mode, so the next turn
// restores the base, and is never taken as the thread's access: leaving plan
// can land the CLI on the user's own `permissions.defaultMode` (acceptEdits,
// say), which the user never picked for this thread.
//
// In plan the CLI drops nothing and reports nothing: it keeps auto as the
// mode to return to. The drop surfaces when AO restores auto: the CLI refuses
// with error_code "auto_mode_model", and the session takes the same drop
// (dropAutoBaseRefusedByModel).
//
// A session spawned on auto for a model without it can start on default
// instead, and nothing reports the change but `system/init`, which restates
// the mode the process runs before every turn. init is read for that one
// change alone (initPermissionModeDrop): for a supervised spawn it carries
// the user's own `permissions.defaultMode`, which says nothing about the
// thread's access.
//
// "plan" is never adopted. Plan is AO's interaction mode, set per turn, and a
// CLI-side entry into plan is not a change of the thread's access.

// autoModeUnavailableForModel is set_permission_mode's error_code when the
// session's model has no auto mode (2.1.300 bundle).
const autoModeUnavailableForModel = "auto_mode_model"

// notePermissionModeRequested marks a set_permission_mode for mode as in
// flight. Paired with notePermissionModeSettled.
func (s *Session) notePermissionModeRequested(mode string) {
	s.permissionModeMu.Lock()
	defer s.permissionModeMu.Unlock()
	if s.requestedPermissionModes == nil {
		s.requestedPermissionModes = make(map[string]int, 1)
	}
	s.requestedPermissionModes[mode]++
}

func (s *Session) notePermissionModeSettled(mode string) {
	s.permissionModeMu.Lock()
	defer s.permissionModeMu.Unlock()
	if s.requestedPermissionModes[mode] <= 1 {
		delete(s.requestedPermissionModes, mode)
		return
	}
	s.requestedPermissionModes[mode]--
}

// adoptReportedPermissionMode applies one CLI permission-mode report and
// answers whether it was a change the CLI made on its own, which is the only
// report worth forwarding. Read-loop only.
func (s *Session) adoptReportedPermissionMode(reported string) bool {
	reported = strings.TrimSpace(reported)
	if reported == "" || reported == "plan" {
		return false
	}
	if normalizeClaudePermissionMode(reported) != reported {
		// Not adopted: normalizing it to "default" would invent a
		// supervised session the CLI never reported.
		log.Printf("claude: thread %s: ignoring unknown reported permission mode %q", s.threadID, reported)
		return false
	}
	s.permissionModeMu.Lock()
	defer s.permissionModeMu.Unlock()
	if s.requestedPermissionModes[reported] > 0 || s.currentPermissionMode == reported {
		return false
	}
	if s.basePermissionMode != "auto" || reported != "default" {
		log.Printf("claude: thread %s: the CLI moved from %q to %q on its own; the thread keeps %q", s.threadID, s.currentPermissionMode, reported, s.basePermissionMode)
		s.currentPermissionMode = reported
		return false
	}
	log.Printf("claude: thread %s: the CLI left auto mode on its own", s.threadID)
	s.currentPermissionMode = reported
	s.basePermissionMode = reported
	return true
}

// initPermissionModeDrop applies the auto drop when a `system/init` says the
// process runs default while the session's base and current mode are auto
// and no set_permission_mode is in flight, and returns the
// EventPermissionModeChanged to forward after the init. Any other init mode
// is left alone. Read-loop only.
func (s *Session) initPermissionModeDrop(evt provider.ProviderEvent) *provider.ProviderEvent {
	var init struct {
		PermissionMode string `json:"permissionMode"`
	}
	if len(evt.Raw) == 0 || json.Unmarshal(evt.Raw, &init) != nil || strings.TrimSpace(init.PermissionMode) != "default" {
		return nil
	}
	s.permissionModeMu.Lock()
	if s.basePermissionMode != "auto" || s.currentPermissionMode != "auto" || len(s.requestedPermissionModes) > 0 {
		s.permissionModeMu.Unlock()
		return nil
	}
	s.basePermissionMode = "default"
	s.currentPermissionMode = "default"
	s.permissionModeMu.Unlock()
	log.Printf("claude: thread %s: the CLI runs default, not the auto mode it was started on", s.threadID)
	meta, _ := json.Marshal(provider.PermissionModeChangeMeta{Mode: "default"})
	return &provider.ProviderEvent{
		Kind:      provider.EventPermissionModeChanged,
		ThreadID:  s.threadID,
		Meta:      meta,
		Timestamp: evt.Timestamp,
	}
}

// dropAutoBaseRefusedByModel takes the CLI's refusal of auto for the
// session's model as the drop it makes outside plan: the base becomes
// `default`, and EventPermissionModeChanged tells the app. False when the
// base is not auto, so a refused request the base no longer asks for is
// reported as the error it is.
func (s *Session) dropAutoBaseRefusedByModel() bool {
	s.permissionModeMu.Lock()
	if s.basePermissionMode != "auto" {
		s.permissionModeMu.Unlock()
		return false
	}
	s.basePermissionMode = "default"
	s.permissionModeMu.Unlock()
	log.Printf("claude: thread %s: the CLI refused auto mode for this model; the session runs default", s.threadID)
	meta, _ := json.Marshal(provider.PermissionModeChangeMeta{Mode: "default"})
	s.onEvent(provider.ProviderEvent{
		Kind:      provider.EventPermissionModeChanged,
		ThreadID:  s.threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
	return true
}

// filterPermissionModeEvent runs the read loop's half of the contract above
// on an EventPermissionModeChanged: true forwards it to the app.
func (s *Session) filterPermissionModeEvent(evt provider.ProviderEvent) bool {
	var meta provider.PermissionModeChangeMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		log.Printf("claude: thread %s: unreadable permission mode report: %v", s.threadID, err)
		return false
	}
	return s.adoptReportedPermissionMode(meta.Mode)
}

// BasePermissionMode is the CLI permission mode the session restores outside
// plan turns: what AO asked for, or what the CLI moved to on its own.
func (s *Session) BasePermissionMode() string {
	s.permissionModeMu.RLock()
	defer s.permissionModeMu.RUnlock()
	return s.basePermissionMode
}

// RuntimeModeForPermissionMode is the inverse of claudeBasePermissionMode:
// the runtime mode whose session runs the CLI permission mode. "plan" and
// unknown names have none. TestRuntimeModeForPermissionModeInvertsTheMapping
// pins the two directions together.
func RuntimeModeForPermissionMode(mode string) (provider.RuntimeMode, bool) {
	for _, runtime := range provider.AllRuntimeModes {
		if claudeBasePermissionMode(runtime) == mode {
			return runtime, true
		}
	}
	return "", false
}
