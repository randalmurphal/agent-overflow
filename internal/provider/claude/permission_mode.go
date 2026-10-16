package claude

import (
	"encoding/json"
	"log"
	"strings"

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
// cached mode. Anything else is the CLI acting alone: the session adopts the
// mode as both current and base and forwards EventPermissionModeChanged so
// the app moves the thread's runtime mode with it.
//
// "plan" is never adopted. Plan is AO's interaction mode, set per turn, and a
// CLI-side entry into plan is not a change of the thread's access.

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
	log.Printf("claude: thread %s: the CLI changed its permission mode from %q to %q on its own", s.threadID, s.currentPermissionMode, reported)
	s.currentPermissionMode = reported
	s.basePermissionMode = reported
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
