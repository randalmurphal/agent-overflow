package main

import (
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
