package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/entityid"
)

func jiraConnection(site, account string, createdAt int64) NewTrackerConnection {
	return NewTrackerConnection{Kind: TrackerKindJira, Site: site, Account: account, Label: "Jira", CreatedAt: createdAt}
}

func mustCreateTrackerConnection(t *testing.T, s *Store, c NewTrackerConnection) TrackerConnection {
	t.Helper()
	created, err := s.CreateTrackerConnection(c)
	if err != nil {
		t.Fatalf("create %s connection to %s: %v", c.Kind, c.Site, err)
	}
	return created
}

func TestTrackerConnectionsCreateReadAndList(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	jira := mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user1@example.com", 20))
	tempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 30})
	linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", Label: "Linear", CreatedAt: 20})

	for _, c := range []TrackerConnection{jira, tempo, linear} {
		if !entityid.Valid(c.ID) || !c.Enabled || c.VerifiedAt != 0 || c.VerifiedAccountID != "" || c.TokenExpiresAt != 0 {
			t.Errorf("created %+v; want a minted ID, enabled and unverified", c)
		}
	}
	if tempo.ParentID != jira.ID || jira.ParentID != "" || jira.Account != "user1@example.com" || linear.Label != "Linear" {
		t.Fatalf("jira %+v, tempo %+v, linear %+v", jira, tempo, linear)
	}
	got, err := s.GetTrackerConnection(tempo.ID)
	if err != nil || got != tempo {
		t.Fatalf("get = %+v, %v; want %+v", got, err, tempo)
	}
	if _, err := s.GetTrackerConnection(entityid.New()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get of a missing connection: %v", err)
	}
	list, err := s.ListTrackerConnections()
	if err != nil {
		t.Fatal(err)
	}
	first := []TrackerConnection{jira, linear}
	slices.SortFunc(first, func(a, b TrackerConnection) int { return strings.Compare(a.ID, b.ID) })
	if want := append(first, tempo); !slices.Equal(list, want) {
		t.Fatalf("list = %+v\nwant %+v (creation time, then ID)", list, want)
	}
}

// Every refusal leaves the table as it was.
func TestCreateTrackerConnectionRefusals(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	jira := mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user1@example.com", 1))
	other := mustCreateTrackerConnection(t, s, jiraConnection("https://other.example.com", "user1@example.com", 1))
	linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 1})
	tempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 1})

	invalid := map[string]NewTrackerConnection{
		"unknown kind":         {Kind: "github", Site: "https://example.com", CreatedAt: 1},
		"empty site":           {Kind: TrackerKindLinear, CreatedAt: 1},
		"jira without account": {Kind: TrackerKindJira, Site: "https://third.example.com", CreatedAt: 1},
		"linear with account":  {Kind: TrackerKindLinear, Site: "workspace-b", Account: "user1@example.com", CreatedAt: 1},
		"tempo with account":   {Kind: TrackerKindTempo, Site: "https://api.us.tempo.io", Account: "user1@example.com", ParentID: other.ID, CreatedAt: 1},
		"tempo without parent": {Kind: TrackerKindTempo, Site: "https://api.us.tempo.io", CreatedAt: 1},
		"jira with parent":     {Kind: TrackerKindJira, Site: "https://third.example.com", Account: "user1@example.com", ParentID: jira.ID, CreatedAt: 1},
		"no creation time":     {Kind: TrackerKindLinear, Site: "workspace-b"},
		"missing parent":       {Kind: TrackerKindTempo, Site: "https://api.us.tempo.io", ParentID: entityid.New(), CreatedAt: 1},
		"parent is linear":     {Kind: TrackerKindTempo, Site: "https://api.us.tempo.io", ParentID: linear.ID, CreatedAt: 1},
		"parent is tempo":      {Kind: TrackerKindTempo, Site: "https://api.us.tempo.io", ParentID: tempo.ID, CreatedAt: 1},
	}
	for name, c := range invalid {
		if _, err := s.CreateTrackerConnection(c); !errors.Is(err, ErrInvalidTrackerConnection) {
			t.Errorf("%s: %v; want ErrInvalidTrackerConnection", name, err)
		}
	}
	duplicate := map[string]NewTrackerConnection{
		"same jira account":      jiraConnection("https://example.atlassian.net", "user1@example.com", 2),
		"same linear workspace":  {Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 2},
		"second tempo of a jira": {Kind: TrackerKindTempo, Site: "https://api.us.tempo.io", ParentID: jira.ID, CreatedAt: 2},
	}
	for name, c := range duplicate {
		if _, err := s.CreateTrackerConnection(c); !errors.Is(err, ErrTrackerConnectionExists) {
			t.Errorf("%s: %v; want ErrTrackerConnectionExists", name, err)
		}
	}
	if list, err := s.ListTrackerConnections(); err != nil || len(list) != 4 {
		t.Fatalf("after refusals: %d connections, %v; want 4", len(list), err)
	}

	// Two accounts on one site, and two Jira sites sharing a Tempo region.
	mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user2@example.com", 3))
	mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: other.ID, CreatedAt: 3})
}

