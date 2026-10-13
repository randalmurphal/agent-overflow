package app

import (
	"fmt"
	"path/filepath"
	"sync"

	skillsassets "agent-overflow/assets/skills"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/skillsinstall"
)

// The Settings page's Skills section (docs/specs/standing-agents.md, "Skills
// library"): the shipped pstack tree, whether each provider home holds it,
// and Install, Update and Remove per provider. Nothing here runs unasked.

// SkillsLibrary is the shipped tree and what each provider home holds of it.
type SkillsLibrary struct {
	Name    string       `json:"name"`
	Version string       `json:"version"`
	Commit  string       `json:"commit"`
	Skills  []string     `json:"skills"`
	Homes   []SkillsHome `json:"homes"`
}

// SkillsHome is one provider home's install. Error is set when the home
// could not be read; the other fields are then zero.
type SkillsHome struct {
	Provider string `json:"provider"`
	skillsinstall.Status
	Error string `json:"error,omitempty"`
}

var shippedSkills = sync.OnceValues(func() (skillsinstall.Tree, error) {
	return skillsinstall.LoadTree(skillsassets.PStackSkills(), skillsassets.PStackUpstream())
})

// skillsMu serializes the writes, so two clients pressing Install at once
// cannot interleave their copies into one home.
var skillsMu sync.Mutex

var skillsProviders = []string{string(provider.Claude), string(provider.Codex)}

// skillsHome is the provider home both providers look for `skills/` under:
// `~/.claude` and `~/.codex`, resolved through the one provider-home seam.
func (a *App) skillsHome(providerName string) (string, error) {
	home, err := a.providerHome()
	if err != nil {
		return "", err
	}
	switch providerName {
	case string(provider.Claude):
		return filepath.Join(home, ".claude"), nil
	case string(provider.Codex):
		return filepath.Join(home, ".codex"), nil
	}
	return "", fmt.Errorf("skills: unknown provider %q", providerName)
}

// GetSkillsLibrary returns the shipped skills tree with its upstream version
// and whether each provider home holds it.
//
//ao:scope settings:read
//ao:route selected
func (a *App) GetSkillsLibrary() (SkillsLibrary, error) {
	tree, err := shippedSkills()
	if err != nil {
		return SkillsLibrary{}, err
	}
	library := SkillsLibrary{
		Name: "pstack", Version: tree.Version, Commit: tree.Commit, Skills: tree.Skills,
		Homes: make([]SkillsHome, 0, len(skillsProviders)),
	}
	for _, name := range skillsProviders {
		row := SkillsHome{Provider: name}
		home, err := a.skillsHome(name)
		if err == nil {
			row.Status, err = skillsinstall.GetStatus(home, tree)
		}
		if err != nil {
			row.Error = err.Error()
		}
		library.Homes = append(library.Homes, row)
	}
	return library, nil
}

// InstallSkills copies the shipped skills into a provider home's `skills/`.
// Refused, with nothing written, when a skill directory it would write is
// already there and Agent Overflow did not install it.
//
//ao:scope settings:write
//ao:route selected
func (a *App) InstallSkills(providerName string) error {
	return a.changeSkills(providerName, skillsinstall.Install)
}

// UpdateSkills replaces an install with the shipped skills. Refused for a
// home with no install.
//
//ao:scope settings:write
//ao:route selected
func (a *App) UpdateSkills(providerName string) error {
	return a.changeSkills(providerName, skillsinstall.Update)
}

// RemoveSkills deletes what the install wrote into a provider home, and
// nothing else.
//
//ao:scope settings:write
//ao:route selected
func (a *App) RemoveSkills(providerName string) error {
	return a.changeSkills(providerName, func(home string, _ skillsinstall.Tree) error {
		return skillsinstall.Remove(home)
	})
}

func (a *App) changeSkills(providerName string, change func(string, skillsinstall.Tree) error) error {
	home, err := a.skillsHome(providerName)
	if err != nil {
		return err
	}
	tree, err := shippedSkills()
	if err != nil {
		return err
	}
	skillsMu.Lock()
	err = change(home, tree)
	skillsMu.Unlock()
	// A refused change writes nothing, but a failed one may have written
	// part of the tree; either way every page reads the home again.
	a.emit(eventchan.SkillsChanged, nil)
	if providerName == string(provider.Codex) {
		// The composer's Codex skill list is cached; drop it so the next
		// read sees the change. Claude's list comes from each session's
		// init, so its next session reports the change itself.
		a.codexAppService().ResetSkills()
	}
	return err
}
