package trackerapp

import (
	"context"
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

// NoSiteConnectionError is a site no connection of this computer reaches.
type NoSiteConnectionError struct {
	Site string
}

func (e *NoSiteConnectionError) Error() string {
	return "trackerapp: no connection of this computer reaches " + e.Site
}

// SiteFields are the fields projectID's setup can pin for the tickets of
// Jira site (ticket_pins), the site's field catalog, which the client
// caches. Jira lists the fields its account can see, so they are read
// through a connection to the site that is on and that one of the
// project's boards is listed through, then through the site's other
// connections that are on, each in the order they were added: the first
// that answers is the answer, so an account whose token stopped working
// holds up no other, and the first failure is the answer when none does.
// A site whose connections are all off is ErrDisabled, and one no
// connection reaches a *NoSiteConnectionError. Each site is its own read,
// so one that fails or stalls holds up no other. Linear's field list is
// fixed (validateSetup) and is refused.
func (s *Service) SiteFields(ctx context.Context, projectID, kind, site string) ([]PinField, error) {
	if kind != store.TrackerKindJira {
		return nil, fmt.Errorf("%w: only a Jira site's fields are pinned, not %s %s's", ErrInvalidRequest, kind, site)
	}
	rows, err := s.store.ListTrackerConnections()
	if err != nil {
		return nil, fmt.Errorf("trackerapp: list connections: %w", err)
	}
	choices, err := s.boardChoices(projectID)
	if err != nil {
		return nil, err
	}
	var listed, others []string
	off := ""
	for _, row := range rows {
		switch {
		case row.Kind != kind || row.Site != site:
		case !row.Enabled:
			if off == "" {
				off = row.ID
			}
		case slices.ContainsFunc(choices, func(c BoardChoice) bool { return c.Connection == row.ID }):
			listed = append(listed, row.ID)
		default:
			others = append(others, row.ID)
		}
	}
	var failed error
	for _, id := range slices.Concat(listed, others) {
		fields, err := ask(s, ctx, id, func(r *run) ([]PinField, error) {
			fs, err := r.p.Fields(r.ctx)
			return pinFields(fs), call(err)
		})
		if err == nil {
			return fields, nil
		}
		if failed == nil {
			failed = err
		}
		// The caller's own end fails every connection alike.
		if ctx.Err() != nil {
			break
		}
	}
	switch {
	case failed != nil:
		return nil, failed
	case off != "":
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
