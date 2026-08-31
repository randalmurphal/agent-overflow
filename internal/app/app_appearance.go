package app

import (
	"log"

	"agent-overflow/internal/assetwatch"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/spinner"
	"agent-overflow/internal/theme"
)

// themeService returns the lazy-initialized themes-directory service.
// Construction is one-shot — subsequent calls reuse the same Service
// (and its mutex), matching keybindingsService.
func (a *App) themeService() (*theme.Service, error) {
	a.themeOnce.Do(func() {
		a.theme, a.themeErr = theme.New(a.configDir)
	})
	return a.theme, a.themeErr
}

// GetThemeFiles returns everything the frontend needs to resolve a
// theme: the directory it lives in, every readable theme file as raw
// JSON, the current appearance selection, and the reasons any file could
// not be read.
//
// One RPC rather than three because the three answers are only useful
// together — a selection naming a theme whose file failed to load has to
// arrive with the failure, not after it. Go does not parse the theme
// bodies; see internal/theme.
//
// The error return covers service construction only (no writable config
// path). Per-file problems are Warnings on the result — user-facing
// state, not log entries.
//
//ao:scope settings:read
func (a *App) GetThemeFiles() (theme.Files, error) {
	service, err := a.themeService()
	if err != nil {
		return theme.Files{}, err
	}
	return service.Files(), nil
}

// SetAppearance validates and atomically persists the appearance
// selection (mode + one theme id per axis + the frontend's cached
// native-window background).
//
// The write is bracketed by watcher suppression so the file event it
// produces is not reflected back as `theme:changed`: the caller already
// knows what it just wrote, and echoing it would make every appearance
// change cost a redundant round trip. Suppression is re-armed after the
// write because the event arrives asynchronously, after os.Rename has
// already returned.
//
//ao:scope settings:write
func (a *App) SetAppearance(appearance theme.Appearance) error {
	service, err := a.themeService()
	if err != nil {
		return err
	}
	path := service.AppearancePath()
	a.suppressThemeWatch(path)
	defer a.suppressThemeWatch(path)
	return service.SetAppearance(appearance)
}

// SetWindowBackgroundColor paints the native window chrome — the color
// visible while a resize outruns the webview's paint — to match the
// resolved theme.
//
// It is deliberately separate from SetAppearance: the persisted
// windowBackground is a CACHE for the NEXT launch's window construction
// (main_desktop.go reads it before the webview exists), while this call
// is the live application. The frontend does both on a theme change, and
// either can be useful without the other.
//
// A build with no native window (the headless WSL backend, whose window
// lives in the Windows launcher process) has nothing to paint and
// succeeds silently — the request is satisfied as far as this process
// can satisfy it. An invalid color is a real error and is reported.
//
//ao:scope host
func (a *App) SetWindowBackgroundColor(hex string) error {
	red, green, blue, err := theme.ParseHexColor(hex)
	if err != nil {
		return err
	}
	if a.setWindowBackground == nil {
		return nil
	}
	a.setWindowBackground(red, green, blue)
	return nil
}

// startThemeWatcher arms the themes-directory watcher. A watcher that
// cannot start is logged and skipped rather than failing boot: live
// reload is a convenience on top of the RPC, and the app is fully usable
// without it (the frontend still reads themes on demand).
func (a *App) startThemeWatcher(dir string) {
	watcher, err := assetwatch.NewThemeWatcher(dir, func() {
		a.emit(eventchan.ThemeChanged, nil)
	})
	if err != nil {
		log.Printf("theme watcher unavailable: %v", err)
		return
	}
	a.themeWatcher = watcher
}

// suppressThemeWatch marks a path as written by this process. Nil-safe
// so the App's write path does not care whether the watcher started.
func (a *App) suppressThemeWatch(path string) {
	if a.themeWatcher == nil {
		return
	}
	a.themeWatcher.Suppress(path)
}

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
//
//ao:scope settings:read
func (a *App) GetSpinnerFiles() (spinner.Files, error) {
	service, err := a.spinnerService()
	if err != nil {
		return spinner.Files{}, err
	}
	return service.Files(), nil
}

// startSpinnerWatcher arms the spinners-directory watcher. A watcher that
// cannot start is logged and skipped rather than failing boot: live
// reload is a convenience on top of GetSpinnerFiles, and the app is fully
// usable without it (built-in sprites ship with the frontend).
func (a *App) startSpinnerWatcher(dir string) {
	watcher, err := assetwatch.NewSpinnerWatcher(dir, func() {
		a.emit(eventchan.SpinnerChanged, nil)
	})
	if err != nil {
		log.Printf("spinner watcher unavailable: %v", err)
		return
	}
	a.spinnerWatcher = watcher
}
