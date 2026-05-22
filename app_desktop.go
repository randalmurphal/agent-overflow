//go:build !nogui

// app_desktop.go owns the Wails-specific wiring around the App service.
// Everything in this file is compiled out when the binary is built with
// `-tags nogui` (the WSL backend payload, see cmd/agent-overflow-windows
// and build/windows/Taskfile.yml). Keeping the Wails import isolated
// here is what lets the WSL payload link without libwebkit2gtk-4.1 /
// libgtk-3 / their transitive .so closure.
package main

import (
	"context"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// ServiceStartup is the Wails v3 entry point for the App service. The
// signature is dictated by Wails' interface assertion — Wails registers
// services by reflecting on this exact (ctx, application.ServiceOptions)
// shape, so even though the body never reads `options`, the parameter
// has to stay typed against the Wails package.
//
// All the real work happens in (*App).Start, which is platform-neutral
// and called directly by runHeadless in the WSL backend. ServiceStartup
// only adds the desktop-only dependency wiring (currently the native
// save-file dialog) before delegating.
func (a *App) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	_ = options // Wails-imposed; unused in the body.
	wailsApp := application.Get()
	a.saveDialog = func(filename string) (string, error) {
		return wailsApp.Dialog.SaveFile().SetFilename(filename).PromptForSingleSelection()
	}
	return a.Start(ctx)
}
