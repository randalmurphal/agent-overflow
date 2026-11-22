package popupadmit

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/browser/ids"
)

// What ended a popup before its listing decides the listing, whatever the
// order of the observations: a close refuses it, a crash alone lists it
// crashed. Attached or not, a crash alone lists it crashed; with no crash,
// a popup whose attach failed is refused.
func TestCommitReadsWhatEndedThePopup(t *testing.T) {
	cases := []struct {
		name       string
		pending    []Observation
		want       VerdictKind
		unattached VerdictKind
		reason     string
	}{
		{"nothing", nil, List, Refuse, ""},
		{"a crash", []Observation{Crashed("oom")}, ListCrashed, ListCrashed, "oom"},
		{"a close", []Observation{Closed()}, Refuse, Refuse, ""},
		{"a crash, then a close", []Observation{Crashed("oom"), Closed()}, Refuse, Refuse, ""},
		{"a close, then a crash", []Observation{Closed(), Crashed("oom")}, Refuse, Refuse, ""},
	}
	for _, tc := range cases {
		for _, attached := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, attached %v", tc.name, attached), func(t *testing.T) {
				r := New(8)
				if !r.Announce("p", "opener") {
					t.Fatal("the first announcement was refused")
				}
				for _, o := range tc.pending {
					if got := r.Observe("p", o); got != Held {
						t.Fatalf("an observation of a pending popup was %v", got)
					}
				}
				want := tc.want
				if !attached {
					want = tc.unattached
				}
				v := r.Commit("p", !attached)
				wantReason := tc.reason
				if want != ListCrashed {
					wantReason = ""
				}
				if v.Kind() != want || v.CrashReason() != wantReason {
					t.Fatalf("commit = %v %q, want %v %q", v.Kind(), v.CrashReason(), want, wantReason)
				}
				if closed := strings.Contains(tc.name, "close"); v.Closed() != closed {
					t.Fatalf("commit closed = %v, want %v", v.Closed(), closed)
				}
				wantDisposition := Ended
				if want == List {
					wantDisposition = Listed
				}
				if d, _ := r.Disposition("p"); d != wantDisposition {
					t.Fatalf("the admission is %v after the commit, want %v", d, wantDisposition)
				}
				if got := r.Observe("p", Closed()); got != Unheld {
					t.Fatalf("an observation after the commit was %v", got)
				}
				if again := r.Commit("p", false); again.Kind() != Refuse {
					t.Fatalf("a second commit answered %v", again.Kind())
				}
			})
		}
	}
}

