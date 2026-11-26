package trackerapp

import (
	"fmt"
	"maps"
	"slices"

	"agent-overflow/internal/store"
)

// A project's statuses as the Status groups and Starting work pages list
// them, read from this computer's copy of its boards and the project's
// held setup, with no tracker request. Each tracker site a listed board of
// the project is on, or the setup regroups statuses of, lists:
//
//   - the statuses of the columns of the site's listed boards, in the
//     picker's order and then column order, each once, with its name and
//     the tracker's own group (StatusView.Group; empty when the mirror
//     does not hold the status);
//   - the statuses the setup regroups that no listed board's columns show
//     (Elsewhere), by ID, so a person can put each back;
//   - the listed boards whose structure the mirror has not read yet
//     (Unread), each board once, whose statuses it cannot list until a
//     client opens them.
//
// The places Starting work sets defaults for (Places) are each tracker
// project a listed board names, in picker order, then each the setup
// holds defaults for: each with the statuses Move the ticket offers it
// and the boards that would add to them once read. A Linear board is one
// team, whose states are its own, so a team offers its boards' statuses;
// a Jira status is the site's, and a Jira board can show several
// projects' tickets, so a Jira project offers every status the project's
// boards on its site show, and Start work checks the ticket's own moves.

// ProjectStatuses is a project's statuses: by tracker site, and by the
// place Starting work sets defaults for.
type ProjectStatuses struct {
	Sites  []SiteStatuses `json:"sites"`
	Places []StartPlace   `json:"places"`
}

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

// StartPlace is a tracker project of a site that Starting work sets
// defaults for: the IDs of the statuses Move the ticket offers it, in
// the site's order (SiteStatuses.Statuses), and the listed boards not
// read yet whose statuses it would also offer.
type StartPlace struct {
	Tracker  string        `json:"tracker"`
	Site     string        `json:"site"`
	Project  string        `json:"project"`
	Statuses []string      `json:"statuses"`
	Unread   []UnreadBoard `json:"unread,omitempty"`
}

// statusSite is SiteStatuses being gathered: the roots of the site's
// connections that list boards, the status IDs in the order shown, and
// each listed board's tracker project with its column statuses, or
// unread.
type statusSite struct {
	out    SiteStatuses
	roots  []int64
	ids    []string
	seen   map[string]bool
	boards []placeBoard
}

// placeBoard is a listed board as the places read it.
type placeBoard struct {
	project  string
	statuses []string
	unread   *UnreadBoard
}

// ProjectStatuses is projectID's statuses by tracker site, in site order
// (tracker, then site), and by place. A setup file that cannot be read
// fails as ProjectSetup does.
func (s *Service) ProjectStatuses(projectID string) (ProjectStatuses, error) {
	if err := s.enter(); err != nil {
		return ProjectStatuses{}, err
	}
	defer s.leave()
	slug, err := s.projectSlug(projectID)
	if err != nil {
		return ProjectStatuses{}, err
	}
	s.setupMu.Lock()
	held, err := s.heldSetupLocked(slug)
	s.setupMu.Unlock()
	if err != nil {
		return ProjectStatuses{}, err
	}
	choices, err := s.boardChoices(projectID)
	if err != nil {
		return ProjectStatuses{}, err
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
		key  siteKey
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
				return ProjectStatuses{}, err
			}
			conn.key = siteKey{row.Kind, row.Site}
			conn.site = siteOf(conn.key)
			conn.root, conn.ok, err = s.store.GetMirrorRoot(choice.Connection)
			if err != nil {
				return ProjectStatuses{}, fmt.Errorf("trackerapp: the mirror of connection %s: %w", choice.Connection, err)
			}
			if conn.ok {
				conn.site.roots = append(conn.site.roots, conn.root)
			}
			connections[choice.Connection] = conn
		}
		var ids []string
		ok := false
		if conn.ok {
			if ids, ok, err = s.store.GetMirrorBoardStatuses(conn.root, choice.Board); err != nil {
				return ProjectStatuses{}, fmt.Errorf("trackerapp: read board %s of connection %s from the mirror: %w", choice.Board, choice.Connection, err)
			}
		}
		if !ok {
			// A board listed through two accounts is one board of the site.
			unread := UnreadBoard{Board: choice.Board, Name: choice.Name}
			if !slices.ContainsFunc(conn.site.out.Unread, func(u UnreadBoard) bool { return u.Board == choice.Board }) {
				conn.site.out.Unread = append(conn.site.out.Unread, unread)
			}
			conn.site.boards = append(conn.site.boards, placeBoard{project: choice.TrackerProject, unread: &unread})
			continue
		}
		conn.site.boards = append(conn.site.boards, placeBoard{project: choice.TrackerProject, statuses: ids})
		for _, id := range ids {
			if !conn.site.seen[id] {
				conn.site.seen[id] = true
				conn.site.ids = append(conn.site.ids, id)
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
	out := ProjectStatuses{Sites: make([]SiteStatuses, 0, len(sites)), Places: []StartPlace{}}
	for _, choice := range choices {
		if isTicketProject(choice.TrackerProject) {
			conn := connections[choice.Connection]
			out.Places = addPlace(out.Places, conn.key, conn.site, choice.TrackerProject)
		}
	}
	for _, entry := range held.view.Setup.StartWork {
		key := siteKey{entry.Tracker, entry.Site}
		out.Places = addPlace(out.Places, key, sites[key], entry.Project)
	}
	for _, key := range slices.SortedFunc(maps.Keys(sites), func(a, b siteKey) int { return compareSites(a.tracker, a.site, b.tracker, b.site) }) {
		site := sites[key]
		elsewhere := regrouped[key]
		names, err := s.statusNames(site.roots, slices.Concat(site.ids, elsewhere))
		if err != nil {
			return ProjectStatuses{}, err
		}
		for _, id := range site.ids {
			site.out.Statuses = append(site.out.Statuses, names.status(id))
		}
		for _, id := range elsewhere {
			site.out.Elsewhere = append(site.out.Elsewhere, names.status(id))
		}
		out.Sites = append(out.Sites, site.out)
	}
	return out, nil
}

// addPlace is places with tracker project project of site key, once: the
// statuses Move the ticket offers it and the boards not read yet that
// would add to them. A Linear team offers its own boards'; a Jira project
// every board's of the site. A site the project lists no board on and
// regroups nothing of (nil) offers nothing.
func addPlace(places []StartPlace, key siteKey, site *statusSite, project string) []StartPlace {
	if slices.ContainsFunc(places, func(p StartPlace) bool { return p.Tracker == key.tracker && p.Site == key.site && p.Project == project }) {
		return places
	}
	place := StartPlace{Tracker: key.tracker, Site: key.site, Project: project, Statuses: []string{}}
	if site == nil {
		return append(places, place)
	}
	seen := map[string]bool{}
	for _, b := range site.boards {
		if key.tracker == store.TrackerKindLinear && b.project != project {
			continue
		}
		if b.unread != nil {
			if !slices.ContainsFunc(place.Unread, func(u UnreadBoard) bool { return u.Board == b.unread.Board }) {
				place.Unread = append(place.Unread, *b.unread)
			}
			continue
		}
		for _, id := range b.statuses {
			seen[id] = true
		}
	}
	for _, id := range site.ids {
		if seen[id] {
			place.Statuses = append(place.Statuses, id)
		}
	}
	return append(places, place)
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
