package trackerapp

import (
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

// capFixture is a mirror fixture whose board keeps 210 issues, 40000 to
// 40209, whose detail reads answer their card, with 40000's detail
// stored; evict opens 200 others later, which drops 40000's detail.
func capFixture(t *testing.T) (m *mirrorFixture, evict func()) {
	t.Helper()
	m = newMirrorFixture(t)
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
	return m, func() {
		for i := 1; i <= 200; i++ {
			if _, err := m.st.TouchMirrorDetail(m.root(), fmt.Sprintf("%05d", 40000+i), m.now().UnixMilli()); err != nil {
				t.Error(err)
			}
		}
		if _, _, ok := m.storedDetail(t, "40000"); ok {
			t.Error("the openings did not drop the detail")
		}
	}
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
	if _, _, ok := m.storedDetail(t, "40000"); !ok || m.p.count("Detail") != 2 {
		t.Fatalf("after the check: stored %v, %d detail reads", ok, m.p.count("Detail"))
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

// A check reads the cards of the issues the detail links with the issue's,
// in one read: their key, title and status show as read and are stored
// with the detail (its read time kept) when they changed; a linked issue
// the tracker does not return keeps what the detail holds.
func TestACheckShowsTheLinkedIssuesAsTheyAreNow(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := card("10001", 1_799_000_000_000)
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	m.openDetail(t, "10001")
	before, _, _ := m.storedDetail(t, "10001")
	link := card("10002", 1_799_000_000_000)
	link.Title, link.Status = "Issue 10002, renamed", "s2"
	var asked [][]tracker.IssueID
	m.p.updatedMovesWithDetail(func(ids []tracker.IssueID) (tracker.Cards, error) {
		asked = append(asked, ids)
		return tracker.Cards{Cards: []tracker.Card{c, link}}, nil
	})
	v, ch := m.openDetail(t, "10001")
	if got := v.Links[0].Issue; got.Key != "WEB-2" || got.Title != link.Title || got.Status.ID != "s2" || !slices.Equal(ch.Details, ids("10001")) {
		t.Fatalf("the link %+v, changes %+v", got, ch)
	}
	if !slices.EqualFunc(asked, [][]tracker.IssueID{ids("10001", "10002")}, slices.Equal) || m.p.count("Detail") != 1 {
		t.Fatalf("cards read %v, %d detail reads", asked, m.p.count("Detail"))
	}
	after, d, _ := m.storedDetail(t, "10001")
	if after.ReadAt != before.ReadAt || d.Links[0].Issue.Title != link.Title {
		t.Fatalf("the stored detail: read %d (was %d), link %+v", after.ReadAt, before.ReadAt, d.Links[0].Issue)
	}
	if v, _, err := m.svc.MirroredDetail(m.conn.ID, "10001"); err != nil || v.Links[0].Issue.Title != link.Title {
		t.Fatalf("the mirror's copy: %+v, %v", v.Links, err)
	}
	if _, ch := m.openDetail(t, "10001"); len(ch.Details) != 0 {
		t.Fatalf("the same links changed %+v", ch)
	}
	m.p.set(func(p *scripted) { p.cards = cardsAnswering(&c) })
	if v, ch := m.openDetail(t, "10001"); v.Links[0].Issue.Title != link.Title || len(ch.Details) != 0 {
		t.Fatalf("a linked issue not returned: %+v, changes %+v", v.Links[0].Issue, ch)
	}
}

// A detail that links more issues than one cards read takes with its own
// is read, not checked; one fewer is checked in one read.
func TestADetailLinkingMoreThanOneCardsReadIsRead(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := card("10001", 1_799_000_000_000)
	links := readBatch
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
		d := detailOfCard(c)
		d.Links = nil
		for i := range links {
			d.Links = append(d.Links, tracker.Link{ID: tracker.LinkID(fmt.Sprint(i)), Type: tracker.LinkType{ID: "10000", Name: "Blocks"},
				Issue: tracker.IssueRef{ID: tracker.IssueID(fmt.Sprint(20000 + i)), Key: fmt.Sprint("WEB-", 20000+i), Status: "s1"}})
		}
		return d, nil
	}
	m.openDetail(t, "10001")
	var asked []tracker.IssueID
	m.p.updatedMovesWithDetail(func(ids []tracker.IssueID) (tracker.Cards, error) {
		asked = ids
		return cardsAnswering(&c)(ids)
	})
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 0 || m.p.count("Detail") != 2 {
		t.Fatalf("%d links: %d card reads, %d detail reads", readBatch, m.p.count("Cards"), m.p.count("Detail"))
	}
	links = readBatch - 1
	m.openDetail(t, "10001")
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 1 || m.p.count("Detail") != 3 || len(asked) != readBatch {
		t.Fatalf("%d links: %d card reads of %d issues, %d detail reads", links, m.p.count("Cards"), len(asked), m.p.count("Detail"))
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
	if m.p.count("Cards") != 1 || m.p.count("Detail") != 1 {
		t.Fatalf("at the bound: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Detail"))
	}
	at(detailMaxAge + time.Millisecond)
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 1 || m.p.count("Detail") != 2 {
		t.Fatalf("past the bound: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Detail"))
	}
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 2 || m.p.count("Detail") != 2 {
		t.Fatalf("read again: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Detail"))
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
	if m.p.count("Cards") != 0 || m.p.count("Detail") != 2 {
		t.Fatalf("a refresh: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Detail"))
	}
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 1 || m.p.count("Detail") != 2 {
		t.Fatalf("an open after it: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Detail"))
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
	if v, _ := m.openDetail(t, "10001"); v.Card.Updated != millis(c.Updated) || m.p.count("Cards") != 0 || m.p.count("Detail") != 2 {
		t.Fatalf("a newer mirror card: updated %d, %d card reads, %d detail reads", v.Card.Updated, m.p.count("Cards"), m.p.count("Detail"))
	}
	c.Updated = c.Updated.Add(time.Minute)
	if _, _, err := m.svc.RefreshIssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil {
		t.Fatal(err)
	}
	m.openDetail(t, "10001")
	if m.p.count("Cards") != 1 || m.p.count("Detail") != 3 {
		t.Fatalf("an older mirror card: %d card reads, %d detail reads", m.p.count("Cards"), m.p.count("Detail"))
	}
}
