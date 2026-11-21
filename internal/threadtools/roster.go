package threadtools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/provider"
)

// Agent profiles (docs/specs/agent-thread-tools.md, "Agent profiles"). The
// roster is the person's, read from the settings of the computer the
// calling thread is on; what a profile resolves to is a fact about the
// computer a spawn targets. One resolution serves thread_options, the
// guide's list of names and the spawn that names a profile, so the three
// never disagree about what a computer can run.

// AgentProfile is one profile of the roster, as the person set it up.
// Every setting but Name and UseFor may be empty, which inherits the
// spawner's. Model is an exact model or a family, read by Provider's
// grammar (provider.ParseModelRef).
type AgentProfile struct {
	Name         string
	UseFor       string
	Provider     string
	Model        provider.ModelRef
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

// SpawnChoice is a spawn's provider, model and effort as asked: an empty
// field inherits, and Model may name a family.
type SpawnChoice struct {
	Provider string
	Model    provider.ModelRef
	Effort   string
}

// ResolvedSpawn is what a spawn runs with: an exact model, and Effort
// empty for a model with no effort setting.
type ResolvedSpawn struct {
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
		if c.Model.IsZero() {
			c.Model = profile.Model
		}
		if c.Effort == "" {
			c.Effort = profile.Effort
		}
	}
	return c
}

// SpawnRefusal is why a computer cannot run a spawn's choice. Missing is
// true when what it lacks is this computer's, which another computer may
// have: the provider, the model, or an effort on the model a family runs
// here. False when the choice itself does not fit the exact model it names.
type SpawnRefusal struct {
	Reason  string
	Missing bool
}

func (r *SpawnRefusal) Error() string { return r.Reason }

// ResolveSpawn is the one answer to what a spawn runs with: the choice on
// top of the spawner's own settings, against the catalog of the computer
// the thread will run on, which where names in a refusal. thread_options'
// profile list, the tools layer's check of a spawn, the settings editor's
// check of a profile and the spawn itself all call it, so what is listed
// is what runs.
//
// A provider other than the spawner's inherits nothing: its model is its
// default and its effort that model's default. On the spawner's provider
// an empty model is the spawner's, and an empty effort is the spawner's
// when the model offers it, else the model's default. A family runs the
// model provider.ResolveModel picks from the computer's list. An effort
// the choice names must be offered by the model, and a model with no
// effort setting takes none.
func ResolveSpawn(spawner SpawnDefaults, choice SpawnChoice, catalog []ProviderOption, where string) (ResolvedSpawn, error) {
	resolved := ResolvedSpawn{Provider: choice.Provider, Effort: choice.Effort}
	if resolved.Provider == "" {
		resolved.Provider = spawner.Provider
	}
	inherits := resolved.Provider == spawner.Provider
	index := slices.IndexFunc(catalog, func(candidate ProviderOption) bool { return candidate.ID == resolved.Provider })
	if index < 0 || len(catalog[index].Models) == 0 {
		return resolved, &SpawnRefusal{Missing: true, Reason: fmt.Sprintf("%s does not run on %s.", providerLabel(resolved.Provider), where)}
	}
	option := catalog[index]
	ref := choice.Model
	if ref.IsZero() {
		ref = provider.ExactModel(option.DefaultModel)
		if inherits && spawner.Model != "" {
			ref = provider.ExactModel(spawner.Model)
		}
	}
	model, err := option.resolveModel(ref, where)
	if err != nil {
		return resolved, err
	}
	resolved.Model = model.Slug
	// A family's model is this computer's newest, so an effort it does not
	// take is this computer's lack, as a missing model is.
	missing := ref.IsFamily()
	switch {
	case len(model.Efforts) == 0:
		if choice.Effort != "" {
			return resolved, &SpawnRefusal{Missing: missing, Reason: fmt.Sprintf("Model %s has no reasoning effort setting on %s.", model.Slug, where)}
		}
		resolved.Effort = ""
	case choice.Effort != "":
		if !slices.Contains(model.Efforts, choice.Effort) {
			return resolved, &SpawnRefusal{Missing: missing, Reason: fmt.Sprintf("Model %s does not offer effort %q on %s. It offers: %s.",
				model.Slug, choice.Effort, where, strings.Join(model.Efforts, ", "))}
		}
	case inherits && spawner.Effort != "" && slices.Contains(model.Efforts, spawner.Effort):
		resolved.Effort = spawner.Effort
	default:
		resolved.Effort = model.DefaultEffort
	}
	return resolved, nil
}

