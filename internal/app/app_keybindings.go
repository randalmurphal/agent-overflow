package app

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
// user overrides layered on top, plus the reason the user's override
// file could not be read when that happened.
//
// The error return covers service construction only (no writable config
// path). An unreadable user file is not an error here — it is a field
// of the result the frontend renders, so the bindings survive the
// report; see keybindings.LoadResult.
//
//ao:scope settings:write
func (a *App) GetKeybindings() (keybindings.LoadResult, error) {
	svc, err := a.keybindingsService()
	if err != nil {
		return keybindings.LoadResult{}, err
	}
	return svc.Get(), nil
}

// UpdateKeybindings replaces the user keybindings file with the
// caller's config. See keybindings.Service.Update for the validation
// + cap contract.
//
//ao:scope settings:write
func (a *App) UpdateKeybindings(bindings []keybindings.Keybinding) error {
	svc, err := a.keybindingsService()
	if err != nil {
		return err
	}
	return svc.Update(bindings)
}

// ResetKeybindings deletes the user file so GetKeybindings returns
// defaults.
//
//ao:scope settings:write
func (a *App) ResetKeybindings() error {
	svc, err := a.keybindingsService()
	if err != nil {
		return err
	}
	return svc.Reset()
}
