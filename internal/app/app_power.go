// app_power.go is the backend half of "keep awake".
//
// Two persisted settings drive one OS sleep inhibitor: keepAwakeEnabled
// is the master switch, keepAwakeScreen decides whether the display is
// held up too. They collapse into a single power.Mode ("off" | "system"
// | "display") the moment they leave this file — the wire and the OS
// layer both take the mode, never the two booleans, so no consumer can
// reconstruct a combination the backend never meant.
//
// One apply path, three entry points:
//
//   - boot (app_startup.go), so a persisted "on" is asserted without the
//     user touching anything;
//   - the UpdateSettings fan-out (app_settings.go), so a flip applies
//     immediately with no restart;
//   - implicitly, a launcher (re)connect — see below.
//
// Each apply does two things. It calls internal/power for THIS process's
// OS, and it emits the power:keepawake directive for the process that
// owns the machine's power state when that isn't us. In the shipped
// Windows/WSL split those are different processes: the backend runs
// inside a Linux distro whose D-Bus inhibitors cannot influence the
// Windows host, so internal/power is a deliberate no-op there and the
// directive is the real mechanism — the launcher answers it by calling
// power.Apply on the Windows side (cmd/agent-overflow-windows).
//
// The directive is a LEVEL, not an edge, and that is what its
// RetentionLatestOnly policy row buys: the launcher subscribes with a
// zero replay cursor on every connection, so the newest frame is
// delivered whether the backend emitted it an hour ago at boot or emits
// it a moment later. Convergence needs no re-emit on subscribe and no
// state tracking on either side.
//
// Failsafes are inherent everywhere (see internal/power): whichever
// process holds the inhibitor releases it by dying. Nothing here has a
// shutdown hook, and nothing should grow one.
package app

import (
	"log"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/power"
	"agent-overflow/internal/settings"
)

// keepAwakeDirective is the power:keepawake payload. One explicit mode
// string; see the package comment for why it isn't the two booleans.
type keepAwakeDirective struct {
	Mode string `json:"mode"`
}

// keepAwakeSettingsKeys are the settings keys whose save must re-apply
// the inhibitor. Presence in the patch is what counts, not the value:
// clearing an axis converges exactly as setting one does.
var keepAwakeSettingsKeys = [...]string{
	"keepAwakeEnabled",
	"keepAwakeScreen",
}

// patchTouchesKeepAwake reports whether an UpdateSettings patch names
// either keep-awake key.
func patchTouchesKeepAwake(patch map[string]any) bool {
	for _, key := range keepAwakeSettingsKeys {
		if _, ok := patch[key]; ok {
			return true
		}
	}
	return false
}

// applyKeepAwake drives both legs from one settings snapshot.
//
// The local error is LOGGED, not surfaced: an OS with no reachable
// inhibitor (a bare session with neither a session manager nor logind) is
// a machine that simply cannot be told to stay awake, and there is no
// action the user could take in response. It also must not stop the
// emit — on WSL the local leg is a no-op by design and the directive is
// the entire feature.
func (a *App) applyKeepAwake(cfg settings.Settings) {
	mode := power.ModeFor(cfg.KeepAwakeEnabled, cfg.KeepAwakeScreen)
	apply := a.keepAwakeApply
	if apply == nil {
		apply = power.Apply
	}
	if err := apply(mode); err != nil {
		log.Printf("keep awake: apply mode %s: %v", mode, err)
	}
	a.emit(eventchan.PowerKeepAwake, keepAwakeDirective{Mode: mode.String()})
}
