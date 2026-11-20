package trackerfake

import (
	"encoding/json"
	"fmt"
)

// Fixture is the state a test seeds: Jira sites, each with an optional
// Tempo account, and Linear workspaces. It is plain JSON; a caller that
// decodes one from text should refuse unknown fields, since a typo would
// seed something other than what the test asserts against.
type Fixture struct {
	Jira   []JiraSite        `json:"jira,omitempty"`
	Linear []LinearWorkspace `json:"linear,omitempty"`
}

// seed normalizes every tenant of f, then installs them all, so a refused
// fixture changes nothing. Callers hold mu.
func (e *Engine) seed(f Fixture) error {
	f, err := cloneFixture(f)
	if err != nil {
		return err
	}
	sites := make([]*jiraSite, 0, len(f.Jira))
	seenSite := map[string]bool{}
	for i := range f.Jira {
		s, err := e.newJiraSite(f.Jira[i])
		if err != nil {
			return err
		}
		if seenSite[s.Site] {
			return fmt.Errorf("trackerfake: Jira site %s is seeded twice", s.Site)
		}
		seenSite[s.Site] = true
		sites = append(sites, s)
	}
	spaces := make([]*linearWorkspace, 0, len(f.Linear))
	seenSpace := map[string]bool{}
	for i := range f.Linear {
		w, err := e.newLinearWorkspace(f.Linear[i])
		if err != nil {
			return err
		}
		if seenSpace[w.key()] {
			return fmt.Errorf("trackerfake: Linear workspace %s is seeded twice", w.key())
		}
		seenSpace[w.key()] = true
		spaces = append(spaces, w)
	}
	if err := e.checkTokens(sites, spaces); err != nil {
		return err
	}
	for _, s := range sites {
		e.jira[s.Site] = s
	}
	for _, w := range spaces {
		e.linear[w.key()] = w
	}
	return nil
}

// Snapshot returns every tenant's state as a Fixture, with every token
// left out, so a test can assert what the trackers now hold.
func (e *Engine) Snapshot() Fixture {
	e.mu.Lock()
	defer e.mu.Unlock()
	var f Fixture
	for _, s := range sortedValues(e.jira) {
		snap := s.JiraSite
		snap.Token = ""
		if snap.Tempo != nil {
			t := *snap.Tempo
			t.Token = ""
			snap.Tempo = &t
		}
		f.Jira = append(f.Jira, snap)
	}
	for _, w := range sortedValues(e.linear) {
		snap := w.LinearWorkspace
		snap.APIKey = ""
		f.Linear = append(f.Linear, snap)
	}
	out, err := cloneFixture(f)
	if err != nil {
		panic(fmt.Sprintf("trackerfake: snapshot: %v", err))
	}
	return out
}

// cloneFixture is a deep copy of f that shares no memory with it.
func cloneFixture(f Fixture) (Fixture, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return Fixture{}, fmt.Errorf("trackerfake: encode the fixture: %w", err)
	}
	var out Fixture
	if err := json.Unmarshal(raw, &out); err != nil {
		return Fixture{}, fmt.Errorf("trackerfake: decode the fixture: %w", err)
	}
	return out, nil
}
