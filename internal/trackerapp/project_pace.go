package trackerapp

import (
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// BoardPace is one Jira board a project lists as Settings shows its
// usual pace: the board and its name, whether this computer has read it
// (Read), its own estimate unit (Estimate, as BoardView.Estimate.Unit),
// the unit the project counts it in (Counts, as BoardView.Counts),
// whether it plans sprints, and the pace sprint planning measures against
// (BoardView.Pace), nil for a board that has finished no sprint.
type BoardPace struct {
	Connection   string    `json:"connection"`
	Board        string    `json:"board"`
	Name         string    `json:"name"`
	Read         bool      `json:"read"`
	Estimate     string    `json:"estimate"`
	Counts       string    `json:"counts"`
	PlansSprints bool      `json:"plans_sprints"`
	Pace         *PaceView `json:"pace,omitempty"`
}

// ProjectPace is the pace of each Jira board projectID lists, in picker
// order, read from the mirror with the project's status groups and
// estimates (StatusGroups), with no tracker request. A setup file that
// cannot be read fails as ProjectSetup does.
func (s *Service) ProjectPace(projectID string) ([]BoardPace, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	slug, err := s.projectSlug(projectID)
	if err != nil {
		return nil, err
	}
	s.setupMu.Lock()
	held, err := s.heldSetupLocked(slug)
	s.setupMu.Unlock()
	if err != nil {
		return nil, err
	}
	choices, err := s.boardChoices(projectID)
	if err != nil {
		return nil, err
	}
	out := []BoardPace{}
	for _, choice := range choices {
		if choice.ConnectionKind != store.TrackerKindJira {
			continue
		}
		bp := BoardPace{Connection: choice.Connection, Board: choice.Board, Name: choice.Name}
		row, err := s.get(choice.Connection)
		if err != nil {
			return nil, err
		}
		groups := held.counting(siteKey{row.Kind, row.Site})
		root, ok, err := s.store.GetMirrorRoot(choice.Connection)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: the mirror of connection %s: %w", choice.Connection, err)
		}
		var pace store.MirrorBoardPace
		if ok {
			if pace, ok, err = s.store.GetMirrorBoardPace(root, choice.Board, groups.regroup); err != nil {
				return nil, fmt.Errorf("trackerapp: read the pace of board %s of connection %s: %w", choice.Board, choice.Connection, err)
			}
		}
		if ok {
			unit := tracker.EstimateUnit(pace.Unit)
			bp.Read, bp.PlansSprints, bp.Pace = true, pace.PlansSprints, paceOf(pace.Finished)
			bp.Estimate, bp.Counts = nameOf(unitNames[:], unit), nameOf(unitNames[:], groups.unit(unit))
		}
		out = append(out, bp)
	}
	return out, nil
}
