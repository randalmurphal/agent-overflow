package trackerapp

import (
	"context"
	"fmt"

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

// NoSiteConnectionError is a site no connection of this computer reaches.
type NoSiteConnectionError struct {
	Site string
}

func (e *NoSiteConnectionError) Error() string {
	return "trackerapp: no connection of this computer reaches " + e.Site
}

// SiteFields are the fields a project's setup can pin for the tickets of
// Jira site (ticket_pins), the site's field catalog, which the client
// caches. They are read through the first connection to the site that is
// on; a site whose connections are all off is ErrDisabled, and one no
// connection reaches a *NoSiteConnectionError. Each site is its own read,
// so one that fails or stalls holds up no other. Linear's field list is
// fixed (validateSetup) and is refused.
func (s *Service) SiteFields(ctx context.Context, kind, site string) ([]PinField, error) {
	if kind != store.TrackerKindJira {
		return nil, fmt.Errorf("%w: only a Jira site's fields are pinned, not %s %s's", ErrInvalidRequest, kind, site)
	}
	rows, err := s.store.ListTrackerConnections()
	if err != nil {
		return nil, fmt.Errorf("trackerapp: list connections: %w", err)
	}
	off := ""
	for _, row := range rows {
		if row.Kind != kind || row.Site != site {
			continue
		}
		if !row.Enabled {
			if off == "" {
				off = row.ID
			}
			continue
		}
		return ask(s, ctx, row.ID, func(r *run) ([]PinField, error) {
			fs, err := r.p.Fields(r.ctx)
			return pinFields(fs), call(err)
		})
	}
	if off != "" {
		return nil, fmt.Errorf("%w: %s", ErrDisabled, off)
	}
	return nil, &NoSiteConnectionError{Site: site}
}

func pinFields(fs []tracker.Field) []PinField {
	out := make([]PinField, len(fs))
	for i, f := range fs {
		out[i] = PinField{ID: string(f.ID), Name: f.Name, Kind: f.Kind.String()}
	}
	return out
}