// An admission ends once: a discard ends a pending one and changes
// nothing after a listing, a listed one ends when its tab leaves, a popup
// is announced once, and a commit of an unknown or discarded popup refuses
// it. An observation of a handle never announced is the caller's, and
// nothing keeps it for an announcement.
func TestAnAdmissionEndsOnce(t *testing.T) {
	r := New(8)
	if r.Commit("unknown", false).Kind() != Refuse {
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
	if r.Commit("a", false).Kind() != Refuse {
		t.Fatal("a discarded popup was listed")
	}
	if r.Announce("a", "o") {
		t.Fatal("an ended popup was announced again")
	}
	r.Announce("b", "o")
	r.Commit("b", false)
	r.Discard("b")
	if d, _ := r.Disposition("b"); d != Listed {
		t.Fatalf("a discard after the listing moved the popup to %v", d)
	}
	r.Leave("b")
	if d, _ := r.Disposition("b"); d != Ended {
		t.Fatalf("a popup whose tab left is %v", d)
	}
	if got := r.Observe("never", Closed()); got != Unheld {
		t.Fatalf("an observation of a handle never announced was %v", got)
	}
	r.Announce("never", "o")
	if v := r.Commit("never", false); v.Kind() != List {
		t.Fatalf("a popup observed before its announcement commits as %v; an engine announces first, and nothing is kept", v.Kind())
	}
}

// A popup's ancestors are its opener chain, nearest first, through popups
// listed, ended or pending, each once even in a cycle; a handle the
// registry never had has an unknown ancestry.
func TestAncestorsFollowTheOpenerChain(t *testing.T) {
	r := New(8)
	r.Announce("child", "parent")
	r.Announce("parent", "grandparent")
	r.Announce("grandparent", "tab")
	r.Discard("parent")
	if got := r.Ancestors("child"); !got.Known() || !slices.Equal(got.Chain(), []ids.PageHandle{"parent", "grandparent", "tab"}) {
		t.Fatalf("ancestors = %v", got)
	}
	r.Announce("x", "y")
	r.Announce("y", "x")
	if got := r.Ancestors("x"); !got.Known() || !slices.Equal(got.Chain(), []ids.PageHandle{"y"}) {
		t.Fatalf("ancestors in a cycle = %v", got)
	}
	if got := r.Ancestors("stranger"); got.Known() || got.Chain() != nil {
		t.Fatalf("a handle never announced has ancestors %v", got)
	}
}

// A live popup keeps its chain however many other popups come and go: a
// listed parent and every ancestor above it, while a child of it is
// pending, even after the parent's tab left. Once nothing depends on it,
// an ended admission goes to the history, which keeps the newest within
// its limit, a child before its parent, and answers unknown past it.
func TestALiveChainOutlivesTurnover(t *testing.T) {
	const limit = 4
	r := New(limit)
	r.Announce("parent", "tab")
	r.Commit("parent", false)
	r.Announce("child", "parent")
	churn := func(prefix string) {
		for i := range 8 * limit {
			h := ids.PageHandle(fmt.Sprintf("%s-%d", prefix, i))
			r.Announce(h, "elsewhere")
			if i%2 == 0 {
				r.Discard(h)
			} else {
				r.Commit(h, false)
				r.Leave(h)
			}
		}
	}
	churn("a")
	want := []ids.PageHandle{"parent", "tab"}
	if got := r.Ancestors("child"); !got.Known() || !slices.Equal(got.Chain(), want) {
		t.Fatalf("after turnover, the pending child's ancestors are %v, want %v", got, want)
	}
	r.Leave("parent")
	churn("b")
	if got := r.Ancestors("child"); !got.Known() || !slices.Equal(got.Chain(), want) {
		t.Fatalf("after its parent's tab left and more turnover, the pending child's ancestors are %v, want %v", got, want)
	}
	if live, kept := retained(r); live != 2 || kept != limit {
		t.Fatalf("the registry keeps %d live and %d settled admissions, want 2 and %d", live, kept, limit)
	}
	r.Discard("child")
	if got := r.Ancestors("child"); !got.Known() || !slices.Equal(got.Chain(), want) {
		t.Fatalf("just after its end, the child's ancestors are %v, want %v", got, want)
	}
	churn("c")
	if got := r.Ancestors("child"); got.Known() {
		t.Fatalf("past the history's limit, the child's ancestors are still %v", got)
	}
	if live, kept := retained(r); live != 0 || kept != limit {
		t.Fatalf("the registry keeps %d live and %d settled admissions, want 0 and %d", live, kept, limit)
	}
}

// A popup announced with an opener that already went to the history makes
// that opener live again, with its own chain, while the popup is.
func TestAPopupOfAnEndedOpenerKeepsItsChain(t *testing.T) {
	const limit = 2
	r := New(limit)
	r.Announce("parent", "tab")
	r.Discard("parent")
	r.Announce("late", "parent")
	for i := range 4 * limit {
		h := ids.PageHandle(fmt.Sprintf("other-%d", i))
		r.Announce(h, "elsewhere")
		r.Discard(h)
	}
	if got := r.Ancestors("late"); !got.Known() || !slices.Equal(got.Chain(), []ids.PageHandle{"parent", "tab"}) {
		t.Fatalf("the late popup's ancestors are %v", got)
	}
}

// retained answers how many admissions r has live and how many its history
// keeps.
func retained(r *Registry) (live, kept int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live), len(r.history.entries)
}
