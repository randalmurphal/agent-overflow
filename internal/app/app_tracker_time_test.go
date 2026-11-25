package app

import (
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// A Time screen's methods and a sprint edit reach the fake Jira's own
// worklogs and sprints end to end through the bound methods: a refresh
// reads a worklog logged in Jira, the mirror read answers it with no
// tracker call, and an add, an edit and a delete each show in the next
// mirror read. The refusals carry a sentence that says what to do.
func TestTrackerTimeReachesItsClient(t *testing.T) {
	t.Parallel()
	site := trackerBoardSite()
	now := time.Now().UTC()
	today, earlier := now.Format(time.DateOnly), now.AddDate(0, 0, -2).Format(time.DateOnly)
	weekAgo := now.AddDate(0, 0, -6).Format(time.DateOnly)
	site.Issues[1].Worklogs = []trackerfake.JiraWorklog{{ID: "20001", Author: "acc-user1",
		Started: now.AddDate(0, 0, -2).Truncate(24 * time.Hour).Add(9 * time.Hour), TimeSpent: 7200}}
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: site})
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})

	days := func(what string, read func() (trackerapp.WorklogDays, error)) []trackerapp.WorklogView {
		t.Helper()
		got, err := read()
		if err != nil || got.Failure != nil || got.More {
			t.Fatalf("%s = %+v, %v", what, got, err)
		}
		return got.Worklogs
	}
	mirrored := func(what string) []trackerapp.WorklogView {
		t.Helper()
		return days(what, func() (trackerapp.WorklogDays, error) {
			return f.a.TrackerWorklogs(ctx, f.conn.ID, "acc-user1", weekAgo, today)
		})
	}

	read := days("the refresh", func() (trackerapp.WorklogDays, error) {
		return f.a.RefreshTrackerWorklogs(ctx, f.conn.ID, "acc-user1", weekAgo, today)
	})
	if len(read) != 1 || read[0].ID != "20001" || read[0].Day != earlier || read[0].SpentSeconds != 7200 || read[0].Issue != "10102" {
		t.Fatalf("the refresh read %+v, want Jira's worklog 20001 of %s", read, earlier)
	}
	if got := mirrored("the mirror read"); len(got) != 1 || got[0] != read[0] {
		t.Fatalf("the mirror read %+v, want what the refresh read", got)
	}

	v, err := f.a.AddTrackerWorklog(ctx, f.conn.ID, trackerapp.WorklogRequest{Issue: "10101", Day: today, SpentSeconds: 1800})
	if err != nil || !appliedOnly(v) || len(v.Stale) != 0 {
		t.Fatalf("AddTrackerWorklog = %+v, %v", v, err)
	}
	got := mirrored("after the add")
	if len(got) != 2 || got[1].Day != today || got[1].SpentSeconds != 1800 {
		t.Fatalf("after the add the mirror reads %+v", got)
	}
	added := got[1]
	hour := int64(3600)
	if v, err = f.a.EditTrackerWorklog(ctx, f.conn.ID, trackerapp.WorklogEditRequest{From: added, SpentSeconds: &hour}); err != nil || !appliedOnly(v) {
		t.Fatalf("EditTrackerWorklog = %+v, %v", v, err)
	}
	if v, err = f.a.DeleteTrackerWorklog(ctx, f.conn.ID, trackerapp.WorklogDeleteRequest{From: read[0]}); err != nil || !appliedOnly(v) {
		t.Fatalf("DeleteTrackerWorklog = %+v, %v", v, err)
	}
	if got := mirrored("after the edit and the delete"); len(got) != 1 || got[0].ID != added.ID || got[0].SpentSeconds != 3600 {
		t.Fatalf("after the edit and the delete the mirror reads %+v", got)
	}

	name := "Sprint 1, renamed"
	if v, err = f.a.EditTrackerSprint(ctx, f.conn.ID, trackerapp.SprintEditRequest{Board: "1", Sprint: "1", Name: &name}); err != nil || !appliedOnly(v) {
		t.Fatalf("EditTrackerSprint = %+v, %v", v, err)
	}
	if got := f.e.Snapshot().Jira[0].Sprints[0].Name; got != name {
		t.Fatalf("Jira's sprint is named %q after the edit", got)
	}

	linear, err := f.a.AddTrackerConnection(ctx, NewTrackerConnection{Kind: store.TrackerKindLinear, Site: "example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linear.KeyFilePath, []byte("lin_api_"+strings.Repeat("x", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	monthAgo := now.AddDate(0, 0, -31).Format(time.DateOnly)
	for _, tc := range []struct {
		name, code, message string
		call                func() error
	}{
		{"a refresh of 32 days", "tracker_request_invalid", "nothing was sent: a worklog refresh reads at most 31 days", func() error {
			return ignoreResult(f.a.RefreshTrackerWorklogs(ctx, f.conn.ID, "acc-user1", monthAgo, today))
		}},
		{"a range that ends before it starts", "tracker_request_invalid", "nothing was sent: worklogs are read for an author, from a day to one no earlier", func() error {
			return ignoreResult(f.a.TrackerWorklogs(ctx, f.conn.ID, "acc-user1", today, earlier))
		}},
		// Before anything of the connection runs: the refresh below makes
		// its mirror.
		{"a mirror read of a connection whose time was never read", "tracker_worklogs_not_read",
			"this computer has not read any logged time of this connection yet; refresh it to read it from the tracker", func() error {
				return ignoreResult(f.a.TrackerWorklogs(ctx, linear.ID, "user-1", today, today))
			}},
		{"a refresh of a tracker with no time tracking", "tracker_unsupported", "this tracker has no time tracking", func() error {
			return ignoreResult(f.a.RefreshTrackerWorklogs(ctx, linear.ID, "user-1", today, today))
		}},
	} {
		code, message, ok := errorsx.PublicDetails(tc.call())
		if !ok || code != tc.code || message != tc.message {
			t.Errorf("%s = %q: %q, want %q: %q", tc.name, code, message, tc.code, tc.message)
		}
	}
	// A 31-day refresh is read.
	if _, err := f.a.RefreshTrackerWorklogs(ctx, f.conn.ID, "acc-user1", now.AddDate(0, 0, -30).Format(time.DateOnly), today); err != nil {
		t.Errorf("a 31-day refresh: %v", err)
	}
	// A worklog write on a tracker with no time tracking is a refused part.
	v, err = f.a.DeleteTrackerWorklog(ctx, linear.ID, trackerapp.WorklogDeleteRequest{From: added})
	if err != nil || len(v.Parts) != 1 || v.Parts[0].State != "refused" || v.Parts[0].Refusal == nil ||
		v.Parts[0].Refusal.Reason != "unsupported" || v.Parts[0].Refusal.Message != "this tracker has no time tracking" {
		t.Errorf("a Linear worklog delete = %+v, %v", v, err)
	}
}
