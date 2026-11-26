package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// PinField is a field a project's setup can pin for a site's tickets, as
// tracker.Provider.Fields lists it: its ID, name and kind (KindRichText
// is a section of the ticket).
type PinField struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// SiteFields is one tracker site's fields a project's setup can pin for
// its tickets (ticket_pins). Err is why they could not be read, nil when
// they were; the app says it in Error.
type SiteFields struct {
	Tracker string     `json:"tracker"`
	Site    string     `json:"site"`
	Fields  []PinField `json:"fields"`
	Error   string     `json:"error,omitempty"`
	Err     error      `json:"-"`
}

// ErrNoSiteConnection is a site the project's setup pins fields of that
// no connection of this computer reaches.
var ErrNoSiteConnection = errors.New("trackerapp: no connection of this computer reaches the site")

// ProjectFields are projectID's fields to pin, by tracker site, in site
// order: every Jira site of a board the project lists, and every one its
// setup pins fields or sections of (Linear's field list is fixed, as
// validateSetup says). A site is read through the first connection to it
// that is on, among those the project's boards name in picker order and
// then the computer's others, so a pin of a site no board lists can be
// seen and taken off. A site that no connection reaches, or whose read
// fails, carries its Err and lists no fields, and the other sites are
// read as usual; only the context's end or a setup that cannot be read
// fails the call. A Jira site's fields are its field catalog, which the
// client caches.
func (s *Service) ProjectFields(ctx context.Context, projectID string) ([]SiteFields, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: a project's fields name the project", ErrProjectNotFound)
	}
	sites, err := s.fieldSites(projectID)
	if err != nil {
		return nil, err
	}
	out := make([]SiteFields, 0, len(sites))
	for _, site := range sites {
		read := SiteFields{Tracker: site.key.tracker, Site: site.key.site, Fields: []PinField{}}
		if site.connection == "" {
			read.Err = fmt.Errorf("%w: %s", ErrNoSiteConnection, site.key.site)
		} else {
			fields, err := ask(s, ctx, site.connection, func(r *run) ([]PinField, error) {
				fs, err := r.p.Fields(r.ctx)
				return pinFields(fs), call(err)
			})
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err != nil {
				read.Err = err
			} else {
				read.Fields = fields
			}
		}
		out = append(out, read)
	}
	return out, nil
}

func pinFields(fs []tracker.Field) []PinField {
	out := make([]PinField, len(fs))
	for i, f := range fs {
		out[i] = PinField{ID: string(f.ID), Name: f.Name, Kind: f.Kind.String()}
	}
	return out
}

// fieldSite is a site ProjectFields reads, and the connection it reads
// it through; none when no connection reaches it.
type fieldSite struct {
	key        siteKey
	connection string
	on         bool
}

// fieldSites are the sites ProjectFields reads for projectID, in site
// order, each with its connection: the first that is on of those the
// project's boards name, then of the computer's others, else the first
// of all, whose read says it is off.
func (s *Service) fieldSites(projectID string) ([]fieldSite, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	choices, err := s.boardChoices(projectID)
	if err != nil {
		return nil, err
	}
	var sites []fieldSite
	at := func(key siteKey) int {
		return slices.IndexFunc(sites, func(f fieldSite) bool { return f.key == key })
	}
	offer := func(key siteKey, connection string, on bool) {
		switch i := at(key); {
		case i < 0:
			sites = append(sites, fieldSite{key, connection, on})
		case sites[i].connection == "" || !sites[i].on && on:
			sites[i] = fieldSite{key, connection, on}
		}
	}
	rows, err := s.store.ListTrackerConnections()
	if err != nil {
		return nil, fmt.Errorf("trackerapp: list connections: %w", err)
	}
	byID := map[string]store.TrackerConnection{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	for _, c := range choices {
		if c.ConnectionKind == store.TrackerKindLinear {
			continue
		}
		if row, ok := byID[c.Connection]; ok {
			offer(siteKey{row.Kind, row.Site}, row.ID, row.Enabled)
		}
	}
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
	for _, pins := range held.view.Setup.TicketPins {
		if key := (siteKey{pins.Tracker, pins.Site}); key.tracker != store.TrackerKindLinear && at(key) < 0 {
			sites = append(sites, fieldSite{key: key})
		}
	}
	// A site no board of the project names a connection for, or names
	// only connections that are off, reads through another of the
	// computer's connections to it.
	for _, row := range rows {
		if row.Kind != store.TrackerKindLinear && at(siteKey{row.Kind, row.Site}) >= 0 {
			offer(siteKey{row.Kind, row.Site}, row.ID, row.Enabled)
		}
	}
	slices.SortFunc(sites, func(a, b fieldSite) int { return compareSites(a.key.tracker, a.key.site, b.key.tracker, b.key.site) })
	return sites, nil
}