// resolveModel finds what ref runs among the models the provider offers
// and those it runs only for threads already on them, by
// provider.ResolveModel's precedence, or refuses naming where.
func (p ProviderOption) resolveModel(ref provider.ModelRef, where string) (ModelOption, error) {
	listed := modelSlugs(p.Models)
	if slug, ok := provider.ResolveModel(ref, listed, modelSlugs(p.Unlisted)); ok {
		for _, list := range [][]ModelOption{p.Models, p.Unlisted} {
			if index := slices.IndexFunc(list, func(candidate ModelOption) bool { return candidate.Slug == slug }); index >= 0 {
				return list[index], nil
			}
		}
	}
	if ref.IsFamily() {
		return ModelOption{}, &SpawnRefusal{Missing: true, Reason: fmt.Sprintf("%s offers no %s model on %s. It offers: %s.",
			providerLabel(p.ID), ref, where, strings.Join(listed, ", "))}
	}
	return ModelOption{}, &SpawnRefusal{Missing: true, Reason: fmt.Sprintf("%s does not offer model %q on %s. It offers: %s.",
		providerLabel(p.ID), ref.String(), where, strings.Join(listed, ", "))}
}

func modelSlugs(models []ModelOption) []string {
	slugs := make([]string, 0, len(models))
	for _, model := range models {
		slugs = append(slugs, model.Slug)
	}
	return slugs
}

// ProfileCheck is what the settings editor says of one profile against
// this computer's catalog (ProfileProblem).
type ProfileCheck struct {
	// Problem is a choice that does not fit its model here, which the
	// editor refuses to save.
	Problem string
	// Missing is a provider or model this computer lacks, which only
	// means threads here are not offered the profile.
	Missing string
	// Runs is the model the profile runs here when it names its provider
	// and model: the exact model, or the newest of its family.
	Runs string
	// Unreadable are the models a family profile's provider lists here
	// that its version rule cannot read, so the family never runs them.
	Unreadable []string
}

