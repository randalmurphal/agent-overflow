package app

import (
	"context"

	"agent-overflow/internal/trackerapp"
)

// A project's tracker setup: the per-project Issues settings beyond its
// boards, one file in the project's app directory that trackerapp reads
// and replaces whole (trackerapp/setup.go). These are its bound methods;
// tracker:setup_updated tells every client of this computer that a
// project's setup changed, and a client reads again the setups it holds.

// ProjectTrackerSetup is the project's tracker setup as it is stored, with
// the revision a replace names. A setup file that cannot be read is
// refused with its path and why.
//
//ao:scope tracker:read
func (a *App) ProjectTrackerSetup(ctx context.Context, projectID string) (trackerapp.SetupView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.SetupView{}, err
	}
	out, err := svc.ProjectSetup(projectID)
	return out, trackerError(err)
}

// ProjectTrackerStatuses is the project's statuses by tracker site, as its
// Status groups page lists them: the statuses of its listed boards'
// columns with the tracker's own group, the statuses its setup regroups
// that no listed board shows, and the listed boards this computer has
// not read yet. It reads this computer's copy and makes no tracker
// request.
//
//ao:scope tracker:read
func (a *App) ProjectTrackerStatuses(ctx context.Context, projectID string) ([]trackerapp.SiteStatuses, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.ProjectStatuses(projectID)
	return out, trackerError(err)
}

// SetProjectTrackerSetup replaces the project's tracker setup with setup,
// which was built from the stored setup at revision. When the stored
// setup moved since, nothing is saved and the answer says so with the
// setup as it is stored now. A section a newer version of the app saved
// is kept as it is.
//
//ao:scope tracker:operate
func (a *App) SetProjectTrackerSetup(ctx context.Context, projectID, revision string, setup trackerapp.Setup) (trackerapp.SetupSave, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.SetupSave{}, err
	}
	out, err := svc.SetProjectSetup(projectID, revision, setup)
	return out, trackerError(err)
}
