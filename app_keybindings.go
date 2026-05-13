package main

import "agent-overflow/internal/keybindings"

// keybindingsService returns the lazy-initialized persisted-config
// service. Construction is one-shot — subsequent calls reuse the same
// Service (and its mutex).
func (a *App) keybindingsService() (*keybindings.Service, error) {
	a.keybindingsOnce.Do(func() {
		a.keybindings, a.keybindingsErr = keybindings.New(a.configDir)
	})
	return a.keybindings, a.keybindingsErr
}

// GetKeybindings returns the effective keybindings: defaults with any
// user overrides layered on top.
func (a *App) GetKeybindings() ([]keybindings.Keybinding, error) {
	svc, err := a.keybindingsService()
	if err != nil {
		return nil, err
	}
	return svc.Get()
}

// UpdateKeybindings replaces the user keybindings file with the
// caller's config. See keybindings.Service.Update for the validation
// + cap contract.
func (a *App) UpdateKeybindings(bindings []keybindings.Keybinding) error {
	svc, err := a.keybindingsService()
	if err != nil {
		return err
	}
	return svc.Update(bindings)
}

// ResetKeybindings deletes the user file so GetKeybindings returns
// defaults.
func (a *App) ResetKeybindings() error {
	svc, err := a.keybindingsService()
	if err != nil {
		return err
	}
	return svc.Reset()
}
