package threadtools

import (
	"context"
	"encoding/json"
	"sync"
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
}

type computerOptions struct {
	ComputerID   string           `json:"computer_id,omitempty"`
	Computer     string           `json:"computer,omitempty"`
	Local        bool             `json:"local,omitempty"`
	Reachable    bool             `json:"reachable"`
	OS           string           `json:"os,omitempty"`
	Defaults     *SpawnDefaults   `json:"defaults,omitempty"`
	Providers    []ProviderOption `json:"providers"`
	RuntimeModes []map[string]any `json:"runtime_modes"`
	Projects     []ProjectOption  `json:"projects"`
}

// The two result shapes. A computer with no pairings inlines the one
// row's fields and says nothing about computers; a paired one answers
// with a row per computer, the caller's own first.
type optionsSolo struct {
	computerOptions
	Note string `json:"note,omitempty"`
}

type optionsGrouped struct {
	Computers []computerOptions `json:"computers"`
	Errors    []errorRow        `json:"errors,omitempty"`
	Note      string            `json:"note,omitempty"`
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
	targets := append([]Computer{c.self()}, c.computers...)
	if id := trim(args.ComputerID); id != "" {
		computer, _, ok := c.computerByID(id)
		if !ok {
			return nil, publicf(CodeInvalidRequest, "There is no computer %q. thread_options with no computer_id lists the ones you can reach.", id)
		}
		targets = []Computer{computer}
	}

	rows, failures := c.runOptions(ctx, targets, args)
	if !c.paired() {
		solo := optionsSolo{}
		if len(rows) == 1 {
			solo.computerOptions = rows[0]
		}
		return solo, nil
	}
	grouped := optionsGrouped{Computers: rows, Errors: failures}
	if grouped.Computers == nil {
		grouped.Computers = []computerOptions{}
	}
	if len(failures) > 0 {
		grouped.Note = "One or more computers did not answer. Spawning there will fail until they are back."
	}
	return grouped, nil
}

func (c *session) runOptions(ctx context.Context, targets []Computer, args optionsArgs) ([]computerOptions, []errorRow) {
	bounded, cancel := context.WithTimeout(ctx, SearchTimeout)
	defer cancel()

	type answer struct {
		computer Computer
		row      computerOptions
		err      error
	}
	answers := make([]answer, len(targets))
	var wg sync.WaitGroup
	for index, computer := range targets {
		wg.Add(1)
		go func(slot int, computer Computer) {
			defer wg.Done()
			_, local, _ := c.computerByID(computer.ID)
			row, err := c.optionsOne(bounded, computer, local, args)
			answers[slot] = answer{computer: computer, row: row, err: err}
		}(index, computer)
	}
	wg.Wait()

	rows := make([]computerOptions, 0, len(answers))
	var failures []errorRow
	for _, a := range answers {
		if a.err != nil {
			failures = append(failures, newErrorRow(a.computer, a.err))
			continue
		}
		rows = append(rows, a.row)
	}
	return rows, failures
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
		forwarded, err := json.Marshal(fields)
		if err != nil {
			return computerOptions{}, err
		}
		answer, err := peer.Query(ctx, "thread_options", forwarded)
		if err != nil {
			return computerOptions{}, err
		}
		var result optionsAnswerShape
		if err := peerResult(answer, &result); err != nil {
			return computerOptions{}, err
		}
		row := result.computerOptions
		if len(result.Computers) > 0 {
			row = result.Computers[0]
		}
		row.ComputerID, row.Computer, row.Local, row.Defaults = id, name, false, nil
		return row, nil
	}

	catalog, err := c.app.Catalog(ctx, CatalogQuery{Provider: trim(args.Provider), ProjectID: trim(args.ProjectID)})
	if err != nil {
		return computerOptions{}, err
	}
	row := computerOptions{
		ComputerID:   id,
		Computer:     name,
		Local:        true,
		Reachable:    true,
		OS:           catalog.OS,
		Providers:    catalog.Providers,
		RuntimeModes: RuntimeModeOptions(),
		Projects:     catalog.Projects,
	}
	if row.Providers == nil {
		row.Providers = []ProviderOption{}
	}
	if row.Projects == nil {
		row.Projects = []ProjectOption{}
	}
	defaults, err := c.callerDefaults(ctx)
	if err != nil {
		return computerOptions{}, err
	}
	row.Defaults = &defaults
	return row, nil
}

// callerDefaults reads the calling thread's own settings. They are what a
// spawn inherits, so the row that carries them is the caller's own.
func (c *session) callerDefaults(ctx context.Context) (SpawnDefaults, error) {
	thread, err := c.app.Thread(ctx, c.caller.ThreadID)
	if err != nil {
		return SpawnDefaults{}, err
	}
	return SpawnDefaults{
		Provider:    thread.Provider,
		Model:       thread.Model,
		Effort:      thread.Effort,
		Mode:        thread.Mode,
		RuntimeMode: thread.RuntimeMode,
	}, nil
}
