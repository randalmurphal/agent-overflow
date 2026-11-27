package trackerapp

import (
	"fmt"

	"agent-overflow/internal/store"
)

// TicketSource is a connection of this computer that holds tickets and is
// turned on: a Jira or Linear connection, never Tempo, which holds none.
// Site is the site as a person names it, Kind the tracker's kind, and
// Account the tracker's ID of the account the connection signed in as at
// its last verification (empty before one). Label is the name the person
// gave the connection in the Issues settings, empty when they gave none,
// and Email a Jira connection's account email, which the settings show
// beside its site: what tells two connections of one site apart.
type TicketSource struct {
	ID      string
	Site    string
	Kind    string
	Account string
	Label   string
	Email   string
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
		out = append(out, TicketSource{ID: row.ID, Site: siteName(row.Site), Kind: row.Kind, Account: row.VerifiedAccountID, Label: row.Label, Email: row.Account})
	}
	return out, nil
}

// KeyedTicket is a ticket key as one site's mirror knows it: the
// connection that reads it there (the site's link reader) and the issue
// when the mirror holds it, empty when it holds only other issues of the
// key's project.
type KeyedTicket struct {
	Connection string
	Issue      string
}

// TicketsOfKey is the tickets key (capitals) names on this computer's
// sites, one per site whose mirror holds its project, with no tracker
// request (store.TicketsByKey): a site whose mirror holds the issue
// first. None is a key no mirror holds a project of.
func (s *Service) TicketsOfKey(key string) ([]KeyedTicket, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	found, err := s.store.TicketsByKey([]string{key}, nil)
	if err != nil {
		return nil, err
	}
	var out []KeyedTicket
	for _, t := range found[key] {
		out = append(out, KeyedTicket{Connection: t.ConnectionID, Issue: t.IssueID})
	}
	return out, nil
}
