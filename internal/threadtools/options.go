package threadtools

import (
	"context"
	"encoding/json"
	"slices"

	"agent-overflow/internal/errorsx"
)

// thread_options: what a spawn can choose from, rendered from the
// catalogs the app already keeps and never from a hand-written list, so a
// provider added later appears the moment it registers one.
//
// The caller's own computer comes first and states the caller's current
// provider, model, effort, mode and runtime mode as the defaults a spawn
// inherits. The thread_spawn descriptions state the same defaults, so the
// common case needs no call here at all.

type optionsArgs struct {
	ComputerID string `json:"computer_id"`
	Provider   string `json:"provider"`
	ProjectID  string `json:"project_id"`
	What       string `json:"what"`
}

type computerOptions struct {
	ComputerID string         `json:"computer_id,omitempty"`
	Computer   string         `json:"computer,omitempty"`
	Local      bool           `json:"local,omitempty"`
	Reachable  bool           `json:"reachable"`
	OS         string         `json:"os,omitempty"`
	Defaults   *SpawnDefaults `json:"defaults,omitempty"`
	// A section what leaves out is nil and absent; one it keeps is
	// present even when empty, which is an answer about this computer.
	Providers    []ProviderOption `json:"providers,omitzero"`
	RuntimeModes []map[string]any `json:"runtime_modes,omitzero"`
	// Profiles are the person's agent profiles this computer can run,
	// resolved against its providers with the caller's own settings.
	Profiles []ProfileOption `json:"profiles,omitzero"`
	Projects []ProjectOption `json:"projects,omitzero"`
	// SpawnInstructions says this computer adds a spawn's instructions to
	// the thread's system prompt; a computer that predates them leaves it
	// out, and a spawn carrying instructions there is refused.
	SpawnInstructions bool `json:"spawn_instructions,omitempty"`
	// UnlistedModels carries each provider's Unlisted models to a caller
	// asking through its pairing, keyed by provider: they resolve a
	// profile there but are never listed. Only a forwarded answer has it,
	// and peerOptions moves it back into the providers on arrival.
	UnlistedModels map[string][]ModelOption `json:"unlisted_models,omitempty"`
}

// The two result shapes. A computer with no pairings inlines the one
// row's fields and says nothing about computers; a paired one answers
// with a row per computer, the caller's own first.
type optionsSolo struct {
	computerOptions
	ProfileNotes []string `json:"profile_notes,omitempty"`
	Note         string   `json:"note,omitempty"`
}

type optionsGrouped struct {
	Computers    []computerOptions `json:"computers"`
	Errors       []errorRow        `json:"errors,omitempty"`
	ProfileNotes []string          `json:"profile_notes,omitempty"`
	Note         string            `json:"note,omitempty"`
}

// optionsAnswerShape reads either shape, which is what a peer's reply
// needs.
type optionsAnswerShape struct {
	computerOptions
	Computers []computerOptions `json:"computers"`
}

func (c *session) options(ctx context.Context, raw json.RawMessage) (any, error) {
	var args optionsArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := c.checkComputerArg(args.ComputerID); err != nil {
		return nil, err
	}
	args.What = trim(args.What)
	if args.What != "" && !slices.Contains([]string{OptionsModels, OptionsProjects, OptionsGroups}, args.What) {
		return nil, invalidf("what must be one of models, projects or groups, or omitted for everything.")
	}
	targets := append([]Computer{c.self()}, c.computers...)
	if id := trim(args.ComputerID); id != "" {
		computer, _, ok := c.computerByID(id)
		if !ok {
			return nil, publicf(CodeInvalidRequest, "There is no computer %q. thread_options with no computer_id lists the ones you can reach.", id)
		}
		targets = []Computer{computer}
	}

	roster, err := c.optionsRoster(ctx, args.What)
	if err != nil {
		return nil, err
	}
	rows, failures, failure := c.runOptions(ctx, targets, args, roster)
	notes := rosterNotes(roster, rows)
	if !c.paired() {
		// One target, this computer: its failure is the call's failure.
		// Answering {"reachable": false} would describe an unreachable
		// computer, and this one is the computer the caller is on.
		if failure != nil {
			return nil, failure
		}
		solo := optionsSolo{ProfileNotes: notes}
		if len(rows) == 1 {
			solo.computerOptions = rows[0]
		}
		return solo, nil
	}
	grouped := optionsGrouped{Computers: rows, Errors: failures, ProfileNotes: notes}
	if grouped.Computers == nil {
		grouped.Computers = []computerOptions{}
	}
	if len(failures) > 0 {
		grouped.Note = "One or more computers did not answer. Spawning there will fail until they are back."
	}
	return grouped, nil
}

// runOptions answers every target concurrently. The third result is the
// first failure as its own error, which a single-computer call returns
// rather than describing this computer as unreachable.
func (c *session) runOptions(ctx context.Context, targets []Computer, args optionsArgs, roster *optionsRosterView) ([]computerOptions, []errorRow, error) {
	type answer struct {
		computer Computer
		row      computerOptions
		err      error
	}
	answers := eachComputer(ctx, SearchTimeout, targets, func(ctx context.Context, computer Computer) answer {
		_, local, _ := c.computerByID(computer.ID)
		row, err := c.optionsOne(ctx, computer, local, args)
		if err == nil && roster != nil && row.Providers != nil {
			// The roster is the caller's, whichever computer answered: a
			// peer's own profiles are its person's settings there, so
			// its row is resolved again against what it offers.
			row.Profiles = ListProfiles(roster.roster, row.Providers, roster.defaults)
		}
		return answer{computer: computer, row: row, err: err}
	})

	rows := make([]computerOptions, 0, len(answers))
	var failures []errorRow
	var failure error
	for _, a := range answers {
		if a.err != nil {
			failures = append(failures, newErrorRow(a.computer, a.err))
			if failure == nil {
				failure = a.err
			}
			continue
		}
		rows = append(rows, a.row)
	}
	return rows, failures, failure
}

