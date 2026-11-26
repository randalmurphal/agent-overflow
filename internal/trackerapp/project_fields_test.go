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
	got, err := m.svc.SiteFields(t.Context(), store.TrackerKindJira, m.conn.Site)
	if err != nil || !slices.Equal(got, []PinField{{ID: "customfield_10040", Name: "Notes", Kind: "rich_text"}}) {
		t.Fatalf("a site with one connection on = %+v, %v", got, err)
	}
	if n := countOf(m.p.callsInOrder(), "Fields"); n != 1 {
		t.Fatalf("%d fields reads, want 1", n)
	}
	if _, err := m.svc.SetEnabled(second.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.SiteFields(t.Context(), store.TrackerKindJira, m.conn.Site); !errors.Is(err, ErrDisabled) {
		t.Fatalf("a site whose connections are all off: %v; want ErrDisabled", err)
	}
	var none *NoSiteConnectionError
	if _, err := m.svc.SiteFields(t.Context(), store.TrackerKindJira, "https://other.atlassian.net"); !errors.As(err, &none) || none.Site != "https://other.atlassian.net" {
		t.Fatalf("a site no connection reaches: %v", err)
	}
	if _, err := m.svc.SiteFields(t.Context(), store.TrackerKindLinear, "example"); !errors.Is(err, ErrInvalidRequest) {
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
	if _, err := m.svc.SiteFields(stalled, store.TrackerKindJira, other.Site); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the stalled site: %v", err)
	}
	if got, err := m.svc.SiteFields(t.Context(), store.TrackerKindJira, m.conn.Site); err != nil || len(got) != 1 {
		t.Fatalf("the healthy site beside it: %+v, %v", got, err)
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
