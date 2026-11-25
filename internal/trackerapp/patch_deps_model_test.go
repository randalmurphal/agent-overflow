package trackerapp

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// depsModel is a tracker of two boards sharing issues, whose rows depend
// on more than their own cards: their parents' cards, on either board,
// the connection's users and statuses, and each board's structure. Its
// row writes go to the mirror as a board's read writes them and through
// the change funnel as that read's tick passes them; its structure
// changes run as a structure tick. readAt is the service's clock, moved
// before each read so that every read begins after the last and its
// writes land.
type depsModel struct {
	t       *testing.T
	m       *mirrorFixture
	rng     *rand.Rand
	root    int64
	clock   *atomic.Int64
	readAt  int64
	next    int
	cards   map[tracker.IssueID]tracker.Card
	rows    map[tracker.BoardID]map[tracker.IssueID]tracker.Row
	catalog tracker.Catalog
	// structures are each board's structure as the tracker answers it.
	structures map[tracker.BoardID]tracker.Structure
}

var depsBoards = []tracker.BoardID{testBoard, "2"}

func (d *depsModel) other(b tracker.BoardID) tracker.BoardID {
	if b == depsBoards[0] {
		return depsBoards[1]
	}
	return depsBoards[0]
}

// anyIssue is a random issue with a row on b; "" for none.
func (d *depsModel) anyIssue(b tracker.BoardID) tracker.IssueID {
	ids := make([]tracker.IssueID, 0, len(d.rows[b]))
	for id := range d.rows[b] {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return ""
	}
	slices.Sort(ids)
	return ids[d.rng.IntN(len(ids))]
}

func (d *depsModel) key() tracker.OrderKey {
	return tracker.OrderKey(fmt.Sprintf("k%06d", d.rng.IntN(1_000_000)))
}

// write stores rows of issues (their cards as d holds them) on b and
// removes missing, as one read of b, and passes the change through the
// funnel as b's tick would.
func (d *depsModel) write(b tracker.BoardID, issues []tracker.IssueID, missing []tracker.IssueID) {
	d.t.Helper()
	d.tick()
	var rows []tracker.Row
	for _, id := range issues {
		r, ok := d.rows[b][id]
		if !ok {
			r = tracker.Row{Order: d.key()}
		}
		r.Card = d.cards[id]
		r.Card.Updated = time.UnixMilli(d.readAt)
		d.cards[id] = r.Card
		d.rows[b][id] = r
		rows = append(rows, r)
	}
	for _, id := range missing {
		delete(d.rows[b], id)
	}
	page := must[store.MirrorRowPage](d.t)(mirrorRows(b, rows, missing, d.catalog, beginReadAt(d.t, d.m.st, d.root, d.readAt)))
	page.Lifetime = d.m.sync(b).Lifetime
	ch := changesOf(must[store.MirrorChanges](d.t)(d.m.st.WriteMirrorRows(d.root, page)))
	d.m.svc.publish(d.root, d.m.svc.gen(d.m.conn.ID), d.m.conn.ID, ch, ownOf(Scope{Connection: d.m.conn.ID, Board: b}, ch), nil)
}

// restore stores id's row on b again, its card as d holds it, unchanged,
// as one read of b, and passes the change through the funnel: the issue is
// kept again, and its children's rows show it again.
func (d *depsModel) restore(b tracker.BoardID, id tracker.IssueID) {
	d.t.Helper()
	d.tick()
	r := tracker.Row{Card: d.cards[id], Order: d.key()}
	d.rows[b][id] = r
	page := must[store.MirrorRowPage](d.t)(mirrorRows(b, []tracker.Row{r}, nil, d.catalog, beginReadAt(d.t, d.m.st, d.root, d.readAt)))
	page.Lifetime = d.m.sync(b).Lifetime
	ch := changesOf(must[store.MirrorChanges](d.t)(d.m.st.WriteMirrorRows(d.root, page)))
	d.m.svc.publish(d.root, d.m.svc.gen(d.m.conn.ID), d.m.conn.ID, ch, ownOf(Scope{Connection: d.m.conn.ID, Board: b}, ch), nil)
}

