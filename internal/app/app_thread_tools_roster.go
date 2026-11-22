package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The agent roster and the sub-thread limit as this computer applies them
// (docs/specs/agent-thread-tools.md, "Agent profiles" and "Sub-thread
// depth"). Settings own the values; threadtools owns what an agent reads
// of them; this file answers what only the app knows: which providers this
// computer offers, a thread's level, and what a session at the limit loses.

// Roster answers the person's agent profiles and notes from this
// computer's settings.
func (t threadToolsApp) Roster(context.Context) (threadtools.Roster, error) {
	current := t.app.currentSettings()
	roster := threadtools.Roster{
		Profiles: make([]threadtools.AgentProfile, 0, len(current.AgentProfiles)),
		Notes:    make([]threadtools.AgentNote, 0, len(current.AgentNotes)),
	}
	for _, profile := range current.AgentProfiles {
		roster.Profiles = append(roster.Profiles, rosterProfile(profile))
	}
	for _, note := range current.AgentNotes {
		roster.Notes = append(roster.Notes, threadtools.AgentNote{Text: note.Text, When: note.When})
	}
	return roster, nil
}

// AgentProfileCheck is what the roster editor shows beside one profile
// (threadtools.ProfileProblem): Problem, a choice that does not fit its
// model here, which the editor and UpdateSettings refuse; Missing, a
// provider or model this computer lacks, so threads here are not offered
// the profile; Runs, the model a profile naming its provider and model
// runs here, the newest of its family for a family; Unreadable, the models
// a family profile's provider lists here that its version rule cannot read
// and so never runs; Family, that the profile's model reads as a family by
// its provider's version rule, which the editor reads from its window's
// own computer to hold back a family on a computer that predates them.
type AgentProfileCheck struct {
	Problem    string   `json:"problem,omitempty"`
	Missing    string   `json:"missing,omitempty"`
	Runs       string   `json:"runs,omitempty"`
	Unreadable []string `json:"unreadable,omitempty"`
	Family     bool     `json:"family,omitempty"`
}

// AgentRosterSeeds are the profiles and notes this computer's release
// starts with (settings.DefaultSettings), which Restore writes back. A
// computer without the method predates model families: the editor says so
// in place of restoring there, and holds back a family typed there.
type AgentRosterSeeds struct {
	Profiles []settings.AgentProfile `json:"profiles"`
	Notes    []settings.AgentNote    `json:"notes"`
}

// AgentRosterSeeds answers this computer's starting roster.
//
//ao:scope settings:read
//ao:route home
func (a *App) AgentRosterSeeds() AgentRosterSeeds {
	return AgentRosterSeeds{
		Profiles: slices.Clone(settings.DefaultSettings.AgentProfiles),
		Notes:    slices.Clone(settings.DefaultSettings.AgentNotes),
	}
}

// CheckAgentProfiles checks each profile against what this computer offers
// a spawn, with the resolution its spawns and thread_options use, so the
// editor refuses what thread_options would never list.
//
//ao:scope settings:read
//ao:route home
func (a *App) CheckAgentProfiles(ctx context.Context, profiles []settings.AgentProfile) ([]AgentProfileCheck, error) {
	catalog, err := a.threadTools().Catalog(ctx, threadtools.CatalogQuery{What: threadtools.OptionsModels})
	if err != nil {
		return nil, err
	}
	checks := make([]AgentProfileCheck, len(profiles))
	for i, profile := range profiles {
		read := rosterProfile(profile)
		check := threadtools.ProfileProblem(read, catalog.Providers)
		checks[i] = AgentProfileCheck{Problem: check.Problem, Missing: check.Missing, Runs: check.Runs, Unreadable: check.Unreadable, Family: read.Model.IsFamily()}
	}
	return checks, nil
}

