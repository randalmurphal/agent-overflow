package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// mirrorScope is one kind of content the mirror admits by read order:
// store writes variant v ("a" or "b") as a read that began at readAt, and
// content reads back the stored variant.
type mirrorScope struct {
	store   func(readAt int64, v string) error
	content func() string
	// later is what an identical read that began later writes: one
	// narrow row per watermark it moves, and the statements it runs.
	later      map[string]int
	statements int
}

// mirrorScopes are the scopes of root, held by s (nil: for their names).
func mirrorScopes(t *testing.T, s *Store, root int64) map[string]mirrorScope {
	var lifetime, sweep int64
	if s != nil {
		lifetime, sweep = currentLifetime(s, root, "10"), currentSweep(s, root)
	}
	scalar := func(query string) func() string {
		return func() string {
			var v string
			if err := s.db.QueryRow(query).Scan(&v); err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	page := func(readAt int64, rows ...MirrorRow) MirrorRowPage {
		return MirrorRowPage{BoardID: "10", Lifetime: lifetime, Rows: rows, ReadAt: readAt}
	}
	return map[string]mirrorScope{
		"detail": {
			store: func(readAt int64, v string) error {
				_, err := s.StoreMirrorDetail(root, sweep, "10001", 10, readAt, []byte(`{"v":"`+v+`"}`))
				return err
			},
			content: scalar(`SELECT data ->> 'v' FROM tracker_cache_issue_details`),
			later:   map[string]int{"tracker_cache_issue_details": 1}, statements: 3,
		},
		"board order": {
			store: func(readAt int64, v string) error {
				_, err := s.WriteMirrorRows(root, page(readAt, webRow("10001", 5, v)))
				return err
			},
			content: scalar(`SELECT order_key FROM tracker_cache_board_issues WHERE issue_id = '10001'`),
			later:   map[string]int{"tracker_cache_board_issues": 1}, statements: 6,
		},
		"cursor": {
			store: func(readAt int64, v string) error {
				_, err := s.WriteMirrorRowPage(root, page(readAt), []byte(v))
				return err
			},
			content: scalar(`SELECT cast(cursor AS TEXT) FROM tracker_cache_sync WHERE board_id = '10'`),
			later:   map[string]int{"tracker_cache_sync": 1}, statements: 2,
		},
		"structure": {
			store: func(readAt int64, v string) error {
				st := webStructure()
				st.Board.Name, st.Catalog, st.ReadAt, st.Lifetime = v, MirrorCatalog{}, readAt, lifetime
				_, err := s.ReplaceMirrorStructure(root, st)
				return err
			},
			content: scalar(`SELECT name FROM tracker_cache_boards WHERE board_id = '10'`),
			later:   map[string]int{"tracker_cache_boards": 1}, statements: 9,
		},
		"catalog user": {
			store: func(readAt int64, v string) error {
				_, err := s.UpsertMirrorCards(root, sweep, nil, MirrorCatalog{Users: []MirrorUser{{ID: "acc-user1", Name: v}}}, readAt)
				return err
			},
			content: scalar(`SELECT name FROM tracker_cache_users`),
			later:   map[string]int{"tracker_cache_users": 1}, statements: 2,
		},
		"catalog status": {
			store: func(readAt int64, v string) error {
				_, err := s.UpsertMirrorCards(root, sweep, nil, MirrorCatalog{Statuses: []MirrorStatus{{ID: "1", Name: v, Group: MirrorGroupToDo}}}, readAt)
				return err
			},
			content: scalar(`SELECT name FROM tracker_cache_statuses WHERE status_id = '1'`),
			later:   map[string]int{"tracker_cache_statuses": 1}, statements: 2,
		},
		"worklogs": {
			store: func(readAt int64, v string) error {
				spent := map[string]int64{"a": 60, "b": 120}[v]
				_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-05", readAt, []MirrorWorklog{webWorklog("w1", "2026-01-05", spent)})
				return err
			},
			content: func() string {
				return map[string]string{"60": "a", "120": "b"}[scalar(`SELECT spent_seconds FROM tracker_cache_worklogs`)()]
			},
			later: map[string]int{"tracker_cache_worklog_days": 1}, statements: 6,
		},
	}
}

// The latest admitted read of each scope is kept even when its content is
// identical to what is stored: an identical read that began later moves
// the scope's watermark, one narrow row write with no revision bump, so a
// different read that began between the two is superseded. A duplicate at
// the same watermark writes nothing.
func TestMirrorLaterIdenticalReadIsAdmitted(t *testing.T) {
	for name := range mirrorScopes(t, nil, 0) {
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
		scope := mirrorScopes(t, s, root)[name]
		recorder := recordMirrorWrites(t, s)
		revision := scalarRevision(t, s, root)
		if err := scope.store(10, "a"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		recorder.reset()
		var err error
		statements := countMirrorStatements(t, func() { err = scope.store(30, "a") })
		if writes := recorder.rowWrites(); err != nil || !reflect.DeepEqual(writes, scope.later) || statements != scope.statements {
			t.Errorf("%s: identical later read wrote %v in %d statements, %v; want %v in %d", name, writes, statements, err, scope.later, scope.statements)
		}
		if got := scalarRevision(t, s, root); got != revision {
			t.Errorf("%s: revision %d -> %d", name, revision, got)
		}
		recorder.reset()
		if err := scope.store(30, "a"); err != nil || len(recorder.rowWrites()) > 0 {
			t.Errorf("%s: the same read again wrote %v, %v", name, recorder.rowWrites(), err)
		}
		if err := scope.store(20, "b"); err != nil || scope.content() != "a" {
			t.Errorf("%s: a read between the two stored %q, %v; want it superseded", name, scope.content(), err)
		}
		if err := scope.store(40, "b"); err != nil || scope.content() != "b" {
			t.Errorf("%s: a later read stored %q, %v", name, scope.content(), err)
		}
	}
}

func scalarRevision(t *testing.T, s *Store, root int64) int64 {
	t.Helper()
	var revision int64
	if err := s.db.QueryRow(`SELECT revision FROM tracker_cache_boards WHERE root_id = ? AND board_id = '10'`, root).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

// An absence is admitted like content: a read that finds an issue off a
// board, or a complete listing that leaves it out, fences the reads that
// began before it even when the board held no row of it, at one write of
// the board's removal time; the same absence again writes nothing.
func TestMirrorAbsenceIsAdmitted(t *testing.T) {
	t.Parallel()
	for _, absence := range []string{"missing", "reconcile"} {
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
		lifetime := currentLifetime(s, root, "10")
		absent := func() error {
			if absence == "missing" {
				_, err := s.WriteMirrorRows(root, MirrorRowPage{BoardID: "10", Lifetime: lifetime, Missing: []string{"10002"}, ReadAt: 30})
				return err
			}
			_, err := s.ReconcileMirrorBoard(root, "10", lifetime, []string{"10001"}, true, 30)
			return err
		}
		recorder := recordMirrorWrites(t, s)
		if err := absent(); err != nil {
			t.Fatalf("%s: %v", absence, err)
		}
		if writes := recorder.rowWrites(); !reflect.DeepEqual(writes, map[string]int{"tracker_cache_boards": 1}) {
			t.Errorf("%s of an absent row wrote %v; want the board's removal time alone", absence, writes)
		}
		recorder.reset()
		if err := absent(); err != nil || len(recorder.rowWrites()) > 0 {
			t.Errorf("%s again wrote %v, %v", absence, recorder.rowWrites(), err)
		}
		got, err := s.WriteMirrorRows(root, MirrorRowPage{BoardID: "10", Lifetime: lifetime, Rows: []MirrorRow{webRow("10002", 5, "b")}, ReadAt: 20})
		if err != nil || !reflect.DeepEqual(got, MirrorChanges{Superseded: []string{"10002"}}) {
			t.Errorf("%s: a read from before it = %+v, %v; want it superseded", absence, got, err)
		}
		assertRows(t, s.db, fmt.Sprintf(`SELECT count(*) FROM tracker_cache_board_issues WHERE root_id = %d AND issue_id = '10002'`, root), "0")
	}
}

// A detail write records the opening it carries in the same statement as
// its document or its read time: whatever it moves, one row write. Only an
// opening starts a detail.
func TestStoreMirrorDetailRecordsTheOpening(t *testing.T) {
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	recorder := recordMirrorWrites(t, s)
	sweep := currentSweep(s, root)
	for _, step := range []struct {
		name               string
		issue              string
		opened, read       int64
		doc                string
		want               MirrorDetailWrite
		statements, writes int
		state              string
	}{
		{"new, opened", "10001", 80, 80, `{"v":1}`, MirrorDetailWrite{Stored: true, Changed: true}, 4, 1, "80 80 1"},
		{"opened again", "10001", 200, 80, `{"v":1}`, MirrorDetailWrite{Stored: true}, 3, 1, "200 80 1"},
		{"the same again", "10001", 200, 80, `{"v":1}`, MirrorDetailWrite{Stored: true}, 2, 0, "200 80 1"},
		{"earlier document, opened", "10001", 300, 70, `{"v":0}`, MirrorDetailWrite{Stored: true, Superseded: true}, 3, 1, "300 80 1"},
		{"identical later read", "10001", 0, 90, `{"v":1}`, MirrorDetailWrite{Stored: true}, 3, 1, "300 90 1"},
		{"earlier opening", "10001", 250, 95, `{"v":2}`, MirrorDetailWrite{Stored: true, Changed: true}, 3, 1, "300 95 2"},
		{"earlier opening, same document read later", "10001", 150, 99, `{"v":2}`, MirrorDetailWrite{Stored: true}, 3, 1, "300 99 2"},
		{"new, no opening", "10002", 0, 60, `{"v":1}`, MirrorDetailWrite{}, 2, 0, "none"},
		{"new, opened", "10002", 60, 60, `{"v":1}`, MirrorDetailWrite{Stored: true, Changed: true}, 4, 1, "60 60 1"},
	} {
		recorder.reset()
		var got MirrorDetailWrite
		var err error
		statements := countMirrorStatements(t, func() {
			got, err = s.StoreMirrorDetail(root, sweep, step.issue, step.opened, step.read, []byte(step.doc))
		})
		writes := 0
		for _, n := range recorder.rowWrites() {
			writes += n
		}
		if err != nil || !reflect.DeepEqual(got, step.want) || statements != step.statements || writes != step.writes {
			t.Errorf("%s: %+v, %v in %d statements, %d rows; want %+v in %d, %d", step.name, got, err, statements, writes, step.want, step.statements, step.writes)
		}
		assertRows(t, s.db, `SELECT coalesce((SELECT opened_at || ' ' || read_at || ' ' || (data ->> 'v') FROM tracker_cache_issue_details WHERE root_id = '`+fmt.Sprint(root)+`' AND issue_id = '`+step.issue+`'), 'none')`, step.state)
	}
	if _, err := s.StoreMirrorDetail(root, sweep, "10001", -1, 99, nil); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a negative opening: %v", err)
	}
	if _, err := s.StoreMirrorDetail(root, 0, "10001", 0, 99, nil); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("no sweep: %v", err)
	}
}
