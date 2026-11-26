package trackerapp

import (
	"context"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
)

// SiteFields is one tracker site's fields a project's setup can pin for
// its tickets (ticket_pins), as tracker.Provider.Fields lists them: each
// with its ID, name and kind and no value; one of kind rich_text is a
// section.
type SiteFields struct {
	Tracker string      `json:"tracker"`
	Site    string      `json:"site"`
	Fields  []FieldView `json:"fields"`
}

// ProjectFields are projectID's fields to pin, by tracker site, in site
// order: every site of a board the project lists whose tickets take
// pinned fields (not Linear, whose field list is fixed, as validateSetup
// says), each read once through the first connection to it in picker
// order that is on, else the first, whose failure fails the call. A Jira
// site's fields are its field catalog, which the client caches.
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
		fields, err := ask(s, ctx, site.connection, func(r *run) ([]FieldView, error) {
			fs, err := r.p.Fields(r.ctx)
			return fieldViews(fs), call(err)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, SiteFields{Tracker: site.key.tracker, Site: site.key.site, Fields: fields})
	}
	return out, nil
}

// fieldSite is a site ProjectFields reads, and the connection it reads
// it through.
type fieldSite struct {
	key        siteKey
	connection string
	on         bool
}

// fieldSites are the sites ProjectFields reads for projectID, in site
// order.
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
	for _, c := range choices {
		if c.ConnectionKind == store.TrackerKindLinear {
			continue
		}
		row, err := s.get(c.Connection)
		if err != nil {
			return nil, err
		}
		key := siteKey{row.Kind, row.Site}
		at := -1
		for i, site := range sites {
			if site.key == key {
				at = i
			}
		}
		switch {
		case at < 0:
			sites = append(sites, fieldSite{key, c.Connection, c.ConnectionEnabled})
		case !sites[at].on && c.ConnectionEnabled:
			sites[at] = fieldSite{key, c.Connection, true}
		}
	}
	slices.SortFunc(sites, func(a, b fieldSite) int { return compareSites(a.key.tracker, a.key.site, b.key.tracker, b.key.site) })
	return sites, nil
}
