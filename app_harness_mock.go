// app_harness_mock.go — the mock-provider control surface: scenario
// assignment rules, the loopback control server ao-mockprovider dials
// at boot, and the live-driving RPCs (list mocks, advance gates, inject
// frames, kill). Progress reports from mocks re-emit as harness:mock
// events so tests await scenario step boundaries instead of sleeping.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// harnessScenarioRule maps registering mocks to a scenario. Provider
// comes from the scenario document itself; Cwd is the optional
// workspace selector (empty matches any workspace). One rule per
// (provider, cwd) pair — setting again replaces.
type harnessScenarioRule struct {
	Provider    string          `json:"provider"`
	Cwd         string          `json:"cwd,omitempty"`
	Name        string          `json:"name"`
	FixtureRoot string          `json:"fixtureRoot"`
	scenarioDoc json.RawMessage `json:"-"`
}

// HarnessScenarioSpec is the HarnessSetScenario input.
type HarnessScenarioSpec struct {
	// Name selects a shipped library scenario when Scenario is empty;
	// with an inline Scenario it is ignored (the document names itself).
	Name string `json:"name,omitempty"`
	// Scenario is an inline scenario JSON document. Validated with the
	// same parser the mock uses, so a bad script fails here — at set
	// time — instead of inside a spawned process.
	Scenario json.RawMessage `json:"scenario,omitempty"`
	// Cwd scopes the rule to mocks spawned in that workspace (multi-
	// thread tests drive different scripts per project). Empty matches
	// any workspace of the scenario's provider.
	Cwd string `json:"cwd,omitempty"`
	// FixtureRoot resolves the scenario's relative fixture paths.
	// Defaults to the harness data root.
	FixtureRoot string `json:"fixtureRoot,omitempty"`
}

// harnessMockEvent is the harness:mock wire shape.
type harnessMockEvent struct {
	MockID   string         `json:"mockId"`
	Protocol string         `json:"protocol"`
	Cwd      string         `json:"cwd"`
	Scenario string         `json:"scenario"`
	Report   control.Report `json:"report"`
}

// startControl boots the loopback control server and hands its
// address + token to provider spawns via App.providerExtraEnv — scoped
// to ao-mockprovider processes, not exported process-wide, so other
// harness children (terminals, git hooks) never inherit the control
// credentials. Must run before App.Start so no session can spawn a
// mock that misses the registration window.
func (h *Harness) startControl() error {
	srv, err := control.NewServer(control.ServerConfig{
		Resolve:  h.resolveScenario,
		OnReport: h.onMockReport,
	})
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	h.app.providerExtraEnv = map[string]string{
		control.EnvAddr:  srv.Addr(),
		control.EnvToken: srv.Token(),
	}
	h.mu.Lock()
	h.control = srv
	h.mu.Unlock()
	return nil
}

// shutdownControl stops the control listener; long-polling mocks see
// their connections close and fall back to scenario-only behaviour.
func (h *Harness) shutdownControl() {
	h.mu.Lock()
	srv := h.control
	h.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "harness: control shutdown: %v\n", err)
	}
}

// resolveScenario picks the scenario for a registering mock: the most
// specific matching rule (cwd-scoped beats catch-all), falling back to
// the provider's shipped default so a zero-config harness still streams
// a sensible first reply.
func (h *Harness) resolveScenario(reg control.Registration) (control.Assignment, error) {
	h.mu.Lock()
	var best *harnessScenarioRule
	for i := range h.scenarioRules {
		r := &h.scenarioRules[i]
		if r.Provider != reg.Protocol {
			continue
		}
		if r.Cwd != "" && !sameCanonicalPath(r.Cwd, reg.Cwd) {
			continue
		}
		if best == nil || (best.Cwd == "" && r.Cwd != "") {
			best = r
		}
	}
	if best != nil {
		assignment := control.Assignment{
			ScenarioName: best.Name,
			ScenarioJSON: best.scenarioDoc,
			FixtureRoot:  best.FixtureRoot,
		}
		h.mu.Unlock()
		return assignment, nil
	}
	h.mu.Unlock()

	name, err := scenario.DefaultName(reg.Protocol)
	if err != nil {
		return control.Assignment{}, fmt.Errorf("no scenario rule matches %s mock in %s and %w", reg.Protocol, reg.Cwd, err)
	}
	raw, _, err := scenario.LoadLibrary(name)
	if err != nil {
		return control.Assignment{}, err
	}
	return control.Assignment{
		ScenarioName: name,
		ScenarioJSON: raw,
		FixtureRoot:  h.paths.DataRoot,
	}, nil
}

