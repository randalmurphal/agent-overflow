package trackerapp

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A site's fields are read through its first connection that is on, one
// read a site: a site whose connections are all off says so, one no
// connection reaches is named, and a Linear site is refused, its fields
// being fixed.
func TestSiteFieldsReadThroughAConnectionThatIsOn(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	notes := tracker.Field{ID: "customfield_10040", Name: "Notes", Kind: tracker.KindRichText}
	m.p.fields = func() ([]tracker.Field, error) { return []tracker.Field{notes}, nil }
	second := m.add(NewConnection{Kind: store.TrackerKindJira, Site: jiraSiteIn, Account: "user2@example.com"})
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := m.svc.SiteFields(t.Context(), m.project(), store.TrackerKindJira, m.conn.Site)
	if err != nil || !slices.Equal(got, []PinField{{ID: "customfield_10040", Name: "Notes", Kind: "rich_text"}}) {
		t.Fatalf("a site with one connection on = %+v, %v", got, err)
	}
	if n := countOf(m.p.callsInOrder(), "Fields"); n != 1 {
		t.Fatalf("%d fields reads, want 1", n)
	}
	if _, err := m.svc.SetEnabled(second.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.SiteFields(t.Context(), m.project(), store.TrackerKindJira, m.conn.Site); !errors.Is(err, ErrDisabled) {
		t.Fatalf("a site whose connections are all off: %v; want ErrDisabled", err)
	}
	var none *NoSiteConnectionError
	if _, err := m.svc.SiteFields(t.Context(), m.project(), store.TrackerKindJira, "https://other.atlassian.net"); !errors.As(err, &none) || none.Site != "https://other.atlassian.net" {
		t.Fatalf("a site no connection reaches: %v", err)
	}
	if _, err := m.svc.SiteFields(t.Context(), m.project(), store.TrackerKindLinear, "example"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a Linear site: %v; want ErrInvalidRequest", err)
	}
	if n := countOf(m.p.callsInOrder(), "Fields"); n != 1 {
		t.Fatalf("%d fields reads, want none past the first", n)
	}
}

// A site's read is its own: one that stalls past its caller's end takes
// no other site's fields with it.
func TestSiteFieldsOfAHealthySiteSurviveAStalledOne(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	other := m.add(NewConnection{Kind: store.TrackerKindJira, Site: "other.atlassian.net", Account: "user1@example.com"})
	healthy := tracker.Field{ID: "customfield_1", Name: "Notes", Kind: tracker.KindText}
	m.p.fields = func() ([]tracker.Field, error) { return []tracker.Field{healthy}, nil }
	// The first site asked stalls.
	stall := true
	m.p.block = func(ctx context.Context, name string) error {
		if name == "Fields" && stall {
			stall = false
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	stalled, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := m.svc.SiteFields(stalled, m.project(), store.TrackerKindJira, other.Site); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the stalled site: %v", err)
	}
	if got, err := m.svc.SiteFields(t.Context(), m.project(), store.TrackerKindJira, m.conn.Site); err != nil || len(got) != 1 {
		t.Fatalf("the healthy site beside it: %+v, %v", got, err)
	}
}

// A site two accounts reach is read through the one the project's boards
// are listed through, whichever was added first, and through the other
// when that one fails: an account whose token stopped working holds up
// no other, and the first failure is the answer when both fail.
func TestSiteFieldsPreferTheProjectsAccountAndPassAFailedOne(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	type account struct {
		fields []tracker.Field
		err    error
		reads  int
	}
	accounts := map[string]*account{}
	f.svc.makeProvider = func(c store.TrackerConnection, _, _ *clients) (tracker.Provider, error) {
		p := &scripted{t: t}
		p.fields = func() ([]tracker.Field, error) {
			a := accounts[c.ID]
			a.reads++
			return a.fields, a.err
		}
		return p, nil
	}
	older := f.addJira()
	newer := f.add(NewConnection{Kind: store.TrackerKindJira, Site: jiraSiteIn, Account: "user2@example.com"})
	accounts[older.ID] = &account{fields: []tracker.Field{{ID: "customfield_1", Name: "Older", Kind: tracker.KindText}}}
	accounts[newer.ID] = &account{fields: []tracker.Field{{ID: "customfield_2", Name: "Newer", Kind: tracker.KindText}}}
	f.listBoards(newer.ID, testBoard)
	project := "boards-" + newer.ID
	read := func() (string, error) {
		t.Helper()
		got, err := f.svc.SiteFields(t.Context(), project, store.TrackerKindJira, older.Site)
		if err != nil || len(got) != 1 {
			return "", err
		}
		return got[0].Name, nil
	}
	if got, err := read(); got != "Newer" || err != nil || accounts[older.ID].reads != 0 {
		t.Fatalf("fields = %q, %v after %d reads through the other account; want the account the project's board is listed through, alone",
			got, err, accounts[older.ID].reads)
	}
	refused := errors.New("the token stopped working")
	accounts[newer.ID].err = refused
	if got, err := read(); got != "Older" || err != nil {
		t.Fatalf("fields = %q, %v; want the other account's when the project's fails", got, err)
	}
	accounts[older.ID].err = errors.New("the site is unreachable")
	if _, err := read(); !errors.Is(err, refused) {
		t.Fatalf("both accounts failing: %v; want the project's account's failure", err)
	}
	// A project that lists no board on the site reads through the site's
	// accounts in the order the computer lists them, and passes one that
	// fails. Two added in one millisecond list in either order.
	rows, err := f.st.ListTrackerConnections()
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, row := range rows {
		order = append(order, row.ID)
	}
	if len(order) != 2 {
		t.Fatalf("connections = %v; want the two accounts", order)
	}
	first, second := accounts[order[0]], accounts[order[1]]
	first.err, second.err = errors.New("the site is unreachable"), nil
	other := func() (string, error) {
		t.Helper()
		got, err := f.svc.SiteFields(t.Context(), "another-project", store.TrackerKindJira, older.Site)
		if err != nil || len(got) != 1 {
			return "", err
		}
		return got[0].ID, nil
	}
	if got, err := other(); err != nil || got != string(second.fields[0].ID) {
		t.Fatalf("with no board listed and the first account failing: %q, %v; want the second account's", got, err)
	}
	first.err = nil
	before := second.reads
	if got, err := other(); err != nil || got != string(first.fields[0].ID) || second.reads != before {
		t.Fatalf("with no board listed: %q, %v after %d reads through the second account; want the first account's, alone", got, err, second.reads-before)
	}
}

func countOf(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}
