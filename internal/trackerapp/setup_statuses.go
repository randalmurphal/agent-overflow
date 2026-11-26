package trackerapp

import (
	"fmt"
	"maps"
	"slices"

	"agent-overflow/internal/store"
)

// A project's statuses as the Status groups page lists them, read from
// this computer's copy of its boards and the project's held setup, with
// no tracker request. Each tracker site a listed board of the project is
// on, or the setup regroups statuses of, lists:
//
//   - the statuses of the columns of the site's listed boards, in the
//     picker's order and then column order, each once, with its name and
//     the tracker's own group (StatusView.Group; empty when the mirror
//     does not hold the status);
//   - the statuses the setup regroups that no listed board's columns show
//     (Elsewhere), by ID, so a person can put each back;
//   - the listed boards whose structure the mirror has not read yet
//     (Unread), whose statuses it cannot list until a client opens them.

// SiteStatuses is one tracker site's statuses for a project.
type SiteStatuses struct {
	Tracker   string        `json:"tracker"`
	Site      string        `json:"site"`
	Statuses  []StatusView  `json:"statuses"`
	Elsewhere []StatusView  `json:"elsewhere,omitempty"`
	Unread    []UnreadBoard `json:"unread,omitempty"`
}

// UnreadBoard is a listed board whose structure the mirror has not read:
// its ID and the name its listing holds (BoardChoice.Name).
type UnreadBoard struct {
	Board string `json:"board"`
	Name  string `json:"name"`
}

// statusSite is SiteStatuses being gathered: the roots of the site's
// connections that list boards, and the status IDs in the order shown.
type statusSite struct {
	out   SiteStatuses
	roots []int64
	ids   []string
	seen  map[string]bool
}

// ProjectStatuses is projectID's statuses by tracker site, in site order
// (tracker, then site). A setup file that cannot be read fails as
// ProjectSetup does.
func (s *Service) ProjectStatuses(projectID string) ([]SiteStatuses, error) {
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
	sites := map[siteKey]*statusSite{}
	siteOf := func(key siteKey) *statusSite {
		site := sites[key]
		if site == nil {
			site = &statusSite{out: SiteStatuses{Tracker: key.tracker, Site: key.site, Statuses: []StatusView{}}, seen: map[string]bool{}}
			sites[key] = site
		}
		return site
	}
	type connectionSite struct {
		site *statusSite
		root int64
		ok   bool
	}
	connections := map[string]connectionSite{}
	for _, choice := range choices {
		conn, known := connections[choice.Connection]
		if !known {
			row, err := s.get(choice.Connection)
			if err != nil {
				return nil, err
			}
			conn.site = siteOf(siteKey{row.Kind, row.Site})
			conn.root, conn.ok, err = s.store.GetMirrorRoot(choice.Connection)
			if err != nil {
				return nil, fmt.Errorf("trackerapp: the mirror of connection %s: %w", choice.Connection, err)
			}
			if conn.ok {
				conn.site.roots = append(conn.site.roots, conn.root)
			}
			connections[choice.Connection] = conn
		}
		unread := UnreadBoard{Board: choice.Board, Name: choice.Name}
		if !conn.ok {
			conn.site.out.Unread = append(conn.site.out.Unread, unread)
			continue
		}
		board, ok, err := s.store.GetMirrorBoard(conn.root, choice.Board)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: read board %s of connection %s from the mirror: %w", choice.Board, choice.Connection, err)
		}
		if !ok {
			conn.site.out.Unread = append(conn.site.out.Unread, unread)
			continue
		}
		for _, col := range board.Columns {
			for _, id := range col.Statuses {
				if !conn.site.seen[id] {
					conn.site.seen[id] = true
					conn.site.ids = append(conn.site.ids, id)
				}
			}
		}
	}
	regrouped := map[siteKey][]string{}
	for _, entry := range held.view.Setup.StatusGroups {
		key := siteKey{entry.Tracker, entry.Site}
		site := siteOf(key)
		for _, id := range slices.Sorted(maps.Keys(entry.Statuses)) {
			if !site.seen[id] {
				regrouped[key] = append(regrouped[key], id)
			}
		}
	}
	out := make([]SiteStatuses, 0, len(sites))
	for _, key := range slices.SortedFunc(maps.Keys(sites), func(a, b siteKey) int { return compareSites(a.tracker, a.site, b.tracker, b.site) }) {
		site := sites[key]
		elsewhere := regrouped[key]
		names, err := s.statusNames(site.roots, slices.Concat(site.ids, elsewhere))
		if err != nil {
			return nil, err
		}
		for _, id := range site.ids {
			site.out.Statuses = append(site.out.Statuses, names.status(id))
		}
		for _, id := range elsewhere {
			site.out.Elsewhere = append(site.out.Elsewhere, names.status(id))
		}
		out = append(out, site.out)
	}
	return out, nil
}

// statusNames names ids from the catalogs of roots, the first root that
// holds a status naming it.
func (s *Service) statusNames(roots []int64, ids []string) (catalogNames, error) {
	names := catalogNamesOf(store.MirrorCatalog{})
	for _, root := range roots {
		var missing []string
		for _, id := range ids {
			if _, ok := names.statuses[id]; !ok {
				missing = append(missing, id)
			}
		}
		for chunk := range slices.Chunk(missing, statusLookupChunk) {
			catalog, err := s.store.GetMirrorCatalog(root, nil, chunk)
			if err != nil {
				return catalogNames{}, fmt.Errorf("trackerapp: read the statuses of mirror %d: %w", root, err)
			}
			names.add(catalog)
		}
	}
	return names, nil
}

// statusLookupChunk is how many statuses one catalog read names, within
// the store's lookup bound.
const statusLookupChunk = 500
