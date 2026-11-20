package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// A batch of details stores each document as StoreMirrorDetail would, in
// one transaction, its results in the batch's order: the same document
// moves its read time, a changed one replaces it, a new one needs an
// opening, an unkept issue stores nothing. A batch naming an issue twice,
// or none, is refused whole.
func TestStoreMirrorDetailsStoresEachInOneTransaction(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, 4, "a"))
	sweep := currentSweep(s, root)
	got, err := s.StoreMirrorDetails(root, sweep, []MirrorDetailDoc{{"10001", []byte(`{"v":1}`)}, {"10002", []byte(`{"v":1}`)}}, 10, 10)
	if err != nil || !reflect.DeepEqual(got, []MirrorDetailWrite{{Stored: true, Changed: true}, {Stored: true, Changed: true}}) {
		t.Fatalf("opened: %+v, %v", got, err)
	}
	got, err = s.StoreMirrorDetails(root, sweep, []MirrorDetailDoc{
		{"10001", []byte(`{"v":1}`)}, {"10002", []byte(`{"v":2}`)}, {"10003", []byte(`{"v":1}`)}, {"10009", []byte(`{"v":1}`)},
	}, 0, 20)
	want := []MirrorDetailWrite{{Stored: true}, {Stored: true, Changed: true}, {}, {}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("a round's write: %+v, %v; want %+v", got, err, want)
	}
	assertRows(t, s.db, `SELECT group_concat(issue_id || ' ' || opened_at || ' ' || read_at || ' ' || (data ->> 'v'), ', ') FROM
		(SELECT * FROM tracker_cache_issue_details ORDER BY issue_id)`, "10001 10 20 1, 10002 10 20 2")
	for name, docs := range map[string][]MirrorDetailDoc{
		"none":           nil,
		"an issue twice": {{"10001", []byte(`{}`)}, {"10003", []byte(`{}`)}, {"10001", []byte(`{}`)}},
		"no issue":       {{"", []byte(`{}`)}},
	} {
		if _, err := s.StoreMirrorDetails(root, sweep, docs, 30, 30); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details`, "2")
}

// The states of stored details are what GetMirrorDetail reads of each,
// without the documents, in one statement: none for an issue with no
// detail, no document or no longer kept. A lookup over mirrorMaxLookup
// issues is refused.
func TestListMirrorDetailStates(t *testing.T) {
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, 4, "a"))
	sweep := currentSweep(s, root)
	if _, err := s.StoreMirrorDetails(root, sweep, []MirrorDetailDoc{{"10001", []byte(`{"v":1}`)}, {"10002", []byte(`{"v":1}`)}}, 10, 10); err != nil {
		t.Fatal(err)
	}
	// An opening whose read began earlier holds no document.
	if _, err := s.StoreMirrorDetail(root, sweep, "10003", 30, 20, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10002"}, ReadAt: 40})); err != nil {
		t.Fatal(err)
	}
	var got []MirrorDetail
	statements := countMirrorStatements(t, func() {
		var err error
		if got, err = s.ListMirrorDetailStates(root, []string{"10001", "10002", "10003", "10004", "10009"}); err != nil {
			t.Fatal(err)
		}
	})
	full, ok, err := s.GetMirrorDetail(root, "10001")
	if err != nil || !ok {
		t.Fatalf("detail: %v, %v", ok, err)
	}
	full.Data = nil
	if statements != 1 || !reflect.DeepEqual(got, []MirrorDetail{full}) {
		t.Fatalf("%d statements: %+v; want %+v", statements, got, full)
	}
	if confirmed, err := s.ConfirmMirrorDetail(root, got[0], 50); err != nil || !confirmed.Stored {
		t.Fatalf("a state confirms as the detail: %+v, %v", confirmed, err)
	}
	ids := make([]string, mirrorMaxLookup+1)
	for i := range ids {
		ids[i] = fmt.Sprint(20000 + i)
	}
	if _, err := s.ListMirrorDetailStates(root, ids); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a lookup over the bound: %v", err)
	}
	if got, err := s.ListMirrorDetailStates(root, slices.Clone(ids[:mirrorMaxLookup])); err != nil || len(got) != 0 {
		t.Fatalf("a lookup at the bound: %v, %v", got, err)
	}
}
