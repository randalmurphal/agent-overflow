//go:build !nogui

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"reflect"
	"runtime"
	"sync/atomic"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/appimage"
	"agent-overflow/internal/startuppage"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/uiwindow"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// desktopApplyWindow is the helper's window (desktopApplyUI): the loading
// page while the update or the migration runs, and a failure page after
// one that does not end in a launch. It is also the Wails service whose
// RetryMigration the Retry button calls. It holds the desktop's
// single-instance identity, so a launch while it runs shows it.
type desktopApplyWindow struct {
	app     *application.App
	pages   *desktopPages
	applier *desktopApplier
	// running is set while the loading page shows: closing the window then
	// hides it, and the helper continues.
	running atomic.Bool
}

func (w *desktopApplyWindow) loading() {
	w.running.Store(true)
	w.pages.showLoading()
}

func (w *desktopApplyWindow) progress(p startupprogress.Progress) { w.pages.status.SetProgress(p) }

func (w *desktopApplyWindow) fail(page startuppage.Failure) {
	w.running.Store(false)
	w.pages.showFailure(page)
}

func (w *desktopApplyWindow) quit() { w.app.Quit() }

// RetryMigration is bound to the page of a database upgrade the failure
// memory stopped. It runs the upgrade again, once.
func (w *desktopApplyWindow) RetryMigration() error {
	return w.applier.retryMigration(context.Background())
}

// boundDesktopApplyMethod is the name Wails v3 registers the helper
// window's method under: `<pkgPath>.<TypeName>.<MethodName>`. Derived from
// reflect so a rename does not leave the page calling a missing method.
func boundDesktopApplyMethod(method string) string {
	t := reflect.TypeOf((*desktopApplyWindow)(nil)).Elem()
	return fmt.Sprintf("%s.%s.%s", t.PkgPath(), t.Name(), method)
}

// runDesktopApplyWindow shows the helper's window and runs the update or
// the migration behind it. It returns the process exit code.
func runDesktopApplyWindow(flags desktopApplyFlags) int {
	// The relaunch gets the environment this helper was started with.
	relaunchEnv := appimage.Scrub(os.Environ())
	// The pages are this application's own assets. A build without the
	// production tag would load a dev server's page in their place.
	if err := os.Unsetenv("FRONTEND_DEVSERVER_URL"); err != nil {
		log.Printf("updater: %v", err)
	}
	update, err := newDesktopUpdate(log.Printf)
	if err != nil {
		log.Printf("updater: %v", err)
		return 1
	}
	logPath, err := desktopUpdateLogPath()
	if err != nil {
		log.Printf("updater: %v", err)
		return 1
	}
	title := appidentity.AppTitle(nativeSingleInstanceMode())
	window := &desktopApplyWindow{pages: newDesktopPages("Preparing the update")}
	window.running.Store(true)
	window.applier = &desktopApplier{
		flags:       flags,
		update:      update,
		ui:          window,
		logPath:     logPath,
		retryMethod: boundDesktopApplyMethod("RetryMigration"),
		acquireLock: func(ctx context.Context) (*os.File, error) {
			lock, err := waitForBackendInstanceLock(ctx, bootSettingsDir(), supervise.UpdateLockWait)
			if err != nil {
				return nil, err
			}
			heldBackendLock = lock
			return lock.file, nil
		},
		start: func(install string, args []string) error {
			return supervise.StartDesktopApp(install, args, runtime.GOOS, relaunchEnv)
		},
		logf: log.Printf,
	}

	opts := desktopApplicationOptions(title)
	opts.SingleInstance = desktopSingleInstanceOptions(window.pages.attached)
	opts.Services = []application.Service{application.NewService(window)}
	opts.Assets = application.AssetOptions{Handler: window.pages}
	app := application.New(opts)
	window.app = app
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		// At the saved placement, which this window does not save: the
		// placement belongs to the app.
		w, _ := uiwindow.RestoreAndTrack(app, application.WebviewWindowOptions{
			Title:            title,
			Width:            1280,
			Height:           800,
			MinWidth:         800,
			MinHeight:        600,
			BackgroundColour: bootWindowBackgroundColour(),
			URL:              startuppage.LoadingPath,
		}, loadPersistedWindowGeometry(), nil)
		w.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
			if window.running.Load() {
				event.Cancel()
				w.Hide()
			}
		})
		window.pages.attach(w, startuppage.LoadingPath)
		go window.applier.run(context.Background())
	})
	if err := app.Run(); err != nil {
		log.Printf("updater: %v", err)
		return 1
	}
	return 0
}
