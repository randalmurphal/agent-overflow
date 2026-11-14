package app

import (
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// providerInFlightTurn is one live thread with a turn in flight and the model
// that turn runs on.
type providerInFlightTurn struct {
	ThreadID string
	Model    string
}

// providerTurnsInFlight lists the live threads of a usage family
// (provider.UsageProviderFamily) that have a turn in flight. A Claude
// session's runtime turn counter never moves, so in flight is triage's open
// turn or round for every provider.
func (a *App) providerTurnsInFlight(family string) []providerInFlightTurn {
	var turns []providerInFlightTurn
	a.eachProviderTurnInFlight(family, func(threadID string, entry session) bool {
		turns = append(turns, providerInFlightTurn{ThreadID: threadID, Model: a.sessionRunningModel(threadID, entry)})
		return true
	})
	return turns
}

// providerHasTurnInFlight reports whether any live thread of a usage family
// has a turn in flight.
func (a *App) providerHasTurnInFlight(family string) bool {
	found := false
	a.eachProviderTurnInFlight(family, func(string, session) bool {
		found = true
		return false
	})
	return found
}

// eachProviderTurnInFlight calls visit for each live session of family with
// a turn in flight until visit returns false.
func (a *App) eachProviderTurnInFlight(family string, visit func(threadID string, entry session) bool) {
	if a.triage == nil {
		return
	}
	family = provider.UsageProviderFamily(family)
	for threadID, entry := range a.sessionManager().snapshot() {
		if provider.UsageProviderFamily(entry.Provider) != family || !a.triage.HasInFlightTurnOrRound(threadID) {
			continue
		}
		if !visit(threadID, entry) {
			return
		}
	}
}

// threadRunningModel is the model thread runs on now, or would run on at its
// next send: the provider's rerouted or fallback model, else the live
// session's launch model, else the stored model.
func (a *App) threadRunningModel(thread store.Thread) string {
	launchModel := ""
	if entry, ok := a.sessionManager().get(thread.ID); ok {
		launchModel = entry.LaunchOptions.Model
	}
	return a.runningModel(thread.ID, thread.Provider, launchModel, func() string { return thread.Model })
}

// sessionRunningModel is threadRunningModel for a live session whose thread
// row is not at hand; the row is read only when the session names no model.
func (a *App) sessionRunningModel(threadID string, entry session) string {
	return a.runningModel(threadID, entry.Provider, entry.LaunchOptions.Model, func() string {
		thread, err := a.store.GetThread(threadID)
		if err != nil {
			log.Printf("running model of thread %s: %v", threadID, err)
			return ""
		}
		return thread.Model
	})
}

// runningModel applies the precedence; storedModel is read only when neither
// the effective nor the launch model is known. A launch or stored model can
// be an alias (`opus`, `default`). Usage limits are scoped to the model the
// Claude CLI says it names, so that model is returned when the CLI reported
// one.
func (a *App) runningModel(threadID, providerName, launchModel string, storedModel func() string) string {
	if a.triage != nil {
		if model, _ := a.triage.EffectiveModelSnapshot(threadID); model != "" {
			return model
		}
	}
	model := launchModel
	if model == "" {
		model = storedModel()
	}
	if provider.UsageProviderFamily(providerName) != string(provider.Claude) {
		return model
	}
	alias := provider.NormalizeModelSlug(string(provider.Claude), model)
	if named := a.providerDiscoveryService().ClaudeModelAliases()[alias]; named != "" {
		return named
	}
	return model
}
