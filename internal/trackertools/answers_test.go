package trackertools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/trackerapp"
)

// twoSitesWithENG12 is Jira and Linear each holding an ENG-12, the Linear
// one the only ticket a search for "quartz lantern" finds.
func twoSitesWithENG12() *fakeTracker {
	jira := detail("ENG-12")
	jira.Card.ID, jira.Card.Title = "10012", "Jira's ENG-12"
	linear := detail("ENG-12")
	linear.Card.ID, linear.Card.Title = "uuid-12", "Quartz lantern"
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{jiraSource, linearSource},
		lookup:  map[string]map[string]string{"conn-jira": {"ENG-12": "10012"}, "conn-linear": {"ENG-12": "uuid-12"}},
		details: map[string]trackerapp.DetailView{"conn-jira/10012": jira, "conn-linear/uuid-12": linear},
	}
	f.search = func(connection string, _ trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
		if connection == linearSource.ID {
			return trackerapp.SearchPageView{Cards: []trackerapp.CardView{linear.Card}}, nil
		}
		return trackerapp.SearchPageView{}, nil
	}
	return f
}

// What ticket_find gives for a ticket, ticket_read opens as that ticket,
// though another connection has a ticket with the same key.
func TestAFoundKeyOpensThatTicket(t *testing.T) {
	t.Parallel()
	f := twoSitesWithENG12()
	found, err := call(t, f, "ticket_find", map[string]any{"text": "quartz lantern"})
	if err != nil {
		t.Fatal(err)
	}
	var page struct{ Tickets []struct{ Key, Title string } }
	if err := json.Unmarshal([]byte(found), &page); err != nil || len(page.Tickets) != 1 {
		t.Fatalf("ticket_find = %s, %v", found, err)
	}
	read, err := call(t, f, "ticket_read", map[string]any{"key": page.Tickets[0].Key})
	if err != nil {
		t.Fatal(err)
	}
	var ticket struct{ Key, Title string }
	if err := json.Unmarshal([]byte(read), &ticket); err != nil || ticket.Title != "Quartz lantern" {
		t.Fatalf("ticket_read of the found key %q = %s; want the Linear ticket the search found", page.Tickets[0].Key, read)
	}
	if ticket.Key != page.Tickets[0].Key {
		t.Fatalf("ticket_read answered key %q for the found key %q", ticket.Key, page.Tickets[0].Key)
	}
}

// A read carries the estimate as the ticket screen shows it, with its
// unit, and when the ticket was created, last updated and resolved.
func TestReadCarriesTheEstimateAndTimes(t *testing.T) {
	t.Parallel()
	d := detail("WEB-12")
	three := 3.0
	d.Rows = []trackerapp.TicketRowView{{Board: "1", Estimate: &three, Unit: "points"}}
	d.Card.Created, d.Card.Updated, d.Card.Resolved = 1_764_000_000_000, 1_764_003_600_000, 1_764_007_200_000
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{jiraSource},
		lookup:  map[string]map[string]string{"conn-jira": {"WEB-12": "10012"}},
		details: map[string]trackerapp.DetailView{"conn-jira/10012": d},
	}
	read, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"estimate":"3 points"`, `"created":"2025-11-24T16:00Z"`, `"updated":"2025-11-24T17:00Z"`, `"resolved":"2025-11-24T18:00Z"`} {
		if !strings.Contains(read, want) {
			t.Errorf("ticket_read = %s\nwant it to hold %s", read, want)
		}
	}
}

// longTicket is a ticket with a 200 KiB description and ten 20 KiB
// comments.
func longTicket() trackerapp.DetailView {
	d := detail("WEB-12")
	d.Description = strings.Repeat("Line of the build log that the reporter pasted whole. ", 200<<10/54)
	for i := range 10 {
		d.Comments = append(d.Comments, trackerapp.CommentView{
			ID: fmt.Sprint("c", i), Author: trackerapp.UserView{Name: fmt.Sprint("User ", i+1)}, Created: 1_764_000_000_000 - int64(i)*60_000,
			Body: fmt.Sprintf("Comment %d. ", i) + strings.Repeat("More of what this person said about it. ", 20<<10/40),
		})
	}
	return d
}

// The first read of a ticket with a 200 KiB description and ten 20 KiB
// comments fits the answer budget and carries the card, the head of the
// description and the ten latest comments, each a piece of its body.
func TestALongTicketsFirstAnswerFitsAndShowsEveryPart(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{jiraSource},
		lookup:  map[string]map[string]string{"conn-jira": {"WEB-12": "10012"}},
		details: map[string]trackerapp.DetailView{"conn-jira/10012": longTicket()},
	}
	read, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	if len(read) > 24<<10 {
		t.Fatalf("the first answer is %d bytes, over the budget of %d", len(read), 24<<10)
	}
	var ticket struct {
		Title       string
		Description string
		Comments    []struct{ By, Body string }
	}
	if err := json.Unmarshal([]byte(read), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.Title == "" || len(ticket.Description) < 1<<10 || len(ticket.Comments) != 10 {
		t.Fatalf("the first answer has title %q, %d bytes of description and %d comments", ticket.Title, len(ticket.Description), len(ticket.Comments))
	}
	for i, c := range ticket.Comments {
		if !strings.HasPrefix(c.Body, fmt.Sprintf("Comment %d. ", i)) || c.By != fmt.Sprint("User ", i+1) {
			t.Fatalf("comment %d = %q by %q", i, c.Body[:min(len(c.Body), 40)], c.By)
		}
	}
}
