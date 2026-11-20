package trackerapp

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// tickingClock makes the fixture's clock move a millisecond each read, so
// openings and reads have an order.
func (m *mirrorFixture) tickingClock() {
	m.ticking.Store(true)
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
	if m.p.count("Details") != 1 || m.p.count("Cards") != 1 || opened[1] <= opened[0] {
		t.Fatalf("%d detail reads, %d card reads, opened %v", m.p.count("Details"), m.p.count("Cards"), opened)
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
	reads := m.p.count("Details")
	open("40000")
	if _, _, ok := m.storedDetail(t, "40000"); !ok || m.p.count("Details") != reads+1 {
		t.Fatalf("reopened: stored %v, %d detail reads after %d", ok, m.p.count("Details"), reads)
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

// capFixture is a mirror fixture whose board keeps 210 issues, 40000 to
// 40209, whose detail reads answer their card, with 40000's detail
// stored; evict opens 200 others later, which drops 40000's detail, and
// passes what it stored through the change funnel as a read does.
func capFixture(t *testing.T) (m *mirrorFixture, evict func()) {
	t.Helper()
	m = newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
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
	m.openDetail(t, "40000")
	return m, func() { m.publishRaw(m.evictUntold(t)) }
}

// evictUntold opens 200 issues other than 40000 later, as a read stores
// them, which drops 40000's detail, and returns what it stored without
// passing it through the change funnel.
func (m *mirrorFixture) evictUntold(t *testing.T) Changes {
	t.Helper()
	var docs []store.MirrorDetailDoc
	for i := 1; i <= 200; i++ {
		id := fmt.Sprintf("%05d", 40000+i)
		data, err := mirrorDetail(tracker.Detail{Card: card(id, 1_799_000_000_000)})
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, store.MirrorDetailDoc{IssueID: id, Data: data})
	}
	at := m.now().UnixMilli()
	written, err := m.st.StoreMirrorDetails(m.root(), m.sync("").Sweep, docs, at, at)
	if err != nil {
		t.Error(err)
	}
	var ch Changes
	for i, w := range written.Docs {
		if w.Changed {
			ch.Details = append(ch.Details, tracker.IssueID(docs[i].IssueID))
		}
	}
	ch.Dropped = issueIDs(written.Dropped)
	if _, _, ok := m.storedDetail(t, "40000"); ok {
		t.Error("the openings did not drop the detail")
	}
	return ch
}

// publishRaw passes ch through the change funnel as the read that stored
// it would.
func (m *mirrorFixture) publishRaw(ch Changes) {
	m.svc.publish(m.root(), m.svc.gen(m.conn.ID), m.conn.ID, ch, nil, nil)
}

// A detail the cap drops while its card is checked is not marked opened
// in its absence (which would add a detail with no document and drop
// another unreported): it is read again and stored, and the detail that
// store drops is reported.
func TestADetailDroppedDuringItsCheckIsReadAgain(t *testing.T) {
	t.Parallel()
	m, evict := capFixture(t)
	checks := 0
	m.p.updatedMovesWithDetail(func(asked []tracker.IssueID) (tracker.Cards, error) {
		if checks++; checks == 1 {
			evict()
		}
		return cardsOf(asked)
	})
	_, ch := m.openDetail(t, "40000")
	if _, _, ok := m.storedDetail(t, "40000"); !ok || m.p.count("Details") != 2 {
		t.Fatalf("after the check: stored %v, %d detail reads", ok, m.p.count("Details"))
	}
	if !slices.Equal(ch.Dropped, ids("40001")) || !slices.Equal(ch.Details, ids("40000")) {
		t.Fatalf("changes %+v", ch)
	}
}

// heldDetail reports whether the mirror holds a detail row of issue, with
// a document or none: a write that is no opening stores only into one.
func (m *mirrorFixture) heldDetail(t *testing.T, issue string) bool {
	t.Helper()
	w := must[store.MirrorDetailWrite](t)(m.st.StoreMirrorDetail(m.root(), m.sync("").Sweep, issue, 0, m.now().UnixMilli(), []byte(`{}`)))
	return w.Stored
}

// afterDetailRead is a store that runs then after each stored detail read.
type afterDetailRead struct {
	*store.Store
	then func()
}

func (s afterDetailRead) GetMirrorDetail(root int64, issue string) (store.MirrorDetail, bool, error) {
	d, ok, err := s.Store.GetMirrorDetail(root, issue)
	s.then()
	return d, ok, err
}

// The mirror's copy of a detail the cap drops between its read and its
// opening is not mirrored, and its opening adds no detail and drops none.
func TestTheMirrorsCopyDroppedBeforeItsOpeningIsNotMirrored(t *testing.T) {
	t.Parallel()
	m, evict := capFixture(t)
	m.svc.store = afterDetailRead{Store: m.st, then: evict}
	if _, _, err := m.svc.MirroredDetail(m.conn.ID, "40000"); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("the mirror's copy of a dropped detail: %v", err)
	}
	if m.heldDetail(t, "40000") || !m.heldDetail(t, "40001") {
		t.Fatal("the opening added a detail with no document, dropping the least recently opened")
	}
}

// The mirror's copy of a detail another read rewrites between its read and
// its opening is read again, and the answer is the rewritten document: the
// mirror holds the issue's detail throughout.
func TestTheMirrorsCopyRewrittenBeforeItsOpeningIsReadAgain(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := card("10001", 1_799_000_000_000)
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	m.openDetail(t, "10001")
	rewritten := false
	m.svc.store = afterDetailRead{Store: m.st, then: func() {
		if rewritten {
			return
		}
		rewritten = true
		m.pass(time.Minute)
		c.Title = "Rewritten"
		if _, _, err := m.svc.RefreshIssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil {
			t.Error(err)
		}
	}}
	v, _, err := m.svc.MirroredDetail(m.conn.ID, "10001")
	if err != nil || v.Card.Title != "Rewritten" {
		t.Fatalf("the mirror's copy rewritten before its opening: %v, %q", err, v.Card.Title)
	}
}

// A check reads the issue's own card only. A linked issue the mirror keeps
// shows the mirror's card of it, read in the view's one cards lookup; one
// it does not keep shows what the detail's last full read found. Nothing
// rewrites the stored document.
func TestACheckShowsTheLinkedIssuesAsTheMirrorHasThem(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := card("10001", 1_799_000_000_000)
	d := detailOfCard(c)
	d.Links = append(d.Links, tracker.Link{ID: "l2", Type: d.Links[0].Type, Outward: true,
		Issue: tracker.IssueRef{ID: "50000", Key: "WEB-50", Title: "Not kept", Status: "s1"}})
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return d, nil }
	m.openDetail(t, "10001")
	before, _, _ := m.storedDetail(t, "10001")
	link := card("10002", 1_799_000_000_000)
	link.Title, link.Status, link.Updated = "Issue 10002, renamed", "s2", link.Updated.Add(time.Minute)
	must[store.MirrorChanges](t)(m.st.UpsertMirrorCards(m.root(), m.sync("").Sweep, []store.MirrorCard{must[store.MirrorCard](t)(mirrorCard(link))}, store.MirrorCatalog{}, m.now().UnixMilli()))
	var asked [][]tracker.IssueID
	m.p.updatedMovesWithDetail(func(ids []tracker.IssueID) (tracker.Cards, error) {
		asked = append(asked, ids)
		return cardsAnswering(&c)(ids)
	})
	cs := newCountingStore(m.st)
	m.svc.store = cs
	v, ch := m.openDetail(t, "10001")
	if got := v.Links[0].Issue; got.Key != "WEB-2" || got.Title != link.Title || got.Status.ID != "s2" || len(ch.Details) != 0 {
		t.Fatalf("the kept link %+v, changes %+v", got, ch)
	}
	if got := v.Links[1].Issue; got.Key != "WEB-50" || got.Title != "Not kept" {
		t.Fatalf("the link the mirror does not keep %+v", got)
	}
	if !slices.EqualFunc(asked, [][]tracker.IssueID{ids("10001")}, slices.Equal) || m.p.count("Details") != 1 {
		t.Fatalf("cards read %v, %d detail reads", asked, m.p.count("Details"))
	}
	if n := cs.take()["ListMirrorCards"]; n != 1 {
		t.Fatalf("%d cards lookups for the view", n)
	}
	after, stored, _ := m.storedDetail(t, "10001")
	if after.ReadAt != before.ReadAt || !bytes.Equal(after.Data, before.Data) || stored.Links[0].Issue.Title != "Issue 10002" {
		t.Fatalf("the stored detail was rewritten: read %d (was %d), link %+v", after.ReadAt, before.ReadAt, stored.Links[0].Issue)
	}
	if v, _, err := m.svc.MirroredDetail(m.conn.ID, "10001"); err != nil || v.Links[0].Issue.Title != link.Title {
		t.Fatalf("the mirror's copy: %+v, %v", v.Links, err)
	}
}

