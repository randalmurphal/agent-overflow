package threadtools

import (
	"context"
	"encoding/json"
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

// resolveProfile answers what profile runs on a computer offering
// providers, with defaults the spawner's own settings. ok is false, with
// the reason, when that computer cannot run it.
func resolveProfile(profile AgentProfile, providers []ProviderOption, defaults SpawnDefaults) (option ProfileOption, ok bool, reason string) {
	option = ProfileOption{
		Name: profile.Name, UseFor: profile.UseFor,
		RuntimeMode: profile.RuntimeMode, Instructions: profile.Instructions != "",
	}
	inherited := profile.Provider == "" || profile.Provider == defaults.Provider
	option.Provider = profile.Provider
	if option.Provider == "" {
		option.Provider = defaults.Provider
	}
	index := slices.IndexFunc(providers, func(candidate ProviderOption) bool { return candidate.ID == option.Provider })
	if index < 0 {
		return option, false, fmt.Sprintf("it runs on %s, which is not offered there", providerLabel(option.Provider))
	}
	provider := providers[index]
	option.Model = profile.Model
	if option.Model == "" {
		option.Model = provider.DefaultModel
		if inherited && defaults.Model != "" {
			option.Model = defaults.Model
		}
	}
	modelIndex := slices.IndexFunc(provider.Models, func(candidate ModelOption) bool { return candidate.Slug == option.Model })
	if modelIndex < 0 {
		return option, false, fmt.Sprintf("%s does not offer its model %s there", providerLabel(option.Provider), option.Model)
	}
	model := provider.Models[modelIndex]
	option.Effort = profile.Effort
	switch {
	case len(model.Efforts) == 0:
		option.Effort = ""
	case option.Effort == "" && inherited && profile.Model == "" && defaults.Effort != "":
		option.Effort = defaults.Effort
	case option.Effort == "":
		option.Effort = model.DefaultEffort
	case !slices.Contains(model.Efforts, option.Effort):
		return option, false, fmt.Sprintf("its model %s does not offer effort %s there", option.Model, option.Effort)
	}
	return option, true, ""
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
		if option, ok, _ := resolveProfile(profile, providers, defaults); ok {
			out = append(out, option)
		}
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

// applyProfile resolves the profile a spawn names into the call: what the
// call itself names wins, and a provider the call names other than the
// profile's takes neither the profile's model nor its effort, which belong
// to the provider replaced. The profile's instructions come before the
// call's own.
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
	replaced := call.Provider != "" && profile.Provider != "" && call.Provider != profile.Provider
	if call.Provider == "" {
		call.Provider = profile.Provider
	}
	if !replaced {
		if call.Model == "" {
			call.Model = profile.Model
		}
		if call.Effort == "" {
			call.Effort = profile.Effort
		}
	}
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
	// The call already carries the profile's settings with the caller's
	// overrides, so it is resolved as a profile of its own.
	resolved := AgentProfile{Name: call.Profile, Provider: call.Provider, Model: call.Model, Effort: call.Effort}
	if _, ok, reason := resolveProfile(resolved, providers, defaults); !ok {
		where := "this computer"
		if !local {
			where = NameOfComputer(computer)
		}
		return publicf(CodeInvalidRequest, "Agent profile %q cannot run on %s: %s. thread_options lists the profiles each computer can run.", call.Profile, where, reason)
	}
	return nil
}
