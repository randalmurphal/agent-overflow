package app

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/trackerapp"
)

// The time tools follow the Time settings of the calling thread's
// project (trackerapp.TimeSettings), end to end on the fake Jira site:
// time_read names the meetings and time-off tickets; a draft on one that
// Jira no longer finds is refused with what to do; with Never log to an
// epic on, a draft on an epic is refused from the lookup the draft makes
// anyway, with no other request; and settings a newer version saved fail
// the read with their sentence.
func TestTheTimeToolsFollowTheProjectsTimeSettings(t *testing.T) {
	t.Parallel()
	site := trackerBoardSite()
	day := site.Issues[0].Created
	site.Issues = append(site.Issues, trackerfake.JiraIssue{ID: "10109", Key: "WEB-9", Project: "WEB", Type: "10000", Summary: "The epic", Status: "10000", Created: day, Updated: day})
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: site})
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	svc, err := f.a.trackerService()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := svc.ProjectSetup(f.project)
	if err != nil {
		t.Fatal(err)
	}
	setup := stored.Setup
	setup.Time = &trackerapp.TimeSettings{
		Meetings: &trackerapp.TimeTicket{Tracker: "jira", Site: f.conn.Site, Key: "WEB-404"},
		TimeOff:  &trackerapp.TimeTicket{Tracker: "jira", Site: f.conn.Site, Key: "WEB-2"},
	}
	if save, err := svc.SetProjectSetup(f.project, stored.Revision, setup); err != nil || !save.Saved {
		t.Fatalf("saving the Time settings = %+v, %v", save, err)
	}
	_, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	today := time.Now().Format(time.DateOnly)

	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "time_read", map[string]any{"from": today, "to": today})
	if isError || !strings.HasSuffix(text, `"meetings_ticket":"WEB-404","time_off_ticket":"WEB-2"}`) {
		t.Fatalf("time_read = %s (error %v)", text, isError)
	}

	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "time_draft", map[string]any{"entries": []map[string]any{{"day": today, "key": "WEB-404", "hours": 1, "reason": "Standup"}}})
	if want := "[tracker_not_found] WEB-404, this project's meetings ticket in Settings > Issues > Time, cannot be found on example.atlassian.net, or this connection cannot see it. " +
		"Leave out key to ask the user which ticket, and tell them to choose another meetings ticket there."; !isError || text != want {
		t.Fatalf("a draft on the gone meetings ticket = %s (error %v)\nwant %s", text, isError, want)
	}

	since := f.e.Requests(0).Requests
	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "time_draft", map[string]any{"entries": []map[string]any{{"day": today, "key": "WEB-9", "hours": 1, "reason": "Planning"}}})
	if want := "[tracker_request_invalid] entries[0].key WEB-9 is an epic, and this project's Time settings keep time off epics; put the hours on a ticket under it, or leave out key and offer tickets under it."; !isError || text != want {
		t.Fatalf("a draft on an epic = %s (error %v)\nwant %s", text, isError, want)
	}
	var routes []string
	for _, r := range f.e.Requests(0).Requests[len(since):] {
		routes = append(routes, r.Route)
	}
	if !slices.Equal(routes, []string{"POST rest/api/3/issue/bulkfetch"}) {
		t.Errorf("the refused epic draft asked Jira %v; want the key's one lookup", routes)
	}
	if entries, err := svc.OpenTimeEntries(f.conn.ID, []string{today}); err != nil || len(entries) != 0 {
		t.Fatalf("the refused drafts left %+v, %v", entries, err)
	}

	// A hand edit, which the save that loses to it holds.
	if err := os.WriteFile(f.setupPath(t), []byte(`{"time": {"version": 2, "rules": ["any"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if save, err := svc.SetProjectSetup(f.project, stored.Revision, trackerapp.Setup{}); err != nil || save.Saved {
		t.Fatalf("a save over the hand edit = %+v, %v", save, err)
	}
	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "time_read", map[string]any{"from": today, "to": today})
	if want := "[tracker_setup_newer] A newer version of Agent Overflow saved this project's Time settings, so this version cannot tell where its time goes; ask the user to update the app."; !isError || text != want {
		t.Fatalf("time_read with a newer time section = %s (error %v)", text, isError)
	}
}
