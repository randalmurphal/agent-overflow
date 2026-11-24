package trackerapp

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

func detailOfCard(c tracker.Card) tracker.Detail {
	return tracker.Detail{
		Card: c, Description: "Body",
		Fields:   []tracker.Field{{ID: "customfield_10001", Name: "Team", Kind: tracker.KindOption, Editable: true, Value: tracker.OptionValue(tracker.Choice{ID: "o1", Name: "Web"})}},
		Comments: tracker.CommentPage{Comments: []tracker.Comment{{ID: "c1", Author: "acc-user2", Body: "Looks good", Created: c.Created}}, Next: "p2"},
		Links: []tracker.Link{{ID: "l1", Type: tracker.LinkType{ID: "10000", Name: "Blocks", Outward: "blocks", Inward: "is blocked by"},
			Issue: tracker.IssueRef{ID: "10002", Key: "WEB-2", Title: "Issue 10002", Status: "s1"}}},
		History: []tracker.Event{{At: c.Created, Author: "acc-user1", Field: "status", From: "To Do", To: "In Progress"}, {At: c.Created, Field: "resolution"}},
		Moves:   []tracker.Move{{ID: "11", Name: "Done", To: "s3"}},
		Catalog: tracker.Catalog{Users: []tracker.User{user2}},
	}
}

// statusBegan is when the tests' status histories have an issue enter
// its status (a history read's StatusChange into s1).
var statusBegan = time.UnixMilli(1_798_500_000_000).UTC()

// enteredAtStatusBegan is d with its history's latest status change, into
// its card's status at statusBegan, as a document of an issue whose
// history the tests read holds it; with no HistoryTo, the history is not
// final, so the read's result is provisional (store MirrorStatusSince):
// unknown unless the card's update is within the settle margin after
// statusBegan.
func enteredAtStatusBegan(d tracker.Detail) tracker.Detail {
	d.StatusChanged = tracker.StatusChange{Change: tracker.Change{At: statusBegan}, From: "s2", To: d.Card.Status}
	return d
}

// openDetail opens issue's detail through IssueDetail and fails on an
// error.
func (m *mirrorFixture) openDetail(t *testing.T, issue tracker.IssueID) (DetailView, Changes) {
	t.Helper()
	v, ch, err := m.svc.IssueDetail(t.Context(), m.conn.ID, issue, Interactive)
	if err != nil {
		t.Fatal(err)
	}
	return v, ch
}

// cardsAnswering answers a card read with *c for the issues asked that it
// is.
// answerCardsFromDetails makes p's cards read answer each issue's card as
// its detail read would show it now: a tracker whose two reads agree.
func answerCardsFromDetails(p *scripted) {
	p.set(func(p *scripted) {
		p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
			p.mu.Lock()
			detail := p.detail
			p.mu.Unlock()
			var out tracker.Cards
			for _, id := range asked {
				d, err := detail(id)
				if err != nil {
					return tracker.Cards{}, err
				}
				out.Cards = append(out.Cards, d.Card)
			}
			return out, nil
		}
	})
}

func cardsAnswering(c *tracker.Card) func([]tracker.IssueID) (tracker.Cards, error) {
	return func(asked []tracker.IssueID) (tracker.Cards, error) {
		var out tracker.Cards
		if slices.Contains(asked, c.ID) {
			out.Cards = append(out.Cards, *c)
		}
		return out, nil
	}
}

