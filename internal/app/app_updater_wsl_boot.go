package app

import (
	"context"
	"io"
	"log"
	"net/http"
	"runtime"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/wsldistro"
)

// initWSLUpdater resolves process-global boot inputs before crossing into the
// updater package. The WSL backend must be launched by the Windows launcher:
// its injected AppData path is both the feature gate and staging root.
// failure is the unsuccessful update the launcher passed on the argv.
func InitWSLUpdater(a *App, markerDir string, failure appupdate.LauncherFailure) {
	initWSLUpdaterIn(a, a.version, markerDir, failure)
}

func initWSLUpdaterIn(a *App, currentVersion, markerDir string, failure appupdate.LauncherFailure) {
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
		CurrentVersion:  currentVersion,
		Arch:            runtime.GOARCH,
		StagingRoot:     configDir,
		MarkerDir:       markerDir,
		LauncherFailure: failure,
		Provider:        appupdate.Config{GitLab: a.gitlabReleaseClient},
	}); err != nil {
		log.Printf("updater: init failed: %v — in-app updates disabled", err)
		return
	}
	log.Printf("updater: configured (current version %s, staging root %s)", currentVersion, configDir)
}

// gitlabReleaseClient reads the GitLab release feed of a build that updates
// from a GitLab project, through this App's forge API transport and the
// token of the user's glab login for host. The transport is resolved per
// call: the updater is configured before Start builds a.git, and a Core
// without one answers gitops.ErrNoForgeAPI.
func (a *App) gitlabReleaseClient(host string) appupdate.GitLabClient {
	return gitlabReleaseClient{app: a, host: host}
}

type gitlabReleaseClient struct {
	app  *App
	host string
}

func (c gitlabReleaseClient) Stream(ctx context.Context, path string, dst io.Writer, limit int64) error {
	svc := c.app.gitCore().ForgeAPI()
	if svc == nil {
		return gitops.ErrNoForgeAPI
	}
	req := forgeapi.Request{Path: path, Header: http.Header{"Accept": {"*/*"}}}
	// The ctx deadline bounds the call, body included: a launcher download
	// outlasts the transport's default read timeout.
	if deadline, ok := ctx.Deadline(); ok {
		req.Timeout = time.Until(deadline)
	}
	_, err := svc.GitLab(c.host).Stream(ctx, req, dst, limit)
	return err
}

// ReportUnsuccessfulUpdate records the notice for an update from this
// version to `to` that the desktop's update record settled without
// applying. NotifyPendingUpdateApplyFailure presents it.
func ReportUnsuccessfulUpdate(a *App, to, reason string) {
	if a.updater == nil {
		log.Printf("updater: the update to %s did not apply (%s); no updater service carries the notice", to, reason)
		return
	}
	a.updater.ReportUnsuccessfulUpdate(to, reason)
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
		Target: notify.Target{Kind: notify.TargetNone, BackendID: a.notificationBackendID()},
	}
	if err := a.notifyOS(send); err != nil {
		log.Printf("updater: could not present the update-apply notice: %v", err)
	}
}