// parent is a random issue some card names as its parent; "" for none.
func (d *depsModel) parent() tracker.IssueID {
	var ids []tracker.IssueID
	for _, c := range d.cards {
		if c.Parent != "" && !slices.Contains(ids, c.Parent) {
			ids = append(ids, c.Parent)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	slices.Sort(ids)
	return ids[d.rng.IntN(len(ids))]
}

// tick moves the clock to the next read.
func (d *depsModel) tick() {
	d.readAt++
	d.clock.Store(d.readAt)
}

// structure has b's structure tick read the structure d holds.
func (d *depsModel) structure(b tracker.BoardID) {
	d.t.Helper()
	d.tick()
	d.m.p.set(func(p *scripted) {
		p.structure = func(board tracker.BoardID, _ time.Time) (tracker.Structure, error) {
			st := d.structures[board]
			st.Catalog = d.catalog
			return st, nil
		}
	})
	must[Outcome](d.t)(d.m.svc.tickStructure(d.t.Context(), Scope{Connection: d.m.conn.ID, Board: b}, Background))
}

func (d *depsModel) newCard() tracker.IssueID {
	d.next++
	c := card(fmt.Sprint(50000+d.next), 1_799_000_000_000)
	c.Status = []tracker.StatusID{"s1", "s2", "s3"}[d.rng.IntN(3)]
	c.Sprints = [][]tracker.SprintID{nil, {"1"}}[d.rng.IntN(2)]
	d.cards[c.ID] = c
	return c.ID
}

// change makes one random change of the tracker.
func (d *depsModel) change() {
	b := depsBoards[d.rng.IntN(2)]
	id := d.anyIssue(b)
	switch kind := d.rng.IntN(14); {
	case kind >= 11 && kind <= 12:
		// A parent loses its row on this board, or on every board, so the
		// mirror no longer keeps it and its children's rows stop showing it.
		p := d.parent()
		if p == "" {
			return
		}
		for _, on := range depsBoards {
			if _, ok := d.rows[on][p]; ok && (kind == 12 || on == b) {
				d.write(on, nil, []tracker.IssueID{p})
			}
		}
	case kind == 13:
		// A parent with no row comes back to a board, its card unchanged.
		p := d.parent()
		_, on1 := d.rows[depsBoards[0]][p]
		_, on2 := d.rows[depsBoards[1]][p]
		if p != "" && !on1 && !on2 {
			d.restore(b, p)
		}
	case kind == 0 || id == "":
		d.write(b, []tracker.IssueID{d.newCard()}, nil)
	case kind == 1:
		d.write(b, nil, []tracker.IssueID{id})
	case kind == 2:
		// The issue's card renamed, read through b: on the other board its
		// row shows it too, and so do its children's rows on either.
		c := d.cards[id]
		c.Title += "!"
		d.cards[id] = c
		d.write(b, []tracker.IssueID{id}, nil)
	case kind == 3:
		// A parent on the same board or the other.
		parent := d.anyIssue(depsBoards[d.rng.IntN(2)])
		c := d.cards[id]
		if parent != id {
			c.Parent = parent
		}
		d.cards[id] = c
		d.write(b, []tracker.IssueID{id}, nil)
	case kind == 4:
		// An issue of b shared onto the other board.
		d.write(d.other(b), []tracker.IssueID{id}, nil)
	case kind == 5:
		// A user renamed, read with a board's rows.
		d.catalog.Users = slices.Clone(d.catalog.Users)
		d.catalog.Users[0].Name = fmt.Sprint("User 1 v", d.readAt)
		d.write(b, []tracker.IssueID{id}, nil)
	case kind == 6:
		// A status renamed or moved to another group, read with a structure.
		d.catalog.Statuses = slices.Clone(d.catalog.Statuses)
		s := &d.catalog.Statuses[d.rng.IntN(len(d.catalog.Statuses))]
		if d.rng.IntN(2) == 0 {
			s.Name += "*"
		} else {
			s.Group = []tracker.StatusGroup{tracker.GroupToDo, tracker.GroupInProgress, tracker.GroupDone}[d.rng.IntN(3)]
		}
		d.structure(b)
	case kind == 7:
		// The board's columns change.
		st := d.structures[b]
		st.Columns = slices.Clone(st.Columns)
		st.Columns[0], st.Columns[1] = st.Columns[1], st.Columns[0]
		d.structures[b] = st
		d.structure(b)
	case kind == 8:
		// The board's estimate resets.
		st := d.structures[b]
		st.Estimate.Unit = map[tracker.EstimateUnit]tracker.EstimateUnit{tracker.UnitPoints: tracker.UnitTime, tracker.UnitTime: tracker.UnitPoints}[st.Estimate.Unit]
		d.structures[b] = st
		d.structure(b)
	case kind == 9:
		r := d.rows[b][id]
		r.Order = d.key()
		d.rows[b][id] = r
		d.write(b, []tracker.IssueID{id}, nil)
	default:
		c := d.cards[id]
		c.Status = []tracker.StatusID{"s1", "s2", "s3"}[d.rng.IntN(3)]
		d.cards[id] = c
		d.write(b, []tracker.IssueID{id}, nil)
	}
}

// Two clients, each holding one of two boards that share issues, apply
// every event the funnel sends their board, patching or reading again, and
// hold what reading the board again holds, on random changes of every
// dependency a row has: parents on the same board and across boards,
// parents that stop being kept (their row removed from one board or from
// every board) and are kept again with their card unchanged, users and
// statuses renamed, a status changing group, column and estimate changes
// of a board's structure, and issues shared between the boards.
func TestPatchingFollowsEveryDependency(t *testing.T) {
	t.Parallel()
	for seed := range uint64(4) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour})
			clock := &atomic.Int64{}
			clock.Store(1_800_000_000_000)
			m.svc.now = func() time.Time { return time.UnixMilli(clock.Load()) }
			m.tracks(nil)
			// Each board's pump has read it once; it reads nothing more.
			for _, b := range depsBoards {
				m.subscribe(t, b)
				m.events.waitFor(t, "the first tick", func(boards []BoardEvent, _ []IssueEvent, _ []error) bool {
					return slices.ContainsFunc(boards, func(e BoardEvent) bool { return e.Scope.Board == b && e.Sync.ReadAt > 0 })
				})
			}
			d := &depsModel{t: t, m: m, rng: rand.New(rand.NewPCG(seed, 11)), root: m.root(), clock: clock, readAt: clock.Load(),
				cards: map[tracker.IssueID]tracker.Card{}, rows: map[tracker.BoardID]map[tracker.IssueID]tracker.Row{},
				catalog: webStructure(testBoard).Catalog, structures: map[tracker.BoardID]tracker.Structure{}}
			for _, b := range depsBoards {
				d.rows[b] = map[tracker.IssueID]tracker.Row{}
				d.structures[b] = webStructure(b)
			}
			for range 40 {
				d.write(depsBoards[d.rng.IntN(2)], []tracker.IssueID{d.newCard()}, nil)
			}
			sections := []Section{{Kind: SectionSprint, Sprint: "1"}, {Kind: SectionBacklog},
				{Kind: SectionColumn}, {Kind: SectionColumn, Column: 1}, {Kind: SectionColumn, Column: 2}}
			read := func(b tracker.BoardID) heldBoard {
				return heldFromView(must[BoardView](t)(m.svc.Board(Scope{Connection: m.conn.ID, Board: b}, StatusGroups{}, sections)), sections)
			}
			clients := map[tracker.BoardID]heldBoard{}
			for _, b := range depsBoards {
				clients[b] = read(b)
			}
			seen, _, _ := m.events.snapshot()
			for round := range 80 {
				for range 1 + d.rng.IntN(2) {
					d.change()
				}
				events, _, _ := m.events.snapshot()
				for _, e := range events[len(seen):] {
					scope := e.Scope
					switch ids := unite(slices.Clone(e.Changed), e.Removed); {
					case e.ReadAgain:
						clients[scope.Board] = read(scope.Board)
					case len(ids) > 0:
						patch := must[BoardPatch](t)(m.svc.BoardPatch(scope, StatusGroups{}, sections, ids, clients[scope.Board].held()))
						clients[scope.Board] = applyPatch(clients[scope.Board], ids, patch)
					}
				}
				seen = events
				for _, b := range depsBoards {
					scope := Scope{Connection: m.conn.ID, Board: b}
					full := pageOn(t, m.svc, scope, read(b))
					if want := prefixOf(full, clients[b]); !clients[b].sameRows(want) {
						t.Fatalf("round %d, board %s: the patched client holds\n%s\nreading again gives\n%s", round, b, clients[b].describe(), want.describe())
					}
				}
			}
			// The changes landed: every row shows the user and status names
			// and the parent title the model holds.
			for _, b := range depsBoards {
				for _, sec := range pageOn(t, m.svc, Scope{Connection: m.conn.ID, Board: b}, read(b)).sections {
					for _, r := range sec.rows {
						d.landed(r.Card)
					}
				}
			}
		})
	}
}

// landed fails the test unless c, as a board shows it, names the user,
// status and parent title the model holds.
func (d *depsModel) landed(c CardView) {
	d.t.Helper()
	if c.Assignee == nil || c.Assignee.Name != d.catalog.Users[0].Name {
		d.t.Fatalf("issue %s shows assignee %+v; the tracker names %q", c.ID, c.Assignee, d.catalog.Users[0].Name)
	}
	i := slices.IndexFunc(d.catalog.Statuses, func(s tracker.Status) bool { return string(s.ID) == c.Status.ID })
	if i < 0 || c.Status.Name != d.catalog.Statuses[i].Name {
		d.t.Fatalf("issue %s shows status %+v; the tracker's statuses %+v", c.ID, c.Status, d.catalog.Statuses)
	}
	if c.Parent != nil && c.Parent.Title != "" {
		if p, ok := d.cards[tracker.IssueID(c.Parent.ID)]; ok && c.Parent.Title != p.Title {
			d.t.Fatalf("issue %s shows parent %+v; the parent's title is %q", c.ID, c.Parent, p.Title)
		}
	}
}