// On a tracker whose update time moves with its detail, a stored detail is
// checked with one card read: served when the card's update time is the
// stored detail's, read again when it moved, the mirror's card refreshed
// by the check either way.
func TestIssueDetail(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := card("10001", 1_799_000_000_000)
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	m.p.updatedMovesWithDetail(cardsAnswering(&c))
	v, ch, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ch.Details, ids("10001")) || v.Description != "Body" || v.MoreComments != "p2" ||
		v.Comments[0].Author.Name != "User 2" || v.Card.Assignee.Name != "User 1" ||
		v.Links[0].Relation != "is blocked by" || v.Links[0].Issue.Status.Name != "To Do" ||
		v.History[0].Author.Name != "User 1" || v.History[1].Author != nil || v.Moves[0].To.Group != "done" ||
		v.Fields[0].Value.Kind != "option" || v.Fields[0].Value.Choices[0].Name != "Web" {
		t.Fatalf("detail %+v, changes %+v", v, ch)
	}
	if m.p.count("Cards") != 0 {
		t.Fatal("a detail the mirror did not hold was checked with a card read")
	}
	// The card did not move: the stored detail, no detail read.
	v, ch, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if err != nil || m.p.count("Details") != 1 || m.p.count("Cards") != 1 || len(ch.Details) != 0 || v.Description != "Body" {
		t.Fatalf("an unchanged card: %d detail reads, %d card reads, changes %+v, %v", m.p.count("Details"), m.p.count("Cards"), ch, err)
	}
	// The card moved: the detail is read again, ten minutes later (within
	// detailMaxAge), which marks it opened then, and the mirror's card is
	// the one the check read. Only the card changed: the body the read
	// stored is the same bytes, now of the new version.
	later := m.now().Add(10 * time.Minute)
	m.svc.now = func() time.Time { return later }
	c.Updated, c.Title = c.Updated.Add(500*time.Millisecond), "Renamed"
	v, ch, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if err != nil || m.p.count("Details") != 2 || m.p.count("Cards") != 2 || v.Card.Title != "Renamed" ||
		!slices.Equal(ch.Details, ids("10001")) || !slices.Equal(ch.Cards, ids("10001")) {
		t.Fatalf("a moved card: %d detail reads, %d card reads, %q, changes %+v, %v", m.p.count("Details"), m.p.count("Cards"), v.Card.Title, ch, err)
	}
	if held := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); held.Card.Updated != millis(c.Updated) || held.Card.Title != "Renamed" {
		t.Fatalf("the mirror's card %+v", held.Card)
	}
	// The opening began at later, the check's read a millisecond after it
	// and the detail's read one more on the frozen clock (store
	// BeginMirrorRead); the opening is recorded as the read that served it.
	if d, ok, err := m.st.GetMirrorBody(m.root(), "10001"); err != nil || !ok || d.OpenedAt != later.UnixMilli()+2 || d.ReadAt != later.UnixMilli()+2 {
		t.Fatalf("the detail read again: opened %d, read %d, want %d: %v", d.OpenedAt, d.ReadAt, later.UnixMilli()+2, err)
	}
	// A card the tracker does not return is no check: the detail is read.
	m.p.set(func(p *scripted) {
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) { return tracker.Cards{}, nil }
	})
	if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil || m.p.count("Details") != 3 {
		t.Fatalf("a card not returned: %d detail reads, %v", m.p.count("Details"), err)
	}
	// An issue the mirror does not keep is read every time, never stored and
	// never checked.
	other := card("10009", 1_799_000_000_000)
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(other), nil }
	})
	for range 2 {
		if _, ch, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10009", Interactive); err != nil || !ch.Empty() {
			t.Fatalf("an unkept issue: %+v, %v", ch, err)
		}
	}
	if m.p.count("Details") != 5 || m.p.count("Cards") != 3 {
		t.Fatalf("%d detail reads, %d card reads", m.p.count("Details"), m.p.count("Cards"))
	}
	if _, _, err := m.svc.MirroredDetail(m.conn.ID, "10009"); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("the mirror's copy of an unkept issue: %v", err)
	}
}

// On a tracker whose update time does not move with its detail (Linear: a
// comment leaves it), every open reads the detail: a comment added since
// the last open shows though the card did not move, and an identical
// document changes nothing.
func TestADetailIsReadWhereItsUpdateTimeDoesNotMove(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	d := detailOfCard(card("10001", 1_799_000_000_000))
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return d, nil }
	open := func() (DetailView, Changes) { t.Helper(); return m.openDetail(t, "10001") }
	if _, ch := open(); !slices.Equal(ch.Details, ids("10001")) {
		t.Fatalf("the first open changed %+v", ch)
	}
	if _, ch := open(); !ch.Empty() {
		t.Fatalf("an identical document changed %+v", ch)
	}
	d.Comments.Comments = append([]tracker.Comment{{ID: "c2", Author: "acc-user1", Body: "A new comment", Created: d.Card.Updated}}, d.Comments.Comments...)
	v, ch := open()
	if len(v.Comments) != 2 || v.Comments[0].Body != "A new comment" || !slices.Equal(ch.Details, ids("10001")) {
		t.Fatalf("after a comment: %d comments, changes %+v", len(v.Comments), ch)
	}
	if m.p.count("Details") != 3 || m.p.count("Cards") != 0 {
		t.Fatalf("%d detail reads, %d card reads", m.p.count("Details"), m.p.count("Cards"))
	}
	if v, _, err := m.svc.MirroredDetail(m.conn.ID, "10001"); err != nil || len(v.Comments) != 2 {
		t.Fatalf("the mirror's copy: %d comments, %v", len(v.Comments), err)
	}
}

