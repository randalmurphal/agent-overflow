package trackerapp

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// The ticket sources are the Jira and Linear connections turned on, in
// the order they were made: never Tempo, which holds no ticket, and never
// one turned off.
func TestTicketSourcesAreTheConnectionsThatHoldTickets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if got := must[[]TicketSource](t)(f.svc.TicketSources()); len(got) != 0 {
		t.Fatalf("a computer with no connection has sources %+v", got)
	}
	jira := f.addJira()
	f.pass(time.Millisecond)
	f.addTempo(jira.ID)
	f.pass(time.Millisecond)
	linear := f.addLinear()
	f.pass(time.Millisecond)
	off := f.add(NewConnection{Kind: "jira", Site: "other.atlassian.net", Account: "user2@example.com"})
	if _, err := f.svc.SetEnabled(off.ID, false); err != nil {
		t.Fatal(err)
	}
	want := []TicketSource{
		{ID: jira.ID, Kind: "jira", Site: "example.atlassian.net"},
		{ID: linear.ID, Kind: "linear", Site: "example"},
	}
	if got := must[[]TicketSource](t)(f.svc.TicketSources()); !slices.Equal(got, want) {
		t.Fatalf("sources = %+v\nwant %+v", got, want)
	}
}

// A thread's linked ticket names the connection that reads its site and
// the key the link saw; a thread with no link has none; a link whose
// site's connection is turned off or removed is refused saying which.
func TestLinkedTicketNamesItsReader(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.linkThread("linked", "10005")
	m.thread("unlinked")
	got, ok, err := m.svc.LinkedTicket("linked")
	want := LinkedTicket{Connection: m.conn.ID, Site: "example.atlassian.net", Issue: "10005", Key: "WEB-5"}
	if err != nil || !ok || got != want {
		t.Fatalf("LinkedTicket = %+v, %v, %v; want %+v", got, ok, err, want)
	}
	if got, ok, err := m.svc.LinkedTicket("unlinked"); err != nil || ok {
		t.Fatalf("an unlinked thread's ticket = %+v, %v, %v", got, ok, err)
	}

	refused := func(what, code string) {
		t.Helper()
		var refusal *LinkRefusal
		_, ok, err := m.svc.LinkedTicket("linked")
		if ok || !errors.As(err, &refusal) || refusal.Code != code {
			t.Fatalf("%s: %v, %v; want a refusal %s", what, ok, err, code)
		}
		if !strings.HasSuffix(refusal.Message, "then read this thread's ticket WEB-5 again") {
			t.Fatalf("%s says %q", what, refusal.Message)
		}
	}
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	refused("a connection turned off", LinkConnectionOff)
	if err := m.svc.Remove(m.conn.ID); err != nil {
		t.Fatal(err)
	}
	refused("a connection removed", LinkNoConnection)
}
