package threadtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Agent profiles (docs/specs/agent-thread-tools.md, "Agent profiles"). The
// roster is the person's, read from the settings of the computer the
// calling thread is on; what a profile resolves to is a fact about the
// computer a spawn targets. One resolution serves thread_options, the
// guide's list of names and the spawn that names a profile, so the three
// never disagree about what a computer can run.

// AgentProfile is one profile of the roster, as the person set it up.
// Every setting but Name and UseFor may be empty, which inherits the
// spawner's.
type AgentProfile struct {
	Name         string
	UseFor       string
	Provider     string
	Model        string
	Effort       string
	RuntimeMode  string
	Instructions string
}

// AgentNote is one note on choosing a profile and the condition it is
// shown under: NoteAlways, NoteBothProviders or NoteOneProvider.
type AgentNote struct {
	Text string
	When string
}

// AgentNote conditions. They match internal/settings' AgentNote* values.
const (
	NoteAlways        = ""
	NoteBothProviders = "both_providers"
	NoteOneProvider   = "one_provider"
)

// Roster is the person's profiles and notes.
type Roster struct {
	Profiles []AgentProfile
	Notes    []AgentNote
}

// ProfileOption is one profile as thread_options lists it for one
// computer: what a spawn naming it runs there.
type ProfileOption struct {
	Name        string `json:"name"`
	UseFor      string `json:"use_for"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Effort      string `json:"effort,omitempty"`
	RuntimeMode string `json:"runtime_mode,omitempty"`
	// Instructions says the profile appends instructions of its own to the
	// thread's system prompt.
	Instructions bool `json:"instructions,omitempty"`
}

// SpawnChoice is a spawn's provider, model and effort. As asked, an empty
// field inherits; as ResolveSpawn answers it, each field is what the
// thread runs with, Effort empty for a model with no effort setting.
type SpawnChoice struct {
	Provider string
	Model    string
	Effort   string
}

// WithProfile is the choice a spawn naming profile makes: what c names
// wins, and a provider c names other than the profile's takes neither the
// profile's model nor its effort, which belong to the provider replaced.
func (c SpawnChoice) WithProfile(profile AgentProfile) SpawnChoice {
	replaced := c.Provider != "" && profile.Provider != "" && c.Provider != profile.Provider
	if c.Provider == "" {
		c.Provider = profile.Provider
	}
	if !replaced {
		if c.Model == "" {
			c.Model = profile.Model
		}
		if c.Effort == "" {
			c.Effort = profile.Effort
		}
	}
	return c
}

// SpawnRefusal is why a computer cannot run a spawn's choice. Missing is
// true when it lacks the provider or the model, which another computer may
// have; false when the choice itself does not fit its model there.
type SpawnRefusal struct {
	Reason  string
	Missing bool
}

func (r *SpawnRefusal) Error() string { return r.Reason }

// ResolveSpawn is the one answer to what a spawn runs with: the choice on
// top of the spawner's own settings, against the catalog of the computer
// the thread will run on, which where names in a refusal. thread_options'
// profile list, the tools layer's check of a profile spawn, the settings
// editor's check of a profile and the spawn itself all call it, so what is
// listed is what runs.
//
// A provider other than the spawner's inherits nothing: its model is its
// default and its effort that model's default. On the spawner's provider
// an empty model is the spawner's, and an empty effort is the spawner's
// when the model offers it, else the model's default. An effort the choice
// names must be offered by the model, and a model with no effort setting
// takes none.
func ResolveSpawn(spawner SpawnDefaults, choice SpawnChoice, catalog []ProviderOption, where string) (SpawnChoice, error) {
	resolved := choice
	if resolved.Provider == "" {
		resolved.Provider = spawner.Provider
	}
	inherits := resolved.Provider == spawner.Provider
	index := slices.IndexFunc(catalog, func(candidate ProviderOption) bool { return candidate.ID == resolved.Provider })
	if index < 0 || len(catalog[index].Models) == 0 {
		return resolved, &SpawnRefusal{Missing: true, Reason: fmt.Sprintf("%s does not run on %s.", providerLabel(resolved.Provider), where)}
	}
	provider := catalog[index]
	if resolved.Model == "" {
		resolved.Model = provider.DefaultModel
		if inherits && spawner.Model != "" {
			resolved.Model = spawner.Model
		}
	}
	model, found := provider.model(resolved.Model)
	if !found {
		slugs := make([]string, 0, len(provider.Models))
		for _, offered := range provider.Models {
			slugs = append(slugs, offered.Slug)
		}
		return resolved, &SpawnRefusal{Missing: true, Reason: fmt.Sprintf("%s does not offer model %q on %s. It offers: %s.",
			providerLabel(provider.ID), resolved.Model, where, strings.Join(slugs, ", "))}
	}
	switch {
	case len(model.Efforts) == 0:
		if choice.Effort != "" {
			return resolved, &SpawnRefusal{Reason: fmt.Sprintf("Model %s has no reasoning effort setting on %s.", model.Slug, where)}
		}
		resolved.Effort = ""
	case choice.Effort != "":
		if !slices.Contains(model.Efforts, choice.Effort) {
			return resolved, &SpawnRefusal{Reason: fmt.Sprintf("Model %s does not offer effort %q on %s. It offers: %s.",
				model.Slug, choice.Effort, where, strings.Join(model.Efforts, ", "))}
		}
	case inherits && spawner.Effort != "" && slices.Contains(model.Efforts, spawner.Effort):
		resolved.Effort = spawner.Effort
	default:
		resolved.Effort = model.DefaultEffort
	}
	return resolved, nil
}

// model finds slug among the models the provider offers, then among those
// it resolves only for threads already on them.
func (p ProviderOption) model(slug string) (ModelOption, bool) {
	for _, list := range [][]ModelOption{p.Models, p.Unlisted} {
		if index := slices.IndexFunc(list, func(candidate ModelOption) bool { return candidate.Slug == slug }); index >= 0 {
			return list[index], true
		}
	}
	return ModelOption{}, false
}

// ProfileProblem is what the settings editor says of a profile against
// this computer's catalog: problem, a choice that does not fit its model
// here, which the editor refuses to save; or missing, a provider or model
// this computer lacks, which only means threads here are not offered the
// profile. A profile that inherits its provider or model is resolved
// against each spawner's own and has neither.
func ProfileProblem(profile AgentProfile, catalog []ProviderOption) (problem, missing string) {
	if profile.Provider == "" || profile.Model == "" {
		if profile.Provider != "" && !slices.ContainsFunc(catalog, func(candidate ProviderOption) bool { return candidate.ID == profile.Provider }) {
			return "", fmt.Sprintf("%s does not run on this computer.", providerLabel(profile.Provider))
		}
		return "", ""
	}
	_, err := ResolveSpawn(SpawnDefaults{Provider: profile.Provider}, SpawnChoice{}.WithProfile(profile), catalog, "this computer")
	var refusal *SpawnRefusal
	switch {
	case err == nil:
		return "", ""
	case errors.As(err, &refusal) && refusal.Missing:
		return "", refusal.Reason
	}
	return err.Error(), ""
}

func providerLabel(id string) string {
	switch id {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "":
		return "no provider"
	}
	return id
}

// ListProfiles is what a computer offering providers lists of roster: the
// profiles it can run, in the person's order.
func ListProfiles(roster Roster, providers []ProviderOption, defaults SpawnDefaults) []ProfileOption {
	out := make([]ProfileOption, 0, len(roster.Profiles))
	for _, profile := range roster.Profiles {
		resolved, err := ResolveSpawn(defaults, SpawnChoice{}.WithProfile(profile), providers, "")
		if err != nil {
			continue
		}
		out = append(out, ProfileOption{
			Name: profile.Name, UseFor: profile.UseFor,
			Provider: resolved.Provider, Model: resolved.Model, Effort: resolved.Effort,
			RuntimeMode: profile.RuntimeMode, Instructions: profile.Instructions != "",
		})
	}
	return out
}

// ProfileNames are the names of listed, for the guide.
func ProfileNames(listed []ProfileOption) []string {
	names := make([]string, 0, len(listed))
	for _, option := range listed {
		names = append(names, option.Name)
	}
	return names
}

// ApplicableNotes are the notes an agent is shown beside listed: a note for
// both providers only when the listed profiles run on both, one for a
// single provider only when they run on one and on at least two different
// models, and the rest whenever any profile is listed.
func ApplicableNotes(notes []AgentNote, listed []ProfileOption) []string {
	if len(listed) == 0 {
		return nil
	}
	providers, models := map[string]bool{}, map[string]bool{}
	for _, option := range listed {
		providers[option.Provider] = true
		models[option.Provider+"/"+option.Model] = true
	}
	out := make([]string, 0, len(notes))
	for _, note := range notes {
		switch note.When {
		case NoteBothProviders:
			if len(providers) < 2 {
				continue
			}
		case NoteOneProvider:
			if len(providers) != 1 || len(models) < 2 {
				continue
			}
		}
		out = append(out, note.Text)
	}
	return out
}

// findProfile looks name up in the roster, without regard to case.
func findProfile(roster Roster, name string) (AgentProfile, bool) {
	for _, profile := range roster.Profiles {
		if strings.EqualFold(profile.Name, name) {
			return profile, true
		}
	}
	return AgentProfile{}, false
}

// applyProfile layers the profile a spawn names under the call
// (SpawnChoice.WithProfile), which the destination resolves as the call's
// own choice. The profile's instructions come before the call's own.
func (c *session) applyProfile(ctx context.Context, call *SpawnCall, name string) error {
	roster, err := c.app.Roster(ctx)
	if err != nil {
		return err
	}
	profile, found := findProfile(roster, name)
	if !found {
		names := make([]string, 0, len(roster.Profiles))
		for _, profile := range roster.Profiles {
			names = append(names, profile.Name)
		}
		if len(names) == 0 {
			return invalidf("There is no agent profile %q: the user has set up none. Spawn without profile.", name)
		}
		return invalidf("There is no agent profile %q. The profiles are %s; thread_options lists the ones each computer can run.", name, joinNames(names))
	}
	call.Profile = profile.Name
	choice := SpawnChoice{Provider: call.Provider, Model: call.Model, Effort: call.Effort}.WithProfile(profile)
	call.Provider, call.Model, call.Effort = choice.Provider, choice.Model, choice.Effort
	if call.RuntimeMode == "" {
		call.RuntimeMode = profile.RuntimeMode
	}
	call.Instructions = joinInstructions(profile.Instructions, call.Instructions)
	return nil
}

// JoinInstructions appends the later instructions after the earlier,
// separated by a blank line, skipping empty ones.
func JoinInstructions(parts ...string) string {
	return joinInstructions(parts...)
}

func joinInstructions(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n\n")
}

// checkProfileOffered refuses a profile spawn the target computer cannot
// run, naming the profile: the destination's own refusal would name only
// the model. It reads the target's catalog, here or through its peer, and
// requires a target that adds instructions when the spawn carries any.
func (c *session) checkProfileOffered(ctx context.Context, call SpawnCall) error {
	if call.Profile == "" && call.Instructions == "" {
		return nil
	}
	computer, local, ok := c.computerByID(call.ComputerID)
	if !ok {
		return publicf(CodeInvalidRequest, "There is no computer %q. thread_options lists the ones you can reach.", call.ComputerID)
	}
	var providers []ProviderOption
	if local {
		catalog, err := c.app.Catalog(ctx, CatalogQuery{What: OptionsModels})
		if err != nil {
			return err
		}
		providers = catalog.Providers
	} else {
		peer, err := c.app.Peer(ctx, computer.ID)
		if err != nil {
			return err
		}
		raw, err := peer.Query(ctx, "thread_options", json.RawMessage(`{"what":"models"}`))
		if err != nil {
			return err
		}
		var answer optionsAnswerShape
		if err := peerResult(raw, &answer); err != nil {
			return err
		}
		row := answer.computerOptions
		if len(answer.Computers) > 0 {
			row = answer.Computers[0]
		}
		if call.Instructions != "" && !row.SpawnInstructions {
			return publicf(CodeInvalidRequest, "%s runs an older version that cannot add instructions to a thread's system prompt. Spawn there without instructions or a profile that has them, or spawn on this computer.", NameOfComputer(computer))
		}
		providers = row.Providers
	}
	if call.Profile == "" || call.FromThread != "" {
		return nil
	}
	defaults := SpawnDefaults{}
	if own, err := c.callerDefaults(ctx); err != nil {
		return err
	} else if own != nil {
		defaults = *own
	}
	where := "this computer"
	if !local {
		where = NameOfComputer(computer)
	}
	// The call already carries the profile's settings under the caller's
	// overrides (applyProfile), which is the choice the destination
	// resolves.
	choice := SpawnChoice{Provider: call.Provider, Model: call.Model, Effort: call.Effort}
	if _, err := ResolveSpawn(defaults, choice, providers, where); err != nil {
		return publicf(CodeInvalidRequest, "Agent profile %q cannot run: %s thread_options lists the profiles each computer can run.", call.Profile, err.Error())
	}
	return nil
}
