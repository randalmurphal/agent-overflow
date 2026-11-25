package store

import (
	"testing"
)

// twoAccounts is a store with two Jira connections of one site and their
// mirror roots: low has the lower connection ID, so it is the site's link
// reader while it is on, and high lists boards 10, 11 and 12 with board
// 10's structure stored. low starts turned off, so high is the reader.
func twoAccounts(t *testing.T) (s *Store, low, high TrackerConnection, lowRoot, highRoot int64) {
	t.Helper()
	s = newTestStore(t)
	low = mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user1@example.com", 10))
	high = mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user2@example.com", 20))
	if high.ID < low.ID {
		low, high = high, low
	}
	var err error
	if lowRoot, _, err = s.GetOrCreateMirrorRoot(low.ID); err != nil {
		t.Fatal(err)
	}
	if highRoot, _, err = s.GetOrCreateMirrorRoot(high.ID); err != nil {
		t.Fatal(err)
	}
	listMirrorBoards(t, s, defaultTestProjectID, high.ID, "10", "11", "12")
	mustReplaceStructure(t, s, highRoot, webStructure(), true)
	setConnectionEnabled(t, s, low.ID, false)
	return s, low, high, lowRoot, highRoot
}

func setConnectionEnabled(t *testing.T, s *Store, id string, enabled bool) {
	t.Helper()
	if _, _, err := s.SetTrackerConnectionEnabled(id, enabled); err != nil {
		t.Fatal(err)
	}
}

// threadLink is threadID's link as its row shows it.
func threadLink(t *testing.T, s *Store, threadID string) ThreadTicketLink {
	t.Helper()
	row, err := s.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if row.TicketLink == nil {
		t.Fatalf("thread %s has no link", threadID)
	}
	return *row.TicketLink
}