// ProfileProblem checks a profile against this computer's catalog. A
// profile that inherits its provider or model is resolved against each
// spawner's own, so it has no problem and no model it runs here.
func ProfileProblem(profile AgentProfile, catalog []ProviderOption) ProfileCheck {
	index := slices.IndexFunc(catalog, func(candidate ProviderOption) bool { return candidate.ID == profile.Provider })
	if profile.Provider == "" || profile.Model.IsZero() {
		if profile.Provider != "" && index < 0 {
			return ProfileCheck{Missing: fmt.Sprintf("%s does not run on this computer.", providerLabel(profile.Provider))}
		}
		return ProfileCheck{}
	}
	var check ProfileCheck
	if profile.Model.IsFamily() && index >= 0 {
		check.Unreadable = provider.UnreadableModels(profile.Provider, modelSlugs(catalog[index].Models))
	}
	resolved, err := ResolveSpawn(SpawnDefaults{Provider: profile.Provider}, SpawnChoice{}.WithProfile(profile), catalog, "this computer")
	var refusal *SpawnRefusal
	switch {
	case err == nil:
		check.Runs = resolved.Model
	case errors.As(err, &refusal) && refusal.Missing:
		check.Missing = refusal.Reason
	default:
		check.Problem = err.Error()
	}
	return check
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

// applyProfile records the profile a spawn names on the call: its runtime
// mode under the call's, and its instructions before the call's own. Its
// provider, model and effort are layered by spawnChoice.
func (c *session) applyProfile(ctx context.Context, call *SpawnCall, name string) (AgentProfile, error) {
	roster, err := c.app.Roster(ctx)
	if err != nil {
		return AgentProfile{}, err
	}
	profile, found := findProfile(roster, name)
	if !found {
		names := make([]string, 0, len(roster.Profiles))
		for _, profile := range roster.Profiles {
			names = append(names, profile.Name)
		}
		if len(names) == 0 {
			return AgentProfile{}, invalidf("There is no agent profile %q: the user has set up none. Spawn without profile.", name)
		}
		return AgentProfile{}, invalidf("There is no agent profile %q. The profiles are %s; thread_options lists the ones each computer can run.", name, joinNames(names))
	}
	call.Profile = profile.Name
	if call.RuntimeMode == "" {
		call.RuntimeMode = profile.RuntimeMode
	}
	call.Instructions = joinInstructions(profile.Instructions, call.Instructions)
	return profile, nil
}

// spawnChoice is the provider, model and effort a spawn asks for: the
// call's own over the profile's (SpawnChoice.WithProfile), with the call's
// model read by the grammar of the provider the spawn ends up on, which is
// the spawner's when neither names one. This is where a thread_spawn model
// becomes a provider.ModelRef.
func spawnChoice(call SpawnCall, profile *AgentProfile, spawner SpawnDefaults) SpawnChoice {
	choice := SpawnChoice{Provider: call.Provider, Effort: call.Effort}
	if profile != nil {
		choice = choice.WithProfile(*profile)
	}
	if call.Model != "" {
		choice.Model = provider.ParseModelRef(cmp.Or(choice.Provider, spawner.Provider), call.Model)
	}
	return choice
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

// checkSpawnChoice settles on the caller's computer what only the target's
// catalog can. A profile spawn the target cannot run is refused naming the
// profile: the destination's own refusal would name only the model. A
// family is resolved to the exact model the target lists and written into
// the call, so what reaches a spawn, here or on a paired computer of any
// release, names one model. It reads the target's catalog, here or through
// its peer, and requires a target that adds instructions when the spawn
// carries any.
func (c *session) checkSpawnChoice(ctx context.Context, call *SpawnCall, choice SpawnChoice, spawner SpawnDefaults) error {
	if call.Profile == "" && call.Instructions == "" && !choice.Model.IsFamily() {
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
		row, err := peerOptions(raw)
		if err != nil {
			return err
		}
		if call.Instructions != "" && !row.SpawnInstructions {
			return publicf(CodeInvalidRequest, "%s runs an older version that cannot add instructions to a thread's system prompt. Spawn there without instructions or a profile that has them, or spawn on this computer.", NameOfComputer(computer))
		}
		providers = row.Providers
	}
	where := "this computer"
	if !local {
		where = NameOfComputer(computer)
	}
	refuse := func(err error) error {
		if call.Profile != "" {
			return publicf(CodeInvalidRequest, "Agent profile %q cannot run: %s thread_options lists the profiles each computer can run.", call.Profile, err.Error())
		}
		return publicf(CodeInvalidRequest, "%s thread_options lists what each computer offers.", err.Error())
	}
	if call.FromThread != "" {
		// A fork resolves its provider and effort against its source
		// where it runs; only a family needs this computer's answer.
		if !choice.Model.IsFamily() {
			return nil
		}
		name := cmp.Or(choice.Provider, spawner.Provider)
		index := slices.IndexFunc(providers, func(candidate ProviderOption) bool { return candidate.ID == name })
		if index < 0 {
			return refuse(&SpawnRefusal{Missing: true, Reason: fmt.Sprintf("%s does not run on %s.", providerLabel(name), where)})
		}
		model, err := providers[index].resolveModel(choice.Model, where)
		if err != nil {
			return refuse(err)
		}
		call.Model = model.Slug
		return nil
	}
	if call.Profile == "" && !choice.Model.IsFamily() {
		return nil
	}
	resolved, err := ResolveSpawn(spawner, choice, providers, where)
	if err != nil {
		return refuse(err)
	}
	if choice.Model.IsFamily() {
		call.Model = resolved.Model
	}
	return nil
}
