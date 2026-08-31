package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/settings"
)

// The `settings:updated` broadcast.
//
// Every persisted settings mutation broadcasts the tier it moved and the keys
// it moved in that tier, so a second attached client converges without a
// refresh — the RPC's return value only ever reaches the client that issued
// it. The rules match `thread:updated` (app_thread_broadcast.go), with one
// difference that is deliberate:
//
//   - One emit per tier a write moved. A settings-panel save touching a font
//     and a confirmation is two frames, because the two tiers are separately
//     authorized (spec §6) and, from phase 4, separately stored.
//   - A write that changed nothing emits nothing. The settings service reports
//     the changed keys, so a repeat save of an unchanged form is silent.
//   - **The frame names keys, never values.** GetSettings redacts endpoint
//     tokens and sensitive environment values, and there is no read path for
//     either; a broadcast carrying values would be the single place around
//     that. Receivers re-read through GetSettings and get the same redacted
//     projection the initiator got back from its own write, so initiator and
//     echo converge on identical state.
//
// Registered once at startup (registerSettingsBroadcast); the settings service
// holds exactly one observer, which is what keeps this the only emit site.

// SettingsUpdatedEvent is the wire shape for settings:updated.
//
// Tier is one of "host" / "user" / "device" — WHOSE preference moved (the
// backend machine's, the person's, or this screen's). Keys are the changed
// settings keys belonging to that tier, sorted.
type SettingsUpdatedEvent struct {
	Tier string   `json:"tier"`
	Keys []string `json:"keys"`
}

// setSettingsService installs the settings service AND its broadcast in one
// step, so an App can never hold a service whose writes nothing announces.
// TestSettingsServiceIsInstalledThroughOneHelper keeps this the only
// assignment site; a second one would be a settings surface that silently
// stopped converging other clients.
func (a *App) setSettingsService(service *settings.Service) {
	a.settings = service
	a.registerSettingsBroadcast()
}

// registerSettingsBroadcast wires the settings service's change observer to
// the event bus.
func (a *App) registerSettingsBroadcast() {
	if a.settings == nil {
		return
	}
	a.settings.SetChangeObserver(func(changes []settings.TierChange) {
		for _, change := range changes {
			a.emitEvent(eventchan.SettingsUpdated, SettingsUpdatedEvent{
				Tier: string(change.Tier),
				Keys: change.Keys,
			})
		}
	})
}