// onMockReport fans a mock progress report onto the event bus.
func (h *Harness) onMockReport(info control.MockInfo, rep control.Report) {
	h.app.emit("harness:mock", harnessMockEvent{
		MockID:   info.MockID,
		Protocol: info.Registration.Protocol,
		Cwd:      info.Registration.Cwd,
		Scenario: info.Scenario,
		Report:   rep,
	})
}

// HarnessSetScenario installs (or replaces) a scenario assignment rule.
// Mocks that register afterwards get the new scenario; already-running
// mocks keep the script they booted with (restart the session to
// re-register).
func (h *Harness) HarnessSetScenario(spec HarnessScenarioSpec) (harnessScenarioRule, error) {
	var raw json.RawMessage
	var parsed *scenario.Scenario
	var err error
	switch {
	case len(spec.Scenario) > 0:
		parsed, err = scenario.Parse(spec.Scenario)
		if err != nil {
			return harnessScenarioRule{}, err
		}
		raw = spec.Scenario
	case spec.Name != "":
		raw, parsed, err = scenario.LoadLibrary(spec.Name)
		if err != nil {
			return harnessScenarioRule{}, err
		}
	default:
		return harnessScenarioRule{}, fmt.Errorf("set either name (library scenario) or scenario (inline JSON)")
	}

	fixtureRoot := spec.FixtureRoot
	if fixtureRoot == "" {
		fixtureRoot = h.paths.DataRoot
	}
	// Fixture files must exist NOW: the mock resolves them lazily
	// mid-scenario and skips a missing file with only a stderr log — a
	// typo'd path would otherwise surface as a test hanging on frames
	// that never arrive. Same fail-at-set-time contract as the scenario
	// schema validation above.
	for _, p := range parsed.FixturePaths() {
		resolved := p
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(fixtureRoot, p)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return harnessScenarioRule{}, fmt.Errorf("scenario %q: fixture %q not readable at %s: %w", parsed.Name, p, resolved, err)
		}
		if info.IsDir() {
			return harnessScenarioRule{}, fmt.Errorf("scenario %q: fixture %q resolves to a directory (%s), want an NDJSON file", parsed.Name, p, resolved)
		}
	}
	rule := harnessScenarioRule{
		Provider:    parsed.Provider,
		Cwd:         spec.Cwd,
		Name:        parsed.Name,
		FixtureRoot: fixtureRoot,
		scenarioDoc: raw,
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.scenarioRules {
		if h.scenarioRules[i].Provider == rule.Provider && h.scenarioRules[i].Cwd == rule.Cwd {
			h.scenarioRules[i] = rule
			return rule, nil
		}
	}
	h.scenarioRules = append(h.scenarioRules, rule)
	return rule, nil
}

// HarnessClearScenarios drops every assignment rule; subsequent mocks
// fall back to the provider defaults.
func (h *Harness) HarnessClearScenarios() error {
	h.mu.Lock()
	h.scenarioRules = nil
	h.mu.Unlock()
	return nil
}

// HarnessScenariosResult pairs the shipped library with the active
// assignment rules.
type HarnessScenariosResult struct {
	Library []scenario.LibraryEntry `json:"library"`
	Rules   []harnessScenarioRule   `json:"rules"`
}

// HarnessListScenarios reports what a mock could run (library) and what
// it currently would run (rules).
func (h *Harness) HarnessListScenarios() (HarnessScenariosResult, error) {
	library, err := scenario.Library()
	if err != nil {
		return HarnessScenariosResult{}, err
	}
	h.mu.Lock()
	rules := make([]harnessScenarioRule, len(h.scenarioRules))
	copy(rules, h.scenarioRules)
	h.mu.Unlock()
	return HarnessScenariosResult{Library: library, Rules: rules}, nil
}

// HarnessListMocks lists registered mock processes in spawn order.
func (h *Harness) HarnessListMocks() ([]control.MockInfo, error) {
	srv, err := h.controlServer()
	if err != nil {
		return nil, err
	}
	return srv.Mocks(), nil
}

// HarnessMockCommand queues a live command for a running mock:
// advance (release a waitSignal/stall gate), emit (inject wire lines),
// exit (terminate with a code).
func (h *Harness) HarnessMockCommand(mockID string, cmd control.Command) error {
	srv, err := h.controlServer()
	if err != nil {
		return err
	}
	switch cmd.Type {
	case control.CommandAdvance, control.CommandExit:
	case control.CommandEmit:
		if len(cmd.Lines) == 0 {
			return fmt.Errorf("emit command needs lines")
		}
	default:
		return fmt.Errorf("command type %q must be %s, %s, or %s", cmd.Type, control.CommandAdvance, control.CommandEmit, control.CommandExit)
	}
	return srv.Command(mockID, cmd)
}

func (h *Harness) controlServer() (*control.Server, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.control == nil {
		return nil, fmt.Errorf("harness control server is not running")
	}
	return h.control, nil
}
