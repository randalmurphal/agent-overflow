package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	sqlite "modernc.org/sqlite"
)

// boundOf keeps issue for pump of session 1 at a read begun at at and
// makes the keep's bind the issue's current one (ActivateMirrorBind).
func boundOf(t *testing.T, s *Store, root int64, issue string, pump, at int64) MirrorBind {
	t.Helper()
	bind, _, err := s.KeepMirrorTicket(root, issue, 1, pump, mirrorReadOf(root, at))
	if err != nil {
		t.Fatal(err)
	}
	if !s.ActivateMirrorBind(bind) {
		t.Fatalf("the keep's bind %+v did not activate", bind)
	}
	return bind
}

// checkOf stores issue's held card under bind with no body (a check by
// card), with a person's opening at opening (zero for none), by a read
// begun at at.
func checkOf(t *testing.T, s *Store, root, sweep int64, issue string, bind MirrorBind, opening, at int64) {
	t.Helper()
	ticket := heldTicket(t, s, root, issue, nil)
	ticket.Bind = bind
	if _, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{ticket}, MirrorCatalog{}, mirrorReadOf(root, opening), mirrorReadOf(root, at)); err != nil {
		t.Fatal(err)
	}
}

// onFirstIssueWrite runs fn once, at the first row write of
// tracker_cache_issues the writer connection makes from now on, inside the
// statement that makes it: in a ticket write, after the admission lookup.
func onFirstIssueWrite(t *testing.T, s *Store, fn func()) {
	t.Helper()
	fired := false
	hook := func(f sqlite.PreUpdateHookFn) {
		conn, err := s.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := conn.Raw(func(dc any) error {
			hooks, ok := dc.(*stmtCacheConn).sqliteConn.(sqlite.HookRegisterer)
			if !ok {
				return fmt.Errorf("writer connection %T has no hooks", dc)
			}
			hooks.RegisterPreUpdateHook(f)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	hook(func(d sqlite.SQLitePreUpdateData) {
		if !fired && d.TableName == "tracker_cache_issues" {
			fired = true
			fn()
		}
	})
	t.Cleanup(func() { hook(nil) })
}

// An open ticket's read stores a body and records its opening only under
// the issue's current bind: not under a bind whose keep committed and
// that was not activated, nor once it ended, though its row still keeps
// the issue (its drop not run or failed); a read no open ticket made
// starts a body only as a person's opening. A close and a reopen make a
// new bind: a read of the closed lifetime starts no body and replaces
// none, however late it began, and the new lifetime's does.
func TestAnOpenTicketsBindAdmitsItsBody(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	sweep := currentSweep(s, root)
	first, _, err := s.KeepMirrorTicket(root, "10001", 1, 1, mirrorReadAt(100))
	if err != nil || first.IsZero() {
		t.Fatalf("the keep: %+v, %v", first, err)
	}
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 105, []byte(`{"v":"pending"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a read under a bind not activated, on an issue a board keeps: %+v", w)
	}
	s.ActivateMirrorBind(first)
	if w, err := storeTicket(t, s, root, sweep, "10001", 0, 110, []byte(`{"v":"direct"}`)); err != nil || w.Body != MirrorBodyNotKept {
		t.Fatalf("a read no open ticket made, not an opening: %+v, %v", w, err)
	}
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 110, []byte(`{"v":"after"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a read under the bind: %+v", w)
	}
	if got := bodyOf(t, s, root, "10001"); got != "5 110 100 after" {
		t.Fatalf("the record holds %s", got)
	}
	// Closed: the bind ends; its row stands until the drop.
	s.EndMirrorBind(first)
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 115, []byte(`{"v":"closed"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a read of the closed ticket before its drop: %+v", w)
	}
	checkOf(t, s, root, sweep, "10001", first, 120, 120)
	if got := bodyOf(t, s, root, "10001"); got != "5 110 100 after" {
		t.Fatalf("after the closed ticket's reads before its drop the record holds %s", got)
	}
	if _, err := s.DropMirrorTicket(1, 1); err != nil {
		t.Fatal(err)
	}
	second := boundOf(t, s, root, "10001", 2, 300)
	// Held: the closed lifetime's read, begun after the new bind, replaces
	// nothing.
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 320, []byte(`{"v":"closed"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a read of the closed lifetime over a held body: %+v", w)
	}
	mustExec(t, s.db, `DELETE FROM tracker_cache_issue_bodies`)
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET body_updated_at = 0, body_read_at = 0, body_wall = 0, body_opened_at = 0, body_digest = x''`)
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 330, []byte(`{"v":"closed"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a read of the closed lifetime with no body held: %+v", w)
	}
	if w := storeBoundTicket(t, s, root, sweep, "10001", second, 310, []byte(`{"v":"reopened"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a read of the new lifetime: %+v", w)
	}
	if got := bodyOf(t, s, root, "10001"); got != "5 310 300 reopened" {
		t.Fatalf("the record holds %s", got)
	}
	checkBodiesHeld(t, s, "the reopen")
}

// A cards-only ticket write (a check that confirmed the held body) records
// its opening on the body, and so its place in the cap, only under the
// issue's current bind: a closed lifetime's check, an opening included,
// leaves the body's opening as it was.
func TestACardsOnlyWriteOfAClosedLifetimeRecordsNoOpening(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	sweep := currentSweep(s, root)
	first := boundOf(t, s, root, "10001", 1, 100)
	s.EndMirrorBind(first)
	if _, err := s.DropMirrorTicket(1, 1); err != nil {
		t.Fatal(err)
	}
	second := boundOf(t, s, root, "10001", 2, 300)
	if w := storeBoundTicket(t, s, root, sweep, "10001", second, 310, []byte(`{"v":"reopened"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a read of the new lifetime: %+v", w)
	}
	checkOf(t, s, root, sweep, "10001", first, 400, 400)
	if got := bodyOf(t, s, root, "10001"); got != "5 310 300 reopened" {
		t.Fatalf("after the closed lifetime's check the record holds %s; want the reopened body's opening", got)
	}
	checkOf(t, s, root, sweep, "10001", second, 410, 410)
	if got := bodyOf(t, s, root, "10001"); got != "5 310 410 reopened" {
		t.Fatalf("after the current bind's check the record holds %s; want its opening", got)
	}
}

// A ticket opened again while the closed pump's row still stands (its
// drop owed): both rows keep the issue, and the bind decides alone. The
// new keep's bind admits nothing until activated, and the old one admits
// until then; activated, the new one replaces it, and a late end of the
// old one leaves it. A bind admits with its row gone (a board keeps the
// issue) until it ends. A keep that fails gives no bind, and no two binds
// of the process are the same.
func TestABindIsCurrentFromItsActivationToItsEnd(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	sweep := currentSweep(s, root)
	first := boundOf(t, s, root, "10001", 1, 100)
	second, _, err := s.KeepMirrorTicket(root, "10001", 1, 2, mirrorReadAt(300))
	if err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT pump FROM tracker_cache_open_tickets ORDER BY pump`, "1,2")
	if w := storeBoundTicket(t, s, root, sweep, "10001", second, 305, []byte(`{"v":"pending"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a read under the reopening's pending bind: %+v", w)
	}
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 306, []byte(`{"v":"first"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a read under the bind still current: %+v", w)
	}
	s.ActivateMirrorBind(second)
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 350, []byte(`{"v":"replaced"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a read under the replaced bind: %+v", w)
	}
	s.EndMirrorBind(first)
	if w := storeBoundTicket(t, s, root, sweep, "10001", second, 360, []byte(`{"v":"second"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a read under the current bind after a late end of the replaced one: %+v", w)
	}
	for _, pump := range []int64{2, 1} {
		if _, err := s.DropMirrorTicket(1, pump); err != nil {
			t.Fatal(err)
		}
	}
	if w := storeBoundTicket(t, s, root, sweep, "10001", second, 370, []byte(`{"v":"rowless"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a read under the current bind with its row gone, a board keeping the issue: %+v", w)
	}
	s.EndMirrorBind(second)
	if w := storeBoundTicket(t, s, root, sweep, "10001", second, 380, []byte(`{"v":"ended"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a read under an ended bind: %+v", w)
	}
	if got := bodyOf(t, s, root, "10001"); got != "5 370 300 rowless" {
		t.Fatalf("the record holds %s", got)
	}
	third := boundOf(t, s, root, "10001", 1, 400)
	again, _, err := s.KeepMirrorTicket(root, "10001", 1, 1, mirrorReadAt(500))
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := s.KeepMirrorTicket(root, "10002", 1, 1, mirrorReadAt(500))
	if !errors.Is(err, ErrInvalidTrackerCache) || !failed.IsZero() {
		t.Fatalf("a keep of a second issue by a pump: %+v, %v; want no bind and refused", failed, err)
	}
	binds := map[MirrorBind]bool{first: true, second: true, third: true, again: true}
	if len(binds) != 4 {
		t.Fatalf("the binds %+v %+v %+v %+v repeat", first, second, third, again)
	}
}

// A bind moves from pending to current once: it activates at most once,
// never after its end, and never once a newer keep of its issue issued
// another, whether that one is pending, current or ended. A refused activation
// leaves the current bind as it was, and no bind stays pending once each
// is activated, ended or replaced.
func TestABindActivatesOnceWhilePending(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	sweep := currentSweep(s, root)
	pump := int64(0)
	keep := func() MirrorBind {
		t.Helper()
		pump++
		b, _, err := s.KeepMirrorTicket(root, "10001", 1, pump, mirrorReadAt(100+pump))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	admits := func(b MirrorBind) bool {
		t.Helper()
		return storeBoundTicket(t, s, root, sweep, "10001", b, 200+pump, []byte(`{"v":"x"}`)).Body == MirrorBodyStored
	}
	a := keep()
	if !s.ActivateMirrorBind(a) || s.ActivateMirrorBind(a) || !admits(a) {
		t.Fatal("a bind activated twice, or not current after its first activation")
	}
	ended := keep()
	s.EndMirrorBind(ended)
	if s.ActivateMirrorBind(ended) || !admits(a) {
		t.Fatal("a bind ended while pending activated, or moved the current one")
	}
	older, newer := keep(), keep()
	if s.ActivateMirrorBind(older) || admits(older) || !admits(a) {
		t.Fatal("an older pending bind activated before a newer one of its issue, or moved the current one")
	}
	if !s.ActivateMirrorBind(newer) || s.ActivateMirrorBind(older) || admits(older) || !admits(newer) {
		t.Fatal("an older pending bind activated after a newer one of its issue")
	}
	older, newer = keep(), keep()
	if !s.ActivateMirrorBind(newer) {
		t.Fatal("the newer bind did not activate")
	}
	s.EndMirrorBind(newer)
	if s.ActivateMirrorBind(older) || admits(older) {
		t.Fatal("an older pending bind activated after a newer one of its issue ended")
	}
	if s.ActivateMirrorBind(MirrorBind{}) {
		t.Fatal("no bind activated")
	}
	if n := len(s.mirrorBinds.pending); n != 0 {
		t.Fatalf("%d binds still pending", n)
	}
}

// A bind of one issue offered for another is refused before anything is
// written; one of another root (a run that began before a restore made
// the pump's new root) is refused as an ended one is.
func TestABindOfAnotherIssueOrRootIsRefused(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	sweep := currentSweep(s, root)
	x := boundOf(t, s, root, "10001", 1, 100)
	boundOf(t, s, root, "10002", 2, 100)
	ticket := heldTicket(t, s, root, "10002", []byte(`{"v":"y"}`))
	ticket.Bind = x
	if _, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{ticket}, MirrorCatalog{}, MirrorRead{}, mirrorReadAt(110)); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a bind of 10001 offered for 10002: %v; want refused", err)
	}
	if got := bodyOf(t, s, root, "10002"); got != "none" {
		t.Fatalf("the refused write left 10002 holding %s", got)
	}
	// The other root keeps 10001 through an open ticket of its own.
	other := mustOtherRoot(t, s)
	boundOf(t, s, other, "10001", 9, 100)
	ticket = heldTicket(t, s, other, "10001", []byte(`{"v":"other"}`))
	ticket.Bind = x
	w, err := s.StoreMirrorTickets(other, currentSweep(s, other), []MirrorTicket{ticket}, MirrorCatalog{}, MirrorRead{}, mirrorReadOf(other, 110))
	if err != nil || w.Tickets[0].Body != MirrorBodyNotKept {
		t.Fatalf("a bind of root %d offered in root %d: %+v, %v; want not kept", root, other, w, err)
	}
}

// A read no open ticket made records its own opening alone, never the
// current bind's, and so does a person's opening confirmed on the cached
// body (ConfirmMirrorBody, a first paint's): under the bind, a check by
// card records the later of its opening and the bind's.
func TestADirectOpeningRecordsItsOwnOpening(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	sweep := currentSweep(s, root)
	if w, err := storeTicket(t, s, root, sweep, "10001", 200, 200, []byte(`{"v":"direct"}`)); err != nil || w.Body != MirrorBodyStored {
		t.Fatalf("a direct opening's body: %+v, %v", w, err)
	}
	p := boundOf(t, s, root, "10001", 1, 500)
	checkOf(t, s, root, sweep, "10001", MirrorBind{}, 300, 300)
	if got := bodyOf(t, s, root, "10001"); got != "5 200 300 direct" {
		t.Fatalf("after a direct check by card opened at 300 the record holds %s; want its own opening", got)
	}
	b, ok, err := s.GetMirrorBody(root, "10001")
	if err != nil || !ok {
		t.Fatalf("the cached body: %v, %v", ok, err)
	}
	if confirmed, err := s.ConfirmMirrorBody(root, b, mirrorReadAt(350)); err != nil || !confirmed {
		t.Fatalf("the confirmation: %v, %v", confirmed, err)
	}
	if got := bodyOf(t, s, root, "10001"); got != "5 200 350 direct" {
		t.Fatalf("after a confirmation opened at 350 the record holds %s; want its own opening", got)
	}
	checkOf(t, s, root, sweep, "10001", p, 0, 400)
	if got := bodyOf(t, s, root, "10001"); got != "5 200 500 direct" {
		t.Fatalf("after a check by card under the bind the record holds %s; want the bind's opening", got)
	}
}

// The bind ends with no I/O, so its end and a write's admission are
// ordered by the one lookup in the write's transaction: a write whose
// lookup came first commits its body and the bind's opening, though the
// end (and a replacement's activation) came before its commit; one whose
// lookup came after stores and records nothing. Either way the next
// write under the ended bind is refused, and the replacement's admits.
func TestAWriteIsAdmittedByItsLookupAgainstTheBindsEnd(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name            string
		lookupFirst     bool
		replaced        bool
		outcome         MirrorBodyOutcome
		held, replaced2 string
	}{
		{"ended before the lookup", false, false, MirrorBodyNotKept, "5 50 50 held", ""},
		{"ended after the lookup", true, false, MirrorBodyStored, "5 120 100 late", ""},
		{"replaced before the lookup", false, true, MirrorBodyNotKept, "5 50 50 held", "5 140 300 new"},
		{"replaced after the lookup", true, true, MirrorBodyStored, "5 120 100 late", "5 140 300 new"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
			sweep := currentSweep(s, root)
			if _, err := storeTicket(t, s, root, sweep, "10001", 50, 50, []byte(`{"v":"held"}`)); err != nil {
				t.Fatal(err)
			}
			p := boundOf(t, s, root, "10001", 1, 100)
			var q MirrorBind
			if c.replaced {
				var err error
				if q, _, err = s.KeepMirrorTicket(root, "10001", 1, 2, mirrorReadAt(300)); err != nil {
					t.Fatal(err)
				}
			}
			release := func() {
				s.EndMirrorBind(p)
				if c.replaced {
					s.ActivateMirrorBind(q)
				}
			}
			if c.lookupFirst {
				onFirstIssueWrite(t, s, release)
			} else {
				release()
			}
			ticket := heldTicket(t, s, root, "10001", []byte(`{"v":"late"}`))
			ticket.Bind = p
			got, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{ticket}, MirrorCatalog{}, MirrorRead{}, mirrorReadAt(120))
			if err != nil || got.Tickets[0].Body != c.outcome {
				t.Fatalf("the write under the bind: %+v, %v; want %v", got, err, c.outcome)
			}
			if got := bodyOf(t, s, root, "10001"); got != c.held {
				t.Fatalf("the record holds %s; want %s", got, c.held)
			}
			if w := storeBoundTicket(t, s, root, sweep, "10001", p, 130, []byte(`{"v":"later"}`)); w.Body != MirrorBodyNotKept {
				t.Fatalf("the next write under the ended bind: %+v", w)
			}
			if c.replaced {
				if w := storeBoundTicket(t, s, root, sweep, "10001", q, 140, []byte(`{"v":"new"}`)); w.Body != MirrorBodyStored {
					t.Fatalf("a write under the replacement: %+v", w)
				}
				if got := bodyOf(t, s, root, "10001"); got != c.replaced2 {
					t.Fatalf("the record holds %s; want %s", got, c.replaced2)
				}
			}
		})
	}
}

// A root ID is never another root's: a restore empties the mirror and
// leaves the roots' sequence, so the root made after it has a new ID; a
// write naming the ended root fails, and a bind of it, still current,
// admits nothing in the new root. A bind names its root by ID on this
// (MirrorBind).
func TestARestoreNeverReusesARootID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	c := mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user1@example.com", 10))
	snap := filepath.Join(t.TempDir(), "db.snapshot")
	if err := s.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	root, _, err := s.GetOrCreateMirrorRoot(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	listMirrorBoards(t, s, defaultTestProjectID, c.ID, "10")
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	p := boundOf(t, s, root, "10001", 1, 100)
	if _, err := s.RestoreFrom(snap); err != nil {
		t.Fatal(err)
	}
	again, _, err := s.GetOrCreateMirrorRoot(c.ID)
	if err != nil || again <= root {
		t.Fatalf("the root after the restore: %d, %v; want an ID above %d", again, err, root)
	}
	ticket := MirrorTicket{Card: webCard("10001", 5), Body: []byte(`{"v":"old"}`), Bind: p}
	if _, err := s.StoreMirrorTickets(root, 1, []MirrorTicket{ticket}, MirrorCatalog{}, MirrorRead{}, mirrorReadAt(110)); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("a write naming the restored root: %v; want ErrTrackerCacheGone", err)
	}
	listMirrorBoards(t, s, defaultTestProjectID, c.ID, "10")
	writeMirrorBoard(t, s, again, "10", []MirrorRow{webRow("10001", 5, "a")})
	sweep := currentSweep(s, again)
	if w := storeBoundTicket(t, s, again, sweep, "10001", p, 110, []byte(`{"v":"old"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a bind of the restored root in the root made after it: %+v", w)
	}
	q := boundOf(t, s, again, "10001", 3, 120)
	if w := storeBoundTicket(t, s, again, sweep, "10001", q, 130, []byte(`{"v":"new"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a bind of the new root: %+v", w)
	}
}
