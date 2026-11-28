package store

import (
	"slices"
	"strings"
	"testing"
)

// applyChange applies read to its source and returns what it changed of
// what credit counts.
func applyChange(t *testing.T, s *Store, read ForgeHistoryRead) ForgeHistoryChange {
	t.Helper()
	read.SourceID = sourceID(t, s, read.Source)
	if read.ReadAt == 0 {
		read.ReadAt = 5_000_000
	}
	if read.WindowStart == 0 {
		read.WindowStart = 1
	}
	change, err := s.ApplyForgeHistory(read)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

// A read reports the keys of the pull requests whose acts credit reads
// changed: an act added, moved earlier or named, a key or a head
// repository changed on a pull request that has acts, and a pull request
// with acts pruned. A read that stores what is stored reports nothing, as
// does a change to a pull request with no act.
func TestApplyForgeHistoryReportsTheKeysWhoseActsChanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}, nil); err != nil {
		t.Fatal(err)
	}
	reviewed, plain := testPR(7, "WEB-1 Retry", "retry", 900, "WEB-1"), testPR(8, "WEB-3 Plain", "plain", 900, "WEB-3")
	bob := approved(7, "bob", "Bob Li", 700)
	steps := []struct {
		name string
		prs  []ForgeHistoryPR
		acts []ForgeHistoryAct
		// window is the read's window start, 1 when 0.
		window int64
		want   []string
	}{
		{"a pull request with an act and one with none", []ForgeHistoryPR{reviewed, plain}, []ForgeHistoryAct{bob}, 0, []string{"WEB-1"}},
		{"the same read again", []ForgeHistoryPR{reviewed, plain}, []ForgeHistoryAct{bob}, 0, nil},
		{"the act at a later time", nil, []ForgeHistoryAct{approved(7, "bob", "Bob Li", 750)}, 0, nil},
		{"the act at an earlier time", nil, []ForgeHistoryAct{approved(7, "bob", "Bob Li", 650)}, 0, []string{"WEB-1"}},
		{"an act of a person the forge does not name", nil, []ForgeHistoryAct{commented(7, "carol", "", 700)}, 0, []string{"WEB-1"}},
		{"the person named", nil, []ForgeHistoryAct{commented(7, "carol", "Carol Wu", 700)}, 0, []string{"WEB-1"}},
		{"the person named by login alone again", nil, []ForgeHistoryAct{commented(7, "carol", "", 700)}, 0, nil},
		{"a key added to the pull request with acts", []ForgeHistoryPR{testPR(7, "WEB-1 WEB-2 Retry", "retry", 900, "WEB-1", "WEB-2")}, nil, 0, []string{"WEB-2"}},
		{"its keys changed", []ForgeHistoryPR{testPR(7, "WEB-2 WEB-4 Retry", "retry", 900, "WEB-2", "WEB-4")}, nil, 0, []string{"WEB-1", "WEB-4"}},
		{"the keys of the pull request with no act changed", []ForgeHistoryPR{testPR(8, "WEB-5 Plain", "plain", 900, "WEB-5")}, nil, 0, nil},
		{"its head repository changed", []ForgeHistoryPR{func() ForgeHistoryPR {
			pr := testPR(7, "WEB-2 WEB-4 Retry", "retry", 900, "WEB-2", "WEB-4")
			pr.HeadRepo = "R_fork"
			return pr
		}()}, nil, 0, []string{"WEB-2", "WEB-4"}},
		{"the window passes both", nil, nil, 2000, []string{"WEB-2", "WEB-4"}},
	}
	for _, step := range steps {
		change := applyChange(t, s, ForgeHistoryRead{Source: testOrigin, PRs: step.prs, Acts: step.acts, WindowStart: step.window})
		if !slices.Equal(change.Keys, step.want) || change.State {
			t.Fatalf("%s: changed %+v; want keys %v and no state", step.name, change, step.want)
		}
		if change.Empty() != (len(step.want) == 0) {
			t.Fatalf("%s: Empty() = %t for %+v", step.name, change.Empty(), change)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM forge_history_prs`, "0")
}

// A read reports a change of what credit reads of the source itself: how
// far it has read, whether acts are owed, its failure, and its own head
// and parent. The read's time, its walk, a good read after a good read
// and the account whose token read it are no such change.
func TestApplyForgeHistoryReportsAChangeOfTheSourcesState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}, nil); err != nil {
		t.Fatal(err)
	}
	walked := func(edit func(*ForgeHistoryRead)) ForgeHistoryRead {
		r := ForgeHistoryRead{Source: testOrigin, Walk: []byte(`{}`), HasReach: true, Reach: 500}
		if edit != nil {
			edit(&r)
		}
		return r
	}
	for _, step := range []struct {
		name string
		read ForgeHistoryRead
		want bool
	}{
		{"a read that reads nothing", ForgeHistoryRead{Source: testOrigin}, false},
		{"the first reach", walked(nil), true},
		{"the same reach, later, with another walk", walked(func(r *ForgeHistoryRead) { r.ReadAt, r.Walk = 6_000_000, []byte(`{"a":1}`) }), false},
		{"a reach further back", walked(func(r *ForgeHistoryRead) { r.Reach = 0 }), true},
		{"acts left unread", walked(func(r *ForgeHistoryRead) { r.Reach, r.ActsOwed = 0, true }), true},
		{"work owed that is no act", walked(func(r *ForgeHistoryRead) { r.Reach, r.ActsOwed, r.Owed = 0, true, true }), false},
		{"the acts read", walked(func(r *ForgeHistoryRead) { r.Reach = 0 }), true},
		{"no reach any more", walked(func(r *ForgeHistoryRead) { r.HasReach, r.Reach = false, 0 }), true},
		{"a failure", ForgeHistoryRead{Source: testOrigin, FailureKind: "auth", Failure: "GitHub refused the token"}, true},
		{"the same failure", ForgeHistoryRead{Source: testOrigin, FailureKind: "auth", Failure: "GitHub refused the token"}, false},
		{"another sentence", ForgeHistoryRead{Source: testOrigin, FailureKind: "auth", Failure: "GitHub refused the token for acme"}, true},
		{"the failure cleared", ForgeHistoryRead{Source: testOrigin}, true},
		{"its own head", ForgeHistoryRead{Source: testOrigin, OwnHead: "R_acme"}, true},
		{"its parent", ForgeHistoryRead{Source: testOrigin, OwnHead: "R_acme", Parent: testParent}, true},
		{"the same head and parent", ForgeHistoryRead{Source: testOrigin, OwnHead: "R_acme", Parent: testParent}, false},
		{"the account whose token read it", ForgeHistoryRead{Source: testOrigin, Viewer: "user9"}, false},
	} {
		if change := applyChange(t, s, step.read); change.State != step.want || len(change.Keys) != 0 {
			t.Fatalf("%s: changed %+v; want state %t", step.name, change, step.want)
		}
	}
	src, ok, err := s.GetForgeHistorySource(sourceID(t, s, testOrigin))
	if err != nil || !ok || src.Viewer != "user9" || src.GoodAt != 5_000_000 || src.ActsOwed || src.HasReach {
		t.Fatalf("source %+v, %v, %v; want the viewer, the last good read's time and no act owed", src, ok, err)
	}
	// A failed read keeps the time of the last good one, and a read that
	// names no viewer keeps the stored one.
	applyChange(t, s, ForgeHistoryRead{Source: testOrigin, ReadAt: 7_000_000, FailureKind: "auth", Failure: "GitHub refused the token"})
	if src, _, err := s.GetForgeHistorySource(src.ID); err != nil || src.GoodAt != 5_000_000 || src.Viewer != "user9" || src.FailedAt != 7_000_000 {
		t.Fatalf("after a failed read %+v, %v; want the good read's time and the viewer kept", src, err)
	}
}

// The sources are set with the repositories each connection's credit
// reads, and the connections whose credit reads other sources than
// before are reported: a use added or removed, and a fork's parent that
// became a source or stopped being one.
func TestSetForgeHistorySourcesReportsTheConnectionsWhoseSourcesMoved(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	a := mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user1@example.com", 1)).ID
	b := mustCreateTrackerConnection(t, s, jiraConnection("https://example2.atlassian.net", "user1@example.com", 2)).ID
	use := func(conn string, ref ForgeHistoryRef) ForgeHistoryUse {
		return ForgeHistoryUse{ConnectionID: conn, RepoKey: ref.RepoKey}
	}
	set := func(name string, wanted []ForgeHistoryRef, uses []ForgeHistoryUse, changed bool, want ...string) {
		t.Helper()
		slices.Sort(want)
		got, connections, err := s.SetForgeHistorySources(wanted, uses)
		if err != nil || got != changed || !slices.Equal(connections, want) {
			t.Fatalf("%s: changed %t, connections %v, %v; want %t and %v", name, got, connections, err, changed, want)
		}
	}
	origin, both := []ForgeHistoryRef{testOrigin}, []ForgeHistoryRef{testOrigin, testOther}
	set("the first use", origin, []ForgeHistoryUse{use(a, testOrigin)}, true, a)
	set("the same again", origin, []ForgeHistoryUse{use(a, testOrigin)}, false)
	set("a second connection's repository", both, []ForgeHistoryUse{use(a, testOrigin), use(b, testOther)}, true, b)
	set("a use of a connection and of a repository that are not stored", both,
		[]ForgeHistoryUse{use(a, testOrigin), use(b, testOther), use("gone", testOrigin), use(a, testParent)}, false)
	stored, err := s.ListForgeHistoryUses()
	slices.SortFunc(stored, func(x, y ForgeHistoryUse) int { return strings.Compare(x.RepoKey, y.RepoKey) })
	if err != nil || !slices.Equal(stored, []ForgeHistoryUse{use(a, testOrigin), use(b, testOther)}) {
		t.Fatalf("uses %+v, %v", stored, err)
	}
	set("a repository both use", both, []ForgeHistoryUse{use(a, testOrigin), use(b, testOther), use(b, testOrigin)}, false, b)

	// The origin is a fork: its parent counts for both once it is a source.
	fork := ForgeHistoryRead{Source: testOrigin, OwnHead: "R_acme", Parent: testParent}
	if change := applyChange(t, s, fork); !change.State {
		t.Fatalf("the read that found the parent reported %+v", change)
	}
	uses := []ForgeHistoryUse{use(a, testOrigin), use(b, testOther), use(b, testOrigin)}
	set("the fork's parent becomes a source", []ForgeHistoryRef{testOrigin, testOther, testParent}, uses, true, a, b)
	for ref, want := range map[ForgeHistoryRef][]string{testOrigin: {a, b}, testOther: {b}, testParent: {a, b}} {
		users, err := s.ForgeHistoryUsers(sourceID(t, s, ref))
		slices.Sort(want)
		if err != nil || !slices.Equal(users, want) {
			t.Fatalf("users of %s = %v, %v; want %v", ref.RepoKey, users, err, want)
		}
	}
	set("a use removed", []ForgeHistoryRef{testOrigin, testOther, testParent}, uses[:2], false, b)
	set("the parent removed", both, uses[:2], true, a)
	set("a source removed with its use", origin, uses[:2], true, b)
	if uses, err := s.ListForgeHistoryUses(); err != nil || !slices.Equal(uses, []ForgeHistoryUse{use(a, testOrigin)}) {
		t.Fatalf("uses at the end %+v, %v", uses, err)
	}
}
