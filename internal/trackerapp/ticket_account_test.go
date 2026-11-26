package trackerapp

import (
	"net/http"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// An open ticket carries its connection's kind and the person's account
// there as the connection's row holds it: the account a verification
// records reaches a ticket opened after it, and one open when it records
// is told, once, as changed; a verification that records the same account
// tells nothing.
func TestAnOpenTicketCarriesItsKindAndTheAccountItsConnectionRecords(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := &scripted{t: t}
	p.detail = func(id tracker.IssueID) (tracker.Detail, error) { return detailOfCard(card(string(id), 1)), nil }
	f.svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) { return p, nil }
	jira := f.addJira()
	f.writeToken(jira, jiraToken)
	f.jira.On("GET", myselfPath, myself(t, jiraToken))
	issues := func() []IssueEvent {
		_, i, _ := f.events.snapshot()
		return i
	}
	account := func(handle string) string {
		t.Helper()
		v, err := f.svc.TicketDetail(handle)
		if err != nil {
			t.Fatal(err)
		}
		return v.Account
	}

	// Hidden handles: their pumps make no read, so every issue event below
	// is a verification's.
	first, err := f.svc.SubscribeTicket(jira.ID, "10001", false, untied)
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != store.TrackerKindJira || first.Account != "" {
		t.Fatalf("before any verification the answer is kind %q account %q", first.Kind, first.Account)
	}

	// The pump exists when the verification records: it is told.
	before := len(issues())
	f.verify(jira.ID)
	got := issues()[before:]
	if len(got) != 1 || got[0].Issue != "10001" || !got[0].Changed {
		t.Fatalf("the verification told %+v", got)
	}
	if a := account(first.ID); a != "acc-user1" {
		t.Fatalf("the open ticket's account after the verification is %q", a)
	}

	// A pump made after the record holds it, and another handle on the
	// first pump is answered it.
	second, err := f.svc.SubscribeTicket(jira.ID, "10002", false, untied)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.SubscribeTicket(jira.ID, "10001", false, untied)
	if err != nil {
		t.Fatal(err)
	}
	if second.Account != "acc-user1" || again.Account != "acc-user1" {
		t.Fatalf("answers after the verification: new pump %q, second handle %q", second.Account, again.Account)
	}

	// The same account again tells nothing.
	before = len(issues())
	f.verify(jira.ID)
	if got := issues()[before:]; len(got) != 0 {
		t.Fatalf("a verification of the same account told %+v", got)
	}

	// Another account reaches both open tickets.
	f.jira.On("GET", myselfPath, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jiratest.Reply(http.StatusOK, `{"accountId":"acc-user2","displayName":"User 2","emailAddress":"user2@example.com","active":true}`)(w, r, nil)
	})
	before = len(issues())
	f.verify(jira.ID)
	told := map[tracker.IssueID]bool{}
	for _, ev := range issues()[before:] {
		told[ev.Issue] = ev.Changed
	}
	if len(told) != 2 || !told["10001"] || !told["10002"] {
		t.Fatalf("a verification of another account told %+v", issues()[before:])
	}
	if a, b := account(first.ID), account(second.ID); a != "acc-user2" || b != "acc-user2" {
		t.Fatalf("accounts after another account was verified: %q, %q", a, b)
	}

	// A Linear connection's ticket names its kind.
	lin := f.addLinear()
	sub, err := f.svc.SubscribeTicket(lin.ID, "10001", false, untied)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Kind != store.TrackerKindLinear {
		t.Fatalf("a Linear ticket's answer names kind %q", sub.Kind)
	}
}
