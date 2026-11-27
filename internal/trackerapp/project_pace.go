package trackerapp

import (
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// BoardPace is one board a project lists as Settings shows its usual
// pace: the board, its connection's kind and its name, and for a Jira
// board whether this computer has read it (Read), its own estimate unit
// (Estimate, as BoardView.Estimate.Unit), the unit the project counts it
// in (Counts, as BoardView.Counts), whether it plans sprints, and the
// pace sprint planning measures against (BoardView.Pace), nil for a board
// that has finished no sprint. A Linear board has none of these: the
// page's settings are a Jira board's.
type BoardPace struct {
	Connection     string    `json:"connection"`
	ConnectionKind string    `json:"connection_kind"`
	Board          string    `json:"board"`
	Name           string    `json:"name"`
	Read           bool      `json:"read"`
	Estimate       string    `json:"estimate"`
	Counts         string    `json:"counts"`
	PlansSprints   bool      `json:"plans_sprints"`
	Pace           *PaceView `json:"pace,omitempty"`
}

// ProjectPace is every board projectID lists, in picker order, with the
// pace of each Jira board read from the mirror with the project's status
// groups and estimates (StatusGroups), with no tracker request. A setup
// file that cannot be read fails as ProjectSetup does.
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
	// mirror is a connection's site and mirror root, read once for its
	// boards.
	type mirror struct {
		site siteKey
		root int64
		ok   bool
	}
	mirrors := map[string]mirror{}
	out := []BoardPace{}
	for _, choice := range choices {
		bp := BoardPace{Connection: choice.Connection, ConnectionKind: choice.ConnectionKind, Board: choice.Board, Name: choice.Name}
		out = append(out, bp)
		if choice.ConnectionKind != store.TrackerKindJira {
			continue
		}
		m, seen := mirrors[choice.Connection]
		if !seen {
			row, err := s.get(choice.Connection)
			if err != nil {
				return nil, err
			}
			m.site = siteKey{row.Kind, row.Site}
			if m.root, m.ok, err = s.store.GetMirrorRoot(choice.Connection); err != nil {
				return nil, fmt.Errorf("trackerapp: the mirror of connection %s: %w", choice.Connection, err)
			}
			mirrors[choice.Connection] = m
		}
		if !m.ok {
			continue
		}
		groups := held.counting(m.site)
		pace, ok, err := s.store.GetMirrorBoardPace(m.root, choice.Board, groups.regroup)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: read the pace of board %s of connection %s: %w", choice.Board, choice.Connection, err)
		}
		if ok {
			unit := tracker.EstimateUnit(pace.Unit)
			bp.Read, bp.PlansSprints, bp.Pace = true, pace.PlansSprints, paceOf(pace.Finished)
			bp.Estimate, bp.Counts = nameOf(unitNames[:], unit), nameOf(unitNames[:], groups.unit(unit))
			out[len(out)-1] = bp
		}
	}
	return out, nil
}
