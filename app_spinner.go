package main

import (
	"agent-overflow/internal/spinner"
)

// spinnerService returns the lazy-initialized spinners-directory service.
// Construction is one-shot — subsequent calls reuse the same Service (and
// its mutex), matching themeService and keybindingsService.
func (a *App) spinnerService() (*spinner.Service, error) {
	a.spinnerOnce.Do(func() {
		a.spinner, a.spinnerErr = spinner.New(a.configDir)
	})
	return a.spinner, a.spinnerErr
}

// GetSpinnerFiles returns every custom spinner sprite the user has
// dropped into <configDir>/spinners: the directory it lives in, each
// sprite as its manifest text plus base64 strip bytes, and the reasons
// any sprite could not be read.
//
// One RPC rather than one per sprite because a sprite is a PAIR and the
// frontend cannot render half of one — and because the failures have to
// arrive with the successes, not after them. Go does not parse the
// manifests; see internal/spinner.
//
// The error return covers service construction only (no writable config
// path). Per-sprite problems are Warnings on the result — user-facing
// state, not log entries.
func (a *App) GetSpinnerFiles() (spinner.Files, error) {
	service, err := a.spinnerService()
	if err != nil {
		return spinner.Files{}, err
	}
	return service.Files(), nil
}