// A stored detail whose read began more than detailMaxAge ago is read,
// not checked: its moves and field metadata change without its card.
func TestAStoredDetailPastTheAgeBoundIsRead(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := card("10001", 1_799_000_000_000)
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	m.openDetail(t, "10001")
	m.p.updatedMovesWithDetail(cardsAnswering(&c))
	read := m.now()
	at := func(d time.Duration) { m.svc.now = func() time.Time { return read.Add(d) } }
	at(detailMaxAge)
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 1 || m.p.count("Details") != 1 {
		t.Fatalf("at the bound: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Details"))
	}
	at(detailMaxAge + time.Millisecond)
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 1 || m.p.count("Details") != 2 {
		t.Fatalf("past the bound: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Details"))
	}
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 2 || m.p.count("Details") != 2 {
		t.Fatalf("read again: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Details"))
	}
}

// A person's refresh reads the detail with no check; an open after it
// checks again.
func TestARefreshReadsTheDetail(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := card("10001", 1_799_000_000_000)
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	m.p.updatedMovesWithDetail(cardsAnswering(&c))
	m.openDetail(t, "10001")
	if _, _, err := m.svc.RefreshIssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil {
		t.Fatal(err)
	}
	if m.p.count("Cards") != 0 || m.p.count("Details") != 2 {
		t.Fatalf("a refresh: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Details"))
	}
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 1 || m.p.count("Details") != 2 {
		t.Fatalf("an open after it: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Details"))
	}
}

