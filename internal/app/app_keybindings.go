package app

import (
	"runtime"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/keybindings"
)

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
//ao:scope settings:read
//ao:route home
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
//ao:route home
func (a *App) UpdateKeybindings(bindings []keybindings.Keybinding) error {
	svc, err := a.keybindingsService()
	if err != nil {
		return err
	}
	if err := svc.Update(bindings); err != nil {
		return err
	}
	a.announceKeybindingsChange()
	return nil
}

// announceKeybindingsChange re-derives this process's accelerator set and
// tells every connected client the user file moved.
//
// The nudge carries no bindings: every receiver re-reads through
// GetKeybindings, which is also where the user-file read error the result
// carries comes from. Startup calls refreshBrowserAccelerators directly —
// a boot is not a change, and there is nobody listening yet.
func (a *App) announceKeybindingsChange() {
	a.refreshBrowserAccelerators()
	a.emit(eventchan.KeybindingsUpdated, nil)
}

// refreshBrowserAccelerators recomputes the bound-chord set the browser's
// native page views gate key events on, from the effective keybindings.
// Called at startup and after every keybindings write: the set is read per
// keypress on the UI thread, so it is computed here, once, not there.
func (a *App) refreshBrowserAccelerators() {
	svc, err := a.keybindingsService()
	if err != nil {
		return
	}
	set := keybindings.Accelerators(svc.Get().Bindings, runtime.GOOS == "darwin")
	a.browser.accelerators.Store(&set)
	if a.browser.manager != nil {
		a.browser.manager.AcceleratorsChanged()
	}
}

// browserAccelerators is the Manager's read of that set. Nil before the first
// refresh, which the set type treats as "nothing bound".
func (a *App) browserAccelerators() keybindings.AcceleratorSet {
	if set := a.browser.accelerators.Load(); set != nil {
		return *set
	}
	return nil
}

// ResetKeybindings deletes the user file so GetKeybindings returns
// defaults.
//
//ao:scope settings:write
//ao:route home
func (a *App) ResetKeybindings() error {
	svc, err := a.keybindingsService()
	if err != nil {
		return err
	}
	if err := svc.Reset(); err != nil {
		return err
	}
	a.announceKeybindingsChange()
	return nil
}