func (c *session) optionsOne(ctx context.Context, computer Computer, local bool, args optionsArgs) (computerOptions, error) {
	id, name := c.stamp(computer)
	if !local {
		peer, err := c.app.Peer(ctx, computer.ID)
		if err != nil {
			return computerOptions{}, err
		}
		fields := map[string]any{}
		if trim(args.Provider) != "" {
			fields["provider"] = trim(args.Provider)
		}
		if trim(args.ProjectID) != "" {
			fields["project_id"] = trim(args.ProjectID)
		}
		if args.What != "" {
			fields["what"] = args.What
		}
		forwarded, err := json.Marshal(fields)
		if err != nil {
			return computerOptions{}, err
		}
		answer, err := peer.Query(ctx, "thread_options", forwarded)
		if err != nil {
			return computerOptions{}, err
		}
		row, err := peerOptions(answer)
		if err != nil {
			return computerOptions{}, err
		}
		row.ComputerID, row.Computer, row.Local, row.Defaults = id, name, false, nil
		return row, nil
	}

	catalog, err := c.app.Catalog(ctx, CatalogQuery{Provider: trim(args.Provider), ProjectID: trim(args.ProjectID), What: args.What})
	if err != nil {
		return computerOptions{}, err
	}
	row := computerOptions{
		ComputerID: id,
		Computer:   name,
		Local:      true,
		Reachable:  true,
		OS:         catalog.OS,
	}
	if args.What == "" || args.What == OptionsModels {
		row.Providers, row.RuntimeModes = catalog.Providers, RuntimeModeOptions()
		row.SpawnInstructions = true
		if row.Providers == nil {
			row.Providers = []ProviderOption{}
		}
		if Forwarded(ctx) {
			row.UnlistedModels = unlistedModels(row.Providers)
		}
	}
	if args.What != OptionsModels {
		row.Projects = projectSection(catalog.Projects, args.What)
	}
	defaults, err := c.callerDefaults(ctx)
	if err != nil {
		return computerOptions{}, err
	}
	row.Defaults = defaults
	return row, nil
}

// peerOptions reads a peer's thread_options answer as its own row, with the
// models it runs but does not list back in its providers, so the caller
// resolves the person's profiles against everything that computer can run.
// A peer that predates UnlistedModels resolves against its list alone.
func peerOptions(raw json.RawMessage) (computerOptions, error) {
	var answer optionsAnswerShape
	if err := peerResult(raw, &answer); err != nil {
		return computerOptions{}, err
	}
	row := answer.computerOptions
	if len(answer.Computers) > 0 {
		row = answer.Computers[0]
	}
	for i := range row.Providers {
		row.Providers[i].Unlisted = row.UnlistedModels[row.Providers[i].ID]
	}
	row.UnlistedModels = nil
	return row, nil
}

func unlistedModels(providers []ProviderOption) map[string][]ModelOption {
	var out map[string][]ModelOption
	for _, provider := range providers {
		if len(provider.Unlisted) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]ModelOption{}
		}
		out[provider.ID] = provider.Unlisted
	}
	return out
}

// optionsRosterView is the caller's roster and its own settings, which a
// profile that leaves a setting empty inherits.
type optionsRosterView struct {
	roster   Roster
	defaults SpawnDefaults
}

// optionsRoster reads the roster when the answer lists models, the section
// the profiles belong to; nil otherwise.
func (c *session) optionsRoster(ctx context.Context, what string) (*optionsRosterView, error) {
	if what != "" && what != OptionsModels {
		return nil, nil
	}
	roster, err := c.app.Roster(ctx)
	if err != nil {
		return nil, err
	}
	view := &optionsRosterView{roster: roster}
	defaults, err := c.callerDefaults(ctx)
	if err != nil {
		return nil, err
	}
	if defaults != nil {
		view.defaults = *defaults
	}
	return view, nil
}

// rosterNotes are the notes that apply to the profiles every answering
// computer lists together: an agent may spawn on any of them.
func rosterNotes(roster *optionsRosterView, rows []computerOptions) []string {
	if roster == nil {
		return nil
	}
	var listed []ProfileOption
	for _, row := range rows {
		listed = append(listed, row.Profiles...)
	}
	return ApplicableNotes(roster.roster.Notes, listed)
}

// callerDefaults reads the calling thread's own settings. They are what a
// spawn inherits, so the row that carries them is the caller's own.
//
// A forwarded call names a thread on the computer it came from, which this
// one does not have. The source drops the defaults of a peer row anyway, so
// a caller thread that is not here leaves them out instead of failing the
// answer this computer alone can give about its projects and models.
func (c *session) callerDefaults(ctx context.Context) (*SpawnDefaults, error) {
	thread, err := c.app.Thread(ctx, c.caller.ThreadID)
	if err != nil {
		if code, _, public := errorsx.PublicDetails(err); public && code == CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &SpawnDefaults{
		Provider:    thread.Provider,
		Model:       thread.Model,
		Effort:      thread.Effort,
		Mode:        thread.Mode,
		RuntimeMode: thread.RuntimeMode,
	}, nil
}

// projectSection keeps the part of each project what asks for. The App may
// skip the reads a section does not need; this is what decides the shape.
func projectSection(projects []ProjectOption, what string) []ProjectOption {
	out := make([]ProjectOption, 0, len(projects))
	for _, project := range projects {
		switch what {
		case OptionsGroups:
			project.Path, project.Workspaces, project.WorktreesError = "", nil, ""
		case OptionsProjects:
			project.Groups = nil
		}
		out = append(out, project)
	}
	return out
}