func TestTrackerConnectionWritesReportChanges(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	c := mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user1@example.com", 1))

	expectChange := func(name string, row TrackerConnection, changed bool, err error, want bool) TrackerConnection {
		t.Helper()
		if err != nil || changed != want {
			t.Fatalf("%s: changed %v, %v; want %v", name, changed, err, want)
		}
		if want && row.ID != c.ID {
			t.Fatalf("%s: read back %+v", name, row)
		}
		return row
	}
	row, changed, err := s.SetTrackerConnectionLabel(c.ID, "Work")
	if row = expectChange("label", row, changed, err, true); row.Label != "Work" {
		t.Fatalf("label read back %+v", row)
	}
	row, changed, err = s.SetTrackerConnectionLabel(c.ID, "Work")
	expectChange("same label", row, changed, err, false)

	row, changed, err = s.SetTrackerConnectionEnabled(c.ID, true)
	expectChange("enable an enabled connection", row, changed, err, false)
	row, changed, err = s.SetTrackerConnectionEnabled(c.ID, false)
	if row = expectChange("disable", row, changed, err, true); row.Enabled {
		t.Fatalf("disable read back %+v", row)
	}
	row, changed, err = s.SetTrackerConnectionEnabled(c.ID, false)
	expectChange("disable again", row, changed, err, false)
	row, changed, err = s.SetTrackerConnectionEnabled(c.ID, true)
	expectChange("enable", row, changed, err, true)

	verification := TrackerVerification{AccountID: "acc-user1", Name: "User 1", VerifiedAt: 50, TokenExpiresAt: 900}
	row, changed, err = s.RecordTrackerConnectionVerification(c.ID, verification)
	row = expectChange("verify", row, changed, err, true)
	if row.VerifiedAccountID != "acc-user1" || row.VerifiedName != "User 1" || row.VerifiedAt != 50 || row.TokenExpiresAt != 900 || row.Label != "Work" || !row.Enabled {
		t.Fatalf("verification read back %+v", row)
	}
	row, changed, err = s.RecordTrackerConnectionVerification(c.ID, verification)
	expectChange("same verification", row, changed, err, false)
	verification.VerifiedAt = 60
	row, changed, err = s.RecordTrackerConnectionVerification(c.ID, verification)
	expectChange("verified again later", row, changed, err, true)

	for name, bad := range map[string]TrackerVerification{
		"no time":         {AccountID: "acc-user1", TokenExpiresAt: 1},
		"negative expiry": {AccountID: "acc-user1", VerifiedAt: 1, TokenExpiresAt: -1},
	} {
		if _, _, err := s.RecordTrackerConnectionVerification(c.ID, bad); !errors.Is(err, ErrInvalidTrackerConnection) {
			t.Errorf("%s: %v; want ErrInvalidTrackerConnection", name, err)
		}
	}

	missing := entityid.New()
	if _, _, err := s.SetTrackerConnectionLabel(missing, "x"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("label of a missing connection: %v", err)
	}
	if _, _, err := s.SetTrackerConnectionEnabled(missing, false); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("enable of a missing connection: %v", err)
	}
	if _, _, err := s.RecordTrackerConnectionVerification(missing, verification); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("verification of a missing connection: %v", err)
	}
}

// A Jira connection's delete takes its Tempo connection and every board
// listed through either, and names both; a Tempo connection's delete
// leaves its Jira connection.
func TestDeleteTrackerConnectionTakesItsTempoConnection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	project := mustCreateTrackerProject(t, s, "p")
	jira := mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user1@example.com", 1))
	tempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 1})
	other := mustCreateTrackerConnection(t, s, jiraConnection("https://other.example.com", "user1@example.com", 1))
	otherTempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: other.ID, CreatedAt: 1})
	if _, err := s.ReplaceProjectTrackerBoards(project, []ProjectTrackerBoard{
		{ConnectionID: jira.ID, BoardID: "10", OpensFirst: true},
		{ConnectionID: other.ID, BoardID: "20"},
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.DeleteTrackerConnection(jira.ID)
	if err != nil || !slices.Equal(deleted, []string{jira.ID, tempo.ID}) {
		t.Fatalf("delete = %v, %v; want the connection and its Tempo connection", deleted, err)
	}
	if boards, err := s.ListProjectTrackerBoards(project); err != nil || !slices.Equal(boards, []ProjectTrackerBoard{{ConnectionID: other.ID, BoardID: "20"}}) {
		t.Fatalf("boards after the delete = %+v, %v", boards, err)
	}
	if deleted, err := s.DeleteTrackerConnection(jira.ID); err != nil || deleted != nil {
		t.Fatalf("second delete = %v, %v; want nothing", deleted, err)
	}
	if deleted, err := s.DeleteTrackerConnection(otherTempo.ID); err != nil || !slices.Equal(deleted, []string{otherTempo.ID}) {
		t.Fatalf("Tempo delete = %v, %v", deleted, err)
	}
	if list, err := s.ListTrackerConnections(); err != nil || len(list) != 1 || list[0].ID != other.ID {
		t.Fatalf("left = %+v, %v; want the other Jira connection", list, err)
	}
}

// Concurrent creates of one connection make one row; every other caller
// gets ErrTrackerConnectionExists.
func TestConcurrentCreatesOfOneTrackerConnection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			_, errs[i] = s.CreateTrackerConnection(jiraConnection("https://example.atlassian.net", "user1@example.com", 1))
		})
	}
	wg.Wait()
	created, duplicates := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrTrackerConnectionExists):
			duplicates++
		default:
			t.Errorf("create: %v", err)
		}
	}
	if created != 1 || duplicates != callers-1 {
		t.Fatalf("%d created, %d duplicates; want 1 and %d", created, duplicates, callers-1)
	}
	if list, err := s.ListTrackerConnections(); err != nil || len(list) != 1 {
		t.Fatalf("%d rows, %v; want 1", len(list), err)
	}
}