// A stored detail older than the card sync stored since is read with no
// check; one newer than the mirror's card (read after sync) is checked,
// which stores the card.
func TestADetailTheMirrorKnowsIsStaleIsRead(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := card("10001", 1_799_000_000_000)
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	m.p.updatedMovesWithDetail(cardsAnswering(&c))
	m.openDetail(t, "10001")
	c.Updated = c.Updated.Add(time.Minute)
	newer := must[store.MirrorCard](t)(mirrorCard(c))
	must[store.MirrorChanges](t)(m.st.UpsertMirrorCards(m.root(), m.sync("").Sweep, []store.MirrorCard{newer}, store.MirrorCatalog{}, m.now().UnixMilli()))
	if v, _ := m.openDetail(t, "10001"); v.Card.Updated != millis(c.Updated) || m.p.count("Cards") != 0 || m.p.count("Details") != 2 {
		t.Fatalf("a newer mirror card: updated %d, %d card reads, %d detail reads", v.Card.Updated, m.p.count("Cards"), m.p.count("Details"))
	}
	c.Updated = c.Updated.Add(time.Minute)
	if _, _, err := m.svc.RefreshIssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil {
		t.Fatal(err)
	}
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 1 || m.p.count("Details") != 3 {
		t.Fatalf("an older mirror card: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Details"))
	}
}