// refuseUnrunnableProfiles refuses a settings patch whose agent profiles
// name an effort their model does not offer here (CheckAgentProfiles).
func (a *App) refuseUnrunnableProfiles(ctx context.Context, patch map[string]any) error {
	raw, ok := patch["agentProfiles"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var profiles []settings.AgentProfile
	if err := json.Unmarshal(encoded, &profiles); err != nil {
		// The settings update's own validation names the shape it wants.
		return nil
	}
	checks, err := a.CheckAgentProfiles(ctx, profiles)
	if err != nil {
		return err
	}
	for i, check := range checks {
		if check.Problem != "" {
			return errorsx.Public(transport.ErrCodeBadParams, fmt.Sprintf("Agent profile %s: %s", strings.TrimSpace(profiles[i].Name), check.Problem), nil)
		}
	}
	return nil
}

// rosterProfile is a settings profile as threadtools reads it: its model,
// an exact model or a family, read by its provider's grammar.
func rosterProfile(profile settings.AgentProfile) threadtools.AgentProfile {
	return threadtools.AgentProfile{
		Name: profile.Name, UseFor: profile.UseFor, Provider: profile.Provider,
		Model:  provider.ParseModelRef(profile.Provider, profile.Model),
		Effort: profile.Effort, RuntimeMode: profile.RuntimeMode, Instructions: profile.Instructions,
	}
}

// providerOffered reports whether this computer offers a provider to a
// spawn: the person has not turned it off in Settings (claudeEnabled,
// codexEnabled, the switches the pickers obey), and its binary resolves, as
// the provider status check's first step finds it. Sign-in is not read: no
// cheap signal of it exists, and a signed-out provider says so at its
// first turn.
func (a *App) providerOffered(name string) bool {
	current := a.currentSettings()
	switch name {
	case string(provider.Claude):
		if !current.ClaudeEnabled {
			return false
		}
	case string(provider.Codex):
		if !current.CodexEnabled {
			return false
		}
	default:
		return false
	}
	_, ok := a.resolveProviderBinaryIdentity(name)
	return ok
}

// subThreadPlace is where a session's thread sits in the owned lineage when
// the session starts: its level and the limit then. The zero value is no
// limit known, which spawns as before.
type subThreadPlace struct {
	level, limit int
}

func (p subThreadPlace) canSpawn() bool { return p.limit == 0 || p.level < p.limit }

// subThreadPlaceOf reads a thread's level and this computer's limit.
func (a *App) subThreadPlaceOf(threadID string) (subThreadPlace, error) {
	level, err := a.store.SubThreadLevel(threadID)
	if err != nil {
		return subThreadPlace{}, err
	}
	return subThreadPlace{level: level, limit: a.currentSettings().SubThreadLevels}, nil
}

// spawnShape is what a session's guide and tool list say about spawning:
// its place, fixed at its start, and the providers this computer offers
// now with the profiles on them (threadtools.GuideProfileNames). It reads
// no model list, so a session's start or handshake never starts or waits
// on a provider probe. A roster that cannot be read leaves the names out
// rather than the guide.
func (a *App) spawnShape(threadID string, place subThreadPlace, defaults threadtools.SpawnDefaults) threadtools.SpawnShape {
	shape := threadtools.SpawnShape{Level: place.level, Limit: place.limit}
	for _, name := range threadToolsProviders {
		if a.providerOffered(name) {
			shape.Providers = append(shape.Providers, name)
		}
	}
	roster, err := a.threadTools().Roster(context.Background())
	if err != nil {
		log.Printf("thread tools: agent profiles for the guide of %s: %v", threadID, err)
		return shape
	}
	shape.Profiles = threadtools.GuideProfileNames(roster, shape.Providers, defaults.Provider)
	return shape
}

// nativeSubagentTools are what a session at the sub-thread limit is not
// given of its provider's own subagents: Claude's agent launcher, by both
// the names it has had, and Codex's collaboration agents toggle.
func nativeSubagentTools(providerName string) []string {
	switch providerName {
	case string(provider.Claude), string(provider.ClaudeTUI):
		return claude.SubagentLaunchTools
	case string(provider.Codex):
		return []string{codex.ToggleCollabAgents}
	}
	return nil
}

// withoutNativeSubagents adds the provider's subagent tools to a session's
// disabled tools when its thread is at the sub-thread limit. The disabled
// list is spawn-only and the reconciler pins it to the live session's, so a
// limit reached or left mid-session applies at the next start.
func withoutNativeSubagents(opts *provider.SessionOptions, providerName string, place subThreadPlace) {
	if place.canSpawn() {
		return
	}
	for _, tool := range nativeSubagentTools(providerName) {
		if !slices.Contains(opts.DisabledTools, tool) {
			opts.DisabledTools = append(opts.DisabledTools, tool)
		}
	}
}

// refuseSpawnPastLimit refuses a spawn from a thread at the limit: a
// session that started before the setting changed or before the thread was
// taken back into ownership still has thread_spawn, and so does a call that
// raced one. It returns the level a thread the spawn makes will have.
func (t threadToolsApp) refuseSpawnPastLimit(callerThreadID string) (int, error) {
	place, err := t.app.subThreadPlaceOf(callerThreadID)
	if err != nil {
		return 0, err
	}
	if !place.canSpawn() {
		return 0, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"This thread is %s of sub-threads below the thread the user talks to, and the limit is %d (Settings, Agent profiles), so it cannot spawn threads. Do the work yourself, or say in your answer that it needs more hands.",
			threadtools.LevelCount(place.level), place.limit), nil)
	}
	return place.level + 1, nil
}

// carriedLevel is the level a request's new thread stores: what a paired
// computer's spawn or ask carried, none below zero, and zero for this
// computer's own, whose spawn link or ask counts instead.
func carriedLevel(origin threadRequestOrigin, level int) int {
	if !origin.foreign {
		return 0
	}
	return max(level, 0)
}

// refuseCarriedLevelPastLimit refuses a paired computer's spawn whose thread
// would sit past this computer's own limit.
func (t threadToolsApp) refuseCarriedLevelPastLimit(level int) error {
	limit := t.app.currentSettings().SubThreadLevels
	if level <= limit {
		return nil
	}
	return errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
		"That thread would be %s of sub-threads below the thread the user talks to, and this computer allows %d.", threadtools.LevelCount(level), limit), nil)
}

// refuseInstructionsUnappended refuses spawn instructions for a provider
// whose sessions cannot append to the system prompt. Claude, Claude's
// terminal and Codex all can.
func refuseInstructionsUnappended(providerName, instructions string) error {
	if instructions == "" {
		return nil
	}
	switch providerName {
	case string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex):
		return nil
	}
	return errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
		"%s sessions cannot add instructions to their system prompt. Put them in the prompt instead.", providerName), nil)
}