func TestIssueDetailFailure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
		return tracker.Detail{}, &trackerapi.StatusError{Tracker: "jira", Status: 500}
	}
	_, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if _, ok := errors.AsType[*trackerapi.StatusError](err); !ok || m.problem() != FailureTracker {
		t.Fatalf("a failed detail read: %v, problem %q", err, m.problem())
	}
	// An issue the tracker leaves out is that issue's failure, not the
	// connection's: the read succeeded.
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			return tracker.Detail{}, &trackerapi.StatusError{Tracker: "jira", Status: 404}
		}
	})
	_, _, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
	if !errors.Is(err, ErrIssueNotFound) || m.problem() != "" {
		t.Fatalf("an issue left out: %v, problem %q", err, m.problem())
	}
	// The tracker answering with another issue is refused, not stored.
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) {
			return detailOfCard(card("10002", 1_799_000_000_000)), nil
		}
	})
	if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err == nil {
		t.Fatal("a detail of another issue was accepted")
	}
	if _, ok, _ := m.st.GetMirrorBody(m.root(), "10002"); ok {
		t.Fatal("the other issue's detail was stored")
	}
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); !errors.Is(err, ErrDisabled) {
		t.Fatalf("a detail of a connection turned off: %v", err)
	}
}

// A check or a read that fails leaves the stored detail as it was and
// returns the tracker's failure, settled as the connection's problem; the
// mirror's copy still reads.
func TestAFailedCheckKeepsTheStoredDetail(t *testing.T) {
	t.Parallel()
	for name, moved := range map[string]bool{"the card read": false, "the detail read after a moved card": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.seedBoard(t)
			c := card("10001", 1_799_000_000_000)
			m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
			m.p.updatedMovesWithDetail(cardsAnswering(&c))
			m.openDetail(t, "10001")
			before, _, _ := m.storedDetail(t, "10001")
			unavailable := &trackerapi.StatusError{Tracker: "jira", Status: 503}
			m.p.set(func(p *scripted) {
				if moved {
					c.Updated = c.Updated.Add(time.Second)
					p.detail = func(tracker.IssueID) (tracker.Detail, error) { return tracker.Detail{}, unavailable }
					return
				}
				p.cards = func([]tracker.IssueID) (tracker.Cards, error) { return tracker.Cards{}, unavailable }
			})
			_, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
			if got, ok := errors.AsType[*trackerapi.StatusError](err); !ok || got.Status != 503 || m.problem() == "" {
				t.Fatalf("a failed check: %v, problem %q", err, m.problem())
			}
			after, _, ok := m.storedDetail(t, "10001")
			if !ok || after.ReadAt != before.ReadAt || !bytes.Equal(after.Data, before.Data) {
				t.Fatalf("the stored detail changed: read %d, was %d", after.ReadAt, before.ReadAt)
			}
			v, readAt, err := m.svc.MirroredDetail(m.conn.ID, "10001")
			if err != nil || readAt != before.Wall || v.Description != "Body" {
				t.Fatalf("the mirror's copy: read %d, %q, %v", readAt, v.Description, err)
			}
		})
	}
}

// A connection turned off while its tracker answers a check or a read
// ends the read: nothing is stored and the call fails wrapping
// ErrDisabled.
func TestATurnOffDuringADetailReadEndsIt(t *testing.T) {
	t.Parallel()
	for _, call := range []string{"Cards", "Details"} {
		t.Run(call, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.seedBoard(t)
			c := card("10001", 1_799_000_000_000)
			off := func() {
				if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
					t.Error(err)
				}
			}
			m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
				if call == "Details" {
					off()
				}
				return detailOfCard(c), nil
			}
			if call == "Cards" {
				m.openDetail(t, "10001")
				m.p.updatedMovesWithDetail(func(asked []tracker.IssueID) (tracker.Cards, error) {
					off()
					return cardsAnswering(&c)(asked)
				})
			}
			reads := m.p.count("Details")
			if _, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); !errors.Is(err, ErrDisabled) {
				t.Fatalf("a read the turn off overtook: %v", err)
			}
			if call == "Cards" && m.p.count("Details") != reads {
				t.Fatal("the detail was read after the check's write was refused")
			}
			if m.root() != 0 {
				t.Fatal("the connection turned off kept a mirror")
			}
		})
	}
}

