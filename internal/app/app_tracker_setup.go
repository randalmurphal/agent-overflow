package app

import (
	"context"

	"agent-overflow/internal/tracker"
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

// ProjectTrackerStatuses is the project's statuses, as its Status groups
// and Starting work pages list them: by tracker site, the statuses of its
// listed boards' columns with the tracker's own group, the statuses its
// setup regroups that no listed board shows, and the listed boards this
// computer has not read yet; and by place Starting work sets defaults
// for, the statuses Move the ticket offers. It reads this computer's copy
// and makes no tracker request.
//
//ao:scope tracker:read
func (a *App) ProjectTrackerStatuses(ctx context.Context, projectID string) (trackerapp.ProjectStatuses, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.ProjectStatuses{}, err
	}
	out, err := svc.ProjectStatuses(projectID)
	return out, trackerError(err)
}

// ProjectTrackerPace is every board the project lists, as its Points and
// credit page shows them, with the usual pace of each Jira board: what
// the board's last finished sprints finished on average, in the unit the
// project counts the board in, as sprint planning measures against it.
// It reads this computer's copy and makes no tracker request.
//
//ao:scope tracker:read
func (a *App) ProjectTrackerPace(ctx context.Context, projectID string) ([]trackerapp.BoardPace, error) {
	svc, err := a.trackerService()
	if err != nil {
		return nil, err
	}
	out, err := svc.ProjectPace(projectID)
	return out, trackerError(err)
}

// ProjectTrackerSupport is which of issues are support tickets of the
// project on board of connectionID, which the project lists, with the hours a
// point counts on them: what the Time screen shows a support ticket's
// logged hours as. Its issues are empty when no such line shows (the
// project counts no credit, the board is not Jira's or is estimated in
// time). It reads this computer's copy and makes no tracker request.
//
//ao:scope tracker:read
func (a *App) ProjectTrackerSupport(ctx context.Context, projectID, connectionID, board string, issues []string) (trackerapp.SupportView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.SupportView{}, err
	}
	out, err := svc.ProjectSupport(projectID, trackerapp.Scope{Connection: connectionID, Board: tracker.BoardID(board)}, issues)
	return out, trackerError(err)
}

// ProjectTrackerSiteFields are the fields the tickets of a Jira site can
// pin, as the project's Fields page lists them for each site its boards
// or its pins name, read from the tracker's field catalog, which the
// computer caches, through an account one of the project's boards is
// listed through when there is one (trackerapp.Service.SiteFields).
// Each site is its own call, so a site that fails or stalls holds up no
// other.
//
//ao:scope tracker:read
func (a *App) ProjectTrackerSiteFields(ctx context.Context, projectID, tracker, site string) ([]trackerapp.PinField, error) {
	return trackerFormRead(a, ctx, "ProjectTrackerSiteFields", "Listing a site's fields", func(ctx context.Context, svc *trackerapp.Service) ([]trackerapp.PinField, error) {
		return svc.SiteFields(ctx, projectID, tracker, site)
	})
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
