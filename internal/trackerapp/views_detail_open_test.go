package trackerapp

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// tickingClock makes the fixture's clock move a millisecond each read, so
// openings and reads have an order.
func (m *mirrorFixture) tickingClock() {
	var n atomic.Int64
	base := m.svc.now()
	m.svc.now = func() time.Time { return base.Add(time.Duration(n.Add(1)) * time.Millisecond) }
}

// storedDetail is issue's stored detail, decoded; ok is false for none.
func (m *mirrorFixture) storedDetail(t *testing.T, issue string) (store.MirrorDetail, tracker.Detail, bool) {
	t.Helper()
	sd, ok, err := m.st.GetMirrorDetail(m.root(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return sd, tracker.Detail{}, false
	}
	d, ok := detailOf(sd)
	if !ok {
		t.Fatalf("the stored detail of %s does not decode", issue)
	}
	return sd, d, true
}

// A first open reads the detail through and stores it, opened when its
// read began; the next open, on a tracker whose update time moves with
// its detail, checks the card and serves the stored detail, opened again.
func TestAFirstOpenStoresTheDetail(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		return tracker.Detail{Card: card(string(id), 1_799_000_000_000)}, nil
	}
	m.p.updatedMovesWithDetail(cardsOf)
	var opened []int64
	for range 2 {
		if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil {
			t.Fatal(err)
		}
		sd, _, ok := m.storedDetail(t, "10001")
		if !ok || sd.ReadAt == 0 || sd.OpenedAt < sd.ReadAt {
			t.Fatalf("stored %v (opened %d, read %d)", ok, sd.OpenedAt, sd.ReadAt)
		}
		opened = append(opened, sd.OpenedAt)
	}
	if m.p.count("Detail") != 1 || m.p.count("Cards") != 1 || opened[1] <= opened[0] {
		t.Fatalf("%d detail reads, %d card reads, opened %v", m.p.count("Detail"), m.p.count("Cards"), opened)
	}
}

// A detail the cap dropped is read through again on its next open and
// stored again.
func TestAReopenAfterEvictionStoresTheDetailAgain(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	var pages [][]tracker.Row
	for p := range 3 {
		var page []tracker.Row
		for i := range 70 {
			id := fmt.Sprintf("%05d", 40000+p*70+i)
			page = append(page, row(id, 1_799_000_000_000, id))
		}
		pages = append(pages, page)
	}
	m.p.changedRows = pagedDelta(t, pages...)
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		return tracker.Detail{Card: card(string(id), 1_799_000_000_000)}, nil
	}
	open := func(issue string) {
		t.Helper()
		if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, tracker.IssueID(issue), Interactive); err != nil {
			t.Fatal(err)
		}
	}
	open("40000")
	for i := 1; i <= 200; i++ {
		open(fmt.Sprintf("%05d", 40000+i))
	}
	if _, _, ok := m.storedDetail(t, "40000"); ok {
		t.Fatal("the least recently opened detail is kept over the cap")
	}
	reads := m.p.count("Detail")
	open("40000")
	if _, _, ok := m.storedDetail(t, "40000"); !ok || m.p.count("Detail") != reads+1 {
		t.Fatalf("reopened: stored %v, %d detail reads after %d", ok, m.p.count("Detail"), reads)
	}
}

// A read through that began before another one stored its detail does
// not replace it: the later read's document stays.
func TestAStaleInFlightDetailReadStoresNothing(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	inside, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		c := card(string(id), 1_799_000_000_000)
		if calls.Add(1) == 1 {
			close(inside)
			<-release
			c.Title = "Stale"
			return tracker.Detail{Card: c}, nil
		}
		c.Title = "Fresh"
		return tracker.Detail{Card: c}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
		done <- err
	}()
	<-inside
	if v, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil || v.Card.Title != "Fresh" {
		t.Fatalf("the later read: %q, %v", v.Card.Title, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, d, ok := m.storedDetail(t, "10001"); !ok || d.Card.Title != "Fresh" {
		t.Fatalf("stored %v: %q", ok, d.Card.Title)
	}
}
