package trackerapp

import (
	"fmt"

	"agent-overflow/internal/store"
)

// TicketSource is a connection of this computer that holds tickets and is
// turned on: a Jira or Linear connection, never Tempo, which holds none.
// Site is the site as a person names it.
type TicketSource struct {
	ID   string
	Kind string
	Site string
}

// TicketSources lists the connections a ticket can be found or read
// through, in the order they were made, from the connection rows alone:
// no key file is read and no tracker is asked, so a caller that only
// needs to know whether there is one pays one store read.
func (s *Service) TicketSources() ([]TicketSource, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	rows, err := s.store.ListTrackerConnections()
	if err != nil {
		return nil, fmt.Errorf("trackerapp: list the connections that hold tickets: %w", err)
	}
	var out []TicketSource
	for _, row := range rows {
		if !row.Enabled || (row.Kind != store.TrackerKindJira && row.Kind != store.TrackerKindLinear) {
			continue
		}
		out = append(out, TicketSource{ID: row.ID, Kind: row.Kind, Site: siteName(row.Site)})
	}
	return out, nil
}
