package app

import (
	"log"
	"slices"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
)

// prRowTickets is, for each of rows, the tickets of this computer's
// trackers that its title and head branch name (trackerapp.TicketKeys),
// in the order they first appear: one store read for every key the rows
// name (store.TicketsByKey), and one of the boards projectID (the project
// the rows list under) lists, whose connections answer a key two sites
// hold: a row shows a key's first ticket. A row that names none has nil. A read that fails leaves every row
// without tickets and is logged: the rows still list, and the next read
// of the repository annotates them again.
func (a *App) prRowTickets(repoKey, projectID string, rows []gitops.PRListRow) [][]store.KeyedTicket {
	named := make([][]string, len(rows))
	var keys []string
	seen := map[string]bool{}
	for i, row := range rows {
		named[i] = trackerapp.TicketKeys(row.Title + " " + row.HeadRefName)
		for _, key := range named[i] {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	if len(keys) == 0 {
		return make([][]store.KeyedTicket, len(rows))
	}
	var prefer []string
	if projectID != "" {
		boards, err := a.store.ListProjectTrackerBoards(projectID)
		if err != nil {
			log.Printf("pr list: the boards of project %s, which pick between two sites' tickets: %v", projectID, err)
		}
		for _, b := range boards {
			if !slices.Contains(prefer, b.ConnectionID) {
				prefer = append(prefer, b.ConnectionID)
			}
		}
	}
	found, err := a.store.TicketsByKey(keys, prefer)
	if err != nil {
		log.Printf("pr list: the tickets the pull requests of %s name: %v", repoKey, err)
		return make([][]store.KeyedTicket, len(rows))
	}
	tickets := make([][]store.KeyedTicket, len(rows))
	for i, keys := range named {
		for _, key := range keys {
			if sites := found[key]; len(sites) > 0 {
				tickets[i] = append(tickets[i], sites[0])
			}
		}
	}
	return tickets
}
