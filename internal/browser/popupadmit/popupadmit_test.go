package popupadmit

import (
	"slices"
	"testing"
)

// What ended a popup before its listing decides the listing, whatever the
// order of the observations and whether they came before its
// announcement: a close refuses it, a crash alone lists it crashed.
func TestCommitReadsWhatEndedThePopup(t *testing.T) {
	cases := []struct {
		name    string
		early   []Observation
		pending []Observation
		want    VerdictKind
		reason  string
	}{
		{"nothing", nil, nil, List, ""},
		{"a crash", nil, []Observation{Crashed("oom")}, ListCrashed, "oom"},
		{"a close", nil, []Observation{Closed()}, Refuse, ""},
		{"a crash, then a close", nil, []Observation{Crashed("oom"), Closed()}, Refuse, ""},
		{"a close, then a crash", nil, []Observation{Closed(), Crashed("oom")}, Refuse, ""},
		{"a close before its announcement", []Observation{Closed()}, nil, Refuse, ""},
		{"a crash before, a close after its announcement", []Observation{Crashed("oom")}, []Observation{Closed()}, Refuse, ""},
		{"a crash before its announcement", []Observation{Crashed("gone")}, nil, ListCrashed, "gone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(8)
			for _, o := range tc.early {
				if got := r.Observe("p", o); got != Unheld {
					t.Fatalf("an observation before the announcement was %v", got)
				}
			}
			if !r.Announce("p", "opener") {
				t.Fatal("the first announcement was refused")
			}
			for _, o := range tc.pending {
				if got := r.Observe("p", o); got != Held {
					t.Fatalf("an observation of a pending popup was %v", got)
				}
			}
			v := r.Commit("p")
			if v.Kind() != tc.want || v.CrashReason() != tc.reason {
				t.Fatalf("commit = %v %q, want %v %q", v.Kind(), v.CrashReason(), tc.want, tc.reason)
			}
			want := Listed
			if tc.want == Refuse {
				want = Ended
			}
			if d, _ := r.Disposition("p"); d != want {
				t.Fatalf("the admission is %v after the commit, want %v", d, want)
			}
			if got := r.Observe("p", Closed()); got != Unheld {
				t.Fatalf("an observation after the commit was %v", got)
			}
			if again := r.Commit("p"); again.Kind() != Refuse {
				t.Fatalf("a second commit answered %v", again.Kind())
			}
		})
	}
}

// An admission ends once: a discard ends a pending one and changes
// nothing after a listing, a popup is announced once, and a commit of an
// unknown or discarded popup refuses it.
func TestAnAdmissionEndsOnce(t *testing.T) {
	r := New(8)
	if r.Commit("unknown").Kind() != Refuse {
		t.Fatal("an unknown popup was listed")
	}
	r.Announce("a", "o")
	if r.Announce("a", "o") {
		t.Fatal("a popup was announced twice")
	}
	r.Discard("a")
	if d, _ := r.Disposition("a"); d != Ended {
		t.Fatalf("a discarded popup is %v", d)
	}
	if r.Commit("a").Kind() != Refuse {
		t.Fatal("a discarded popup was listed")
	}
	r.Announce("b", "o")
	r.Commit("b")
	r.Discard("b")
	if d, _ := r.Disposition("b"); d != Listed {
		t.Fatalf("a discard after the listing moved the popup to %v", d)
	}
}

// A popup's ancestors are its opener chain, nearest first, through
// popups listed, ended or pending, each once even in a cycle.
func TestAncestorsFollowTheOpenerChain(t *testing.T) {
	r := New(8)
	r.Announce("child", "parent")
	r.Announce("parent", "grandparent")
	r.Announce("grandparent", "tab")
	r.Discard("parent")
	if got := r.Ancestors("child"); !slices.Equal(got, []Handle{"parent", "grandparent", "tab"}) {
		t.Fatalf("ancestors = %v", got)
	}
	r.Announce("x", "y")
	r.Announce("y", "x")
	if got := r.Ancestors("x"); !slices.Equal(got, []Handle{"y"}) {
		t.Fatalf("ancestors in a cycle = %v", got)
	}
}

// Settled admissions and early observations are kept within the limit,
// oldest first out; a pending admission is never dropped for room.
func TestTheRegistryIsBounded(t *testing.T) {
	r := New(2)
	r.Announce("pending", "o")
	for _, h := range []Handle{"a", "b", "c"} {
		r.Announce(h, "o")
		r.Discard(h)
		r.Observe("early-"+h, Closed())
	}
	if _, ok := r.Disposition("a"); ok {
		t.Fatal("the oldest settled admission was kept past the limit")
	}
	if _, ok := r.Disposition("c"); !ok {
		t.Fatal("the newest settled admission was dropped")
	}
	if d, ok := r.Disposition("pending"); !ok || d != Pending {
		t.Fatal("a pending admission was dropped for room")
	}
	if len(r.early) != 2 {
		t.Fatalf("kept %d early observations, want 2", len(r.early))
	}
}