// The mirror's copy of a detail builds no client and makes no call, marks
// the detail opened, and is not mirrored when the mirror holds no detail
// of the issue.
func TestMirroredDetailBuildsNoClient(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		return tracker.Detail{Card: card("10001", 1_799_000_000_000)}, nil
	}
	m.openDetail(t, "10001")
	later := m.now().Add(time.Hour)
	svc := must[*Service](t)(New(testCallbacks(Deps{
		Store: m.st, Changed: func() {}, CredentialsDir: m.creds, Origin: APIOrigin, Version: "1", Now: func() time.Time { return later }, Life: t.Context(),
	}, newEventLog())))
	svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) {
		t.Error("a provider was built for the mirror's copy of a detail")
		return m.p, nil
	}
	stored, _, _ := m.storedDetail(t, "10001")
	v, readAt, err := svc.MirroredDetail(m.conn.ID, "10001")
	if err != nil || v.Card.ID != "10001" || readAt != stored.Wall {
		t.Fatalf("the mirror's copy %+v read at %d (stored %d): %v", v.Card, readAt, stored.Wall, err)
	}
	if opened, _, _ := m.storedDetail(t, "10001"); opened.OpenedAt != later.UnixMilli() {
		t.Fatalf("the mirror's copy left the detail opened at %d", opened.OpenedAt)
	}
	if _, _, err := svc.MirroredDetail(m.conn.ID, "10002"); !errors.Is(err, ErrNotMirrored) {
		t.Fatalf("a kept issue with no detail: %v", err)
	}
	if _, _, err := svc.MirroredDetail(m.conn.ID, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no issue: %v", err)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if len(svc.clients) != 0 || m.p.count("Details") != 1 {
		t.Fatalf("%d clients, %d detail reads", len(svc.clients), m.p.count("Details"))
	}
}

// A detail keeps its newest maxDetailEvents events and stays complete
// from its HistoryFrom.
func TestDetailHistoryIsBounded(t *testing.T) {
	t.Parallel()
	base := time.UnixMilli(1_799_000_000_000).UTC()
	var d tracker.Detail
	for i := range maxDetailEvents + 100 {
		// Pairs of events share a time; the newest come first, as a tracker
		// may list them.
		d.History = append(d.History, tracker.Event{At: base.Add(time.Duration((maxDetailEvents+100-i)/2) * time.Minute), Field: fmt.Sprint(i)})
	}
	all := slices.Clone(d.History)
	boundHistory(&d)
	if len(d.History) > maxDetailEvents || len(d.History) < maxDetailEvents-1 {
		t.Fatalf("%d events kept", len(d.History))
	}
	from := 0
	for _, e := range all {
		if !e.At.Before(d.HistoryFrom) {
			from++
		}
	}
	if from != len(d.History) {
		t.Fatalf("%d events at or after %v, %d kept", from, d.HistoryFrom, len(d.History))
	}
	for i, e := range d.History {
		if e.At.Before(d.HistoryFrom) || (i > 0 && e.At.Before(d.History[i-1].At)) {
			t.Fatalf("event %d at %v, history from %v", i, e.At, d.HistoryFrom)
		}
	}
	if !d.HistoryFrom.After(base) {
		t.Fatalf("history from %v", d.HistoryFrom)
	}
	short := tracker.Detail{History: []tracker.Event{{At: base}}}
	if boundHistory(&short); len(short.History) != 1 || !short.HistoryFrom.IsZero() {
		t.Fatalf("a short history changed: %+v", short)
	}
}

// A detail read through keeps and shows at most maxDetailEvents events.
// Its status time comes from the history as read, which starts at the
// creation, so the record holds the creation as final although the kept
// events start later.
func TestDetailReadBoundsItsHistory(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	base := time.UnixMilli(1_798_000_000_000).UTC()
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		c := card("10001", 1_799_000_000_000)
		d := tracker.Detail{Card: c, HistoryTo: c.Updated}
		for i := range maxDetailEvents + 10 {
			d.History = append(d.History, tracker.Event{At: base.Add(time.Duration(i) * time.Minute), Field: "status"})
		}
		return d, nil
	}
	for _, mirrored := range []bool{false, true} {
		var v DetailView
		var err error
		if mirrored {
			v, _, err = m.svc.MirroredDetail(m.conn.ID, "10001")
		} else {
			v, _, err = m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
		}
		if err != nil || len(v.History) != maxDetailEvents || v.HistoryFrom != millis(base.Add(10*time.Minute)) {
			t.Fatalf("mirror's copy %v: %d events from %d, %v", mirrored, len(v.History), v.HistoryFrom, err)
		}
	}
	created := millis(card("10001", 0).Created)
	if r := m.recordOf(t, "10001"); r.StatusSince != created || !r.StatusFinal {
		t.Fatalf("the record holds status since %d, final %v; want the creation %d, final", r.StatusSince, r.StatusFinal, created)
	}
}
