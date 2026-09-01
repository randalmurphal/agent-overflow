package app

import (
	"log"
	"runtime"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/wsldistro"
)

// initWSLUpdater resolves process-global boot inputs before crossing into the
// updater package. The WSL backend must be launched by the Windows launcher:
// its injected AppData path is both the feature gate and staging root.
func InitWSLUpdater(a *App, markerDir string) {
	initWSLUpdaterIn(a, a.version, markerDir)
}

func initWSLUpdaterIn(a *App, currentVersion, markerDir string) {
	if currentVersion == "dev" {
		log.Printf("updater: disabled for dev build (version=%q)", currentVersion)
		return
	}
	configDir, ok := wslConfigDir()
	if !ok {
		log.Printf("updater: WSL self-update unavailable — %s is not set, so this backend was not started by the Windows launcher", wsldistro.AppDataEnv)
		return
	}
	if markerDir == "" {
		log.Printf("updater: WSL self-update disabled — no app data dir resolves for the install marker")
		return
	}
	if a.updater == nil {
		log.Printf("updater: WSL self-update disabled — updater service is unavailable")
		return
	}

	if err := a.updater.ConfigureWSL(appupdate.WSLConfig{
		CurrentVersion: currentVersion,
		Arch:           runtime.GOARCH,
		StagingRoot:    configDir,
		MarkerDir:      markerDir,
	}); err != nil {
		log.Printf("updater: init failed: %v — in-app updates disabled", err)
		return
	}
	log.Printf("updater: configured (current version %s, target wsl/%s, staging root %s)", currentVersion, runtime.GOARCH, configDir)
}

// notifyPendingUpdateApplyFailure presents the boot-detected failure only
// after the notification transport has been wired. The same notice remains
// available through CheckForUpdate for the life of the process.
func NotifyPendingUpdateApplyFailure(a *App) {
	if a.updater == nil {
		return
	}
	notice := a.updater.ApplyFailure()
	if notice == "" {
		return
	}
	// A fixed id, because there is exactly one of these per boot and a
	// second would be the same fact restated.
	send := notify.Send{
		ID:     "app-update-apply-failure",
		Kind:   notify.KindAppUpdate,
		Title:  "Update didn't apply",
		Body:   notice,
		Target: notify.Target{Kind: "none", BackendID: a.notificationBackendID()},
	}
	if err := a.notifyOS(send); err != nil {
		log.Printf("updater: could not present the update-apply notice: %v", err)
	}
}
