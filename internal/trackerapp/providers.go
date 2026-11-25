package trackerapp

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/linearprovider"
)

// Provider is the tracker of connection id, for a Jira or Linear
// connection that is turned on. A Jira connection's provider logs time
// through its Tempo connection when that one is turned on, and through
// Jira's own worklogs otherwise, as the connections are at this call. It
// fails for an unknown connection, one turned off and a Tempo connection.
// The provider stays usable until its connection, or its Tempo
// connection, stops being used; then its calls fail.
func (s *Service) Provider(ctx context.Context, id string) (tracker.Provider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	row, err := s.get(id)
	if err != nil {
		return nil, err
	}
	p, _, _, err := s.providerLocked(row)
	return p, err
}

// providerLocked is the provider of connection row, with the client of
// the connection itself that its calls are stamped on (begin) and the
// kind of the tracker that answers for its time: store.TrackerKindTempo
// while its Tempo connection is on, empty otherwise. The caller holds
// opMu and read row under it, so row.TimeSourceGen is the time source
// generation the provider was built for (every connection change holds
// opMu).
func (s *Service) providerLocked(row store.TrackerConnection) (tracker.Provider, *clients, string, error) {
	if err := usable(row); err != nil {
		return nil, nil, "", err
	}
	c, err := s.use(row)
	if err != nil {
		return nil, nil, "", err
	}
	var timeClients *clients
	timeBy := ""
	if row.Kind == store.TrackerKindJira {
		timeRow, ok, err := s.tempoOf(row.ID)
		if err != nil {
			return nil, nil, "", err
		}
		if ok && timeRow.Enabled {
			if timeClients, err = s.use(timeRow); err != nil {
				return nil, nil, "", fmt.Errorf("trackerapp: the Tempo connection of %s: %w", row.ID, err)
			}
			timeBy = store.TrackerKindTempo
		}
	}
	p, err := s.makeProvider(row, c, timeClients)
	if err != nil {
		return nil, nil, "", err
	}
	return p, c, timeBy, nil
}

// usable refuses a connection no provider serves: a Tempo connection
// (ErrNoProvider) and one turned off (ErrDisabled).
func usable(row store.TrackerConnection) error {
	switch {
	case row.Kind == store.TrackerKindTempo:
		return fmt.Errorf("%w: %s", ErrNoProvider, row.ID)
	case !row.Enabled:
		return fmt.Errorf("%w: %s", ErrDisabled, row.ID)
	}
	return nil
}

// buildProvider is the provider over c, the client of connection row, and
// for a Jira connection tempo, its Tempo connection's client or nil, and
// window the start of the history window now (Service.window). Each of
// its reads is one credential's view (tracker.OneCredential).
func buildProvider(row store.TrackerConnection, c *clients, tempo *clients, window func() time.Time) (tracker.Provider, error) {
	if row.Kind == store.TrackerKindLinear {
		p, err := linearprovider.New(c.linear)
		if err != nil {
			return nil, err
		}
		return tracker.OneCredential(p), nil
	}
	opts := jiraprovider.Options{Jira: c.jira, Site: row.Site, SprintsFrom: window}
	if tempo != nil {
		opts.Tempo = tempo.tempo
	}
	p, err := jiraprovider.New(opts)
	if err != nil {
		return nil, err
	}
	return tracker.OneCredential(p), nil
}

// tempoOf reads the Tempo connection that belongs to Jira connection id.
func (s *Service) tempoOf(id string) (store.TrackerConnection, bool, error) {
	rows, err := s.store.ListTrackerConnections()
	if err != nil {
		return store.TrackerConnection{}, false, fmt.Errorf("trackerapp: list connections: %w", err)
	}
	for _, row := range rows {
		if row.ParentID == id {
			return row, true, nil
		}
	}
	return store.TrackerConnection{}, false, nil
}

// jiraClient is the client of Jira connection id, for its Tempo
// connection's issue lookups.
func (s *Service) jiraClient(id string) (*jira.Client, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	row, err := s.get(id)
	if err != nil {
		return nil, err
	}
	c, err := s.use(row)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: the Jira connection %s: %w", id, err)
	}
	return c.jira, nil
}
