package trackerapp

import (
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// maxSupportIssues bounds the issues one ProjectSupport names, the
// mirror's lookup bound.
const maxSupportIssues = 500

// SupportView is what the Time screen shows of logged hours on support
// tickets of one board of a project: which of the issues asked about are
// support tickets there (SiteSupport), the newest snapshot's hours for a
// point (CreditRules.HoursPerPoint), and the unit the project counts the
// board in (BoardView.Counts: points, or none for tickets). Issues is
// empty, with no hours, when no line shows: the project counts no credit,
// the board is not Jira's, the mirror has not read it, or it is estimated
// in time, where the hours already are the estimate. An issue the mirror
// holds no row of on the board is not a support ticket there.
type SupportView struct {
	Issues        []string `json:"issues"`
	HoursPerPoint int      `json:"hours_per_point,omitempty"`
	Counts        string   `json:"counts,omitempty"`
}

// ProjectSupport is which of issues are support tickets of projectID on
// scope's board, which the project must list (ErrNotListed), read from
// the mirror with no tracker request. A setup file that cannot be read
// fails as ProjectSetup does.
func (s *Service) ProjectSupport(projectID string, scope Scope, issues []string) (SupportView, error) {
	none := SupportView{Issues: []string{}}
	if err := scope.boardOnly(); err != nil {
		return none, err
	}
	if len(issues) > maxSupportIssues {
		return none, fmt.Errorf("%w: %d issues, over %d", ErrInvalidRequest, len(issues), maxSupportIssues)
	}
	if err := s.enter(); err != nil {
		return none, err
	}
	defer s.leave()
	slug, listed, err := s.store.ProjectTrackerBoardListed(projectID, scope.Connection, string(scope.Board))
	switch {
	case err != nil:
		return none, fmt.Errorf("trackerapp: read the boards of project %s: %w", projectID, err)
	case !listed:
		return none, fmt.Errorf("%w: project %s, board %s of %s", ErrNotListed, projectID, scope.Board, scope.Connection)
	}
	s.setupMu.Lock()
	held, err := s.heldSetupLocked(slug)
	s.setupMu.Unlock()
	if err != nil {
		return none, err
	}
	credit := held.credit()
	if credit == nil {
		return none, nil
	}
	row, err := s.get(scope.Connection)
	if err != nil {
		return none, err
	}
	root, ok, err := s.store.GetMirrorRoot(scope.Connection)
	if err != nil {
		return none, fmt.Errorf("trackerapp: the mirror of connection %s: %w", scope.Connection, err)
	}
	if !ok {
		return none, nil
	}
	groups := held.counting(siteKey{row.Kind, row.Site})
	of, ok, err := s.store.ListMirrorSupport(root, string(scope.Board), groups.regroup, issues)
	if err != nil {
		return none, fmt.Errorf("trackerapp: read the support tickets of board %s of connection %s: %w", scope.Board, scope.Connection, err)
	}
	unit := tracker.EstimateUnit(of.Unit)
	if !ok || unit == tracker.UnitTime || len(of.Support) == 0 {
		return none, nil
	}
	return SupportView{Issues: of.Support, HoursPerPoint: credit.Rules[len(credit.Rules)-1].HoursPerPoint,
		Counts: nameOf(unitNames[:], groups.unit(unit))}, nil
}
