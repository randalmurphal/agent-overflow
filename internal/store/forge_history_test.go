package store

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var (
	testOrigin = ForgeHistoryRef{RepoKey: "github:acme/web", Forge: "github", Host: "github.com", Namespace: "acme", Repo: "web"}
	testParent = ForgeHistoryRef{RepoKey: "github:up/web", KeepHead: "acme/web", Forge: "github", Host: "github.com", Namespace: "up", Repo: "web"}
)

const testWindow = int64(1_000_000)

func testPR(n int64, title, branch string, updated int64, keys ...string) ForgeHistoryPR {
	return ForgeHistoryPR{
		Number: n, URL: "https://github.com/acme/web/pull/" + strings.Repeat("1", int(n%3)+1), Title: title, HeadRef: branch, HeadRepo: "acme/web",
		State: "open", Author: "alice", AuthorName: "Alice Ng", CreatedAt: updated - 10, UpdatedAt: updated, Keys: keys,
	}
}

func applyForge(t *testing.T, s *Store, read ForgeHistoryRead) {
	t.Helper()
	if read.ReadAt == 0 {
		read.ReadAt = 5_000_000
	}
	if read.WindowStart == 0 {
		read.WindowStart = testWindow
	}
	if err := s.ApplyForgeHistory(read); err != nil {
		t.Fatal(err)
	}
}

func forgeActs(t *testing.T, s *Store, number int64) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT a.kind, a.login, a.at FROM forge_history_acts a
		JOIN forge_history_sources src ON src.id = a.source_id WHERE src.repo_key = ? AND a.number = ? ORDER BY a.kind, a.login`, testOrigin.RepoKey, number)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind int
		var login string
		var at int64
		if err := rows.Scan(&kind, &login, &at); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join([]string{[]string{"", "approved", "commented"}[kind], login, strconv.FormatInt(at, 10)}, " "))
	}
	return out
}

func numbersOf(rows []ForgeHistoryRow) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Number)
	}
	return out
}

func TestForgeHistoryStoresPullRequestsByKeyAndReplacesTheirActsWhole(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
		t.Fatal(err)
	}
	pr := testPR(7, "WEB-1 Retry", "web-1-retry", 2_000_000, "WEB-1")
	pr.ActsWhole = true
	pr.Acts = []ForgeHistoryAct{
		{Number: 7, Kind: ForgeActApproved, Login: "bob", Name: "Bob Li", At: 2_000_000},
		{Number: 7, Kind: ForgeActCommented, Login: "carol", At: 1_900_000},
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{pr}, Walk: []byte(`{}`), Reach: 0, Walked: true, OwnHead: "acme/web"})
	rows, err := s.ForgeHistoryByKey("web-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := numbersOf(rows); !slices.Equal(got, []int64{7}) || rows[0].Title != "WEB-1 Retry" || rows[0].Source.RepoKey != testOrigin.RepoKey {
		t.Fatalf("by key = %+v", rows)
	}
	if got := forgeActs(t, s, 7); !slices.Equal(got, []string{"approved bob 2000000", "commented carol 1900000"}) {
		t.Fatalf("acts = %v", got)
	}

	// A re-read: bob withdrew his approval, the title no longer names
	// WEB-1 but WEB-2.
	pr = testPR(7, "WEB-2 Retry", "retry", 2_100_000, "WEB-2")
	pr.ActsWhole = true
	pr.Acts = []ForgeHistoryAct{{Number: 7, Kind: ForgeActCommented, Login: "carol", At: 1_900_000}}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{pr}})
	if got := forgeActs(t, s, 7); !slices.Equal(got, []string{"commented carol 1900000"}) {
		t.Fatalf("acts after the re-read = %v", got)
	}
	for key, want := range map[string][]int64{"WEB-1": {}, "WEB-2": {7}} {
		rows, err := s.ForgeHistoryByKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if got := numbersOf(rows); !slices.Equal(got, want) {
			t.Fatalf("%s lists %v, want %v", key, got, want)
		}
	}
}

func TestForgeHistoryEventsAddTheEarliestActToStoredPullRequestsOnly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
		t.Fatal(err)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{testPR(3, "WEB-1 a", "a", 2_000_000, "WEB-1")}})
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Acts: []ForgeHistoryAct{
		{Number: 3, Kind: ForgeActApproved, Login: "bob", At: 2_500_000},
		{Number: 3, Kind: ForgeActApproved, Login: "bob", At: 2_400_000},
		{Number: 99, Kind: ForgeActApproved, Login: "bob", At: 2_400_000},
	}})
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Acts: []ForgeHistoryAct{{Number: 3, Kind: ForgeActApproved, Login: "bob", At: 2_600_000}}})
	if got := forgeActs(t, s, 3); !slices.Equal(got, []string{"approved bob 2400000"}) {
		t.Fatalf("acts = %v", got)
	}
	if got := forgeActs(t, s, 99); len(got) != 0 {
		t.Fatalf("an act of a pull request never stored was kept: %v", got)
	}
	// A list re-read without ActsWhole keeps the events' acts.
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{testPR(3, "WEB-1 a", "a", 2_700_000, "WEB-1")}})
	if got := forgeActs(t, s, 3); !slices.Equal(got, []string{"approved bob 2400000"}) {
		t.Fatalf("acts after a list re-read = %v", got)
	}
}

func TestForgeHistoryParentKeepsOnlyTheForksPullRequests(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin, testParent}); err != nil {
		t.Fatal(err)
	}
	fork := testPR(10, "WEB-1 from the fork", "web-1-fix", 2_000_000, "WEB-1")
	other := testPR(11, "WEB-1 from elsewhere", "web-1-other", 2_000_000, "WEB-1")
	other.HeadRepo = "someone/web"
	applyForge(t, s, ForgeHistoryRead{Source: testParent, PRs: []ForgeHistoryPR{fork, other}, Acts: []ForgeHistoryAct{
		{Number: 10, Kind: ForgeActApproved, Login: "bob", At: 2_000_000},
		{Number: 11, Kind: ForgeActApproved, Login: "bob", At: 2_000_000},
	}})
	rows, err := s.ForgeHistoryByKey("WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := numbersOf(rows); !slices.Equal(got, []int64{10}) || rows[0].Source.KeepHead != "acme/web" {
		t.Fatalf("by key = %+v", rows)
	}
	known, err := s.KnownForgeHistoryPRs(testParent, []int64{10, 11, 12})
	if err != nil {
		t.Fatal(err)
	}
	if !known[10] || !known[11] || known[12] {
		t.Fatalf("known = %v", known)
	}
	var kept, title string
	var acts int
	if err := s.db.QueryRow(`SELECT p.kept, p.title, (SELECT count(*) FROM forge_history_acts a WHERE a.source_id = p.source_id AND a.number = 11)
		FROM forge_history_prs p WHERE p.number = 11`).Scan(&kept, &title, &acts); err != nil {
		t.Fatal(err)
	}
	if kept != "0" || title != "" || acts != 0 {
		t.Fatalf("the parent's other pull request is stored as kept %s title %q with %d acts", kept, title, acts)
	}
}

func TestForgeHistoryPrunesWhatLeftTheWindowAndRemovedSources(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
		t.Fatal(err)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{
		testPR(1, "WEB-1 old", "a", 500_000, "WEB-1"),
		testPR(2, "WEB-1 old, approved since", "b", 500_000, "WEB-1"),
		testPR(3, "WEB-1 new", "c", 2_000_000, "WEB-1"),
	}, WindowStart: 100_000})
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Acts: []ForgeHistoryAct{{Number: 2, Kind: ForgeActApproved, Login: "bob", At: 1_500_000}}, WindowStart: 100_000})
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, WindowStart: testWindow})
	rows, err := s.ForgeHistoryByKey("WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := numbersOf(rows); !slices.Equal(got, []int64{2, 3}) {
		t.Fatalf("after the prune: %v", got)
	}
	changed, err := s.SetForgeHistorySources(nil)
	if err != nil || !changed {
		t.Fatalf("remove = %v, %v", changed, err)
	}
	for _, table := range []string{"forge_history_prs", "forge_history_acts", "forge_history_keys", "forge_history_sources"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s keeps %d rows of a removed source", table, n)
		}
	}
	err = s.ApplyForgeHistory(ForgeHistoryRead{Source: testOrigin, ReadAt: 1, WindowStart: 1})
	if !errors.Is(err, ErrForgeHistoryGone) {
		t.Fatalf("a read of a removed source = %v", err)
	}
}

func TestForgeHistoryRecordsTheWalkAndTheFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
		t.Fatal(err)
	}
	source := func() ForgeHistorySource {
		t.Helper()
		all, err := s.ListForgeHistorySources()
		if err != nil || len(all) != 1 {
			t.Fatalf("sources = %v, %v", all, err)
		}
		return all[0]
	}
	if src := source(); src.HasReach || src.Covers(5_000_000, testWindow) {
		t.Fatalf("a source never read covers: %+v", src)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Walk: []byte(`{"at":1}`), Reach: 3_000_000, OwnHead: "acme/web", ParentNamespace: "up", ParentRepo: "web"})
	src := source()
	margin := ForgeHistoryClockMargin.Milliseconds()
	switch {
	case !src.HasReach || src.Reach != 3_000_000 || src.Walked || string(src.Walk) != `{"at":1}` || src.OwnHead != "acme/web" || src.ParentRepo != "web":
		t.Fatalf("after a first read: %+v", src)
	case !src.Covers(3_000_000+margin, testWindow) || src.Covers(3_000_000+margin-1, testWindow):
		t.Fatal("a source covers a ticket by its reach and the margin")
	}
	// A failed read that read nothing keeps the walk.
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, FailureKind: "rate_limited", Failure: "GitHub rate limit reached",
		FailureDetail: []byte(`{"reserve":true}`), ReadAt: 6_000_000})
	src = source()
	if src.Reach != 3_000_000 || src.Failure != "GitHub rate limit reached" || string(src.FailureDetail) != `{"reserve":true}` ||
		src.FailedAt != 6_000_000 || src.ReadAt != 6_000_000 {
		t.Fatalf("after a failure: %+v", src)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Walk: []byte(`{"at":2}`), Reach: 0, Walked: true, ReadAt: 7_000_000})
	src = source()
	switch {
	case src.Failure != "" || src.FailureDetail != nil || src.FailedAt != 0 || !src.Walked || src.Reach != 0:
		t.Fatalf("after a full read: %+v", src)
	case !src.Covers(testWindow, testWindow) || src.Covers(testWindow-1, testWindow):
		t.Fatal("a full read covers every ticket created inside the window, and none before it")
	}
	// A read left behind its newest updates covers nothing until one is
	// not.
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Walk: []byte(`{"at":3}`), Walked: true, Behind: true, ReadAt: 8_000_000})
	if src = source(); src.HasReach || src.Covers(5_000_000, testWindow) || !src.Walked {
		t.Fatalf("after a read left behind: %+v", src)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Walk: []byte(`{"at":4}`), Walked: true, ReadAt: 9_000_000})
	if src = source(); !src.HasReach || !src.Covers(5_000_000, testWindow) {
		t.Fatalf("after a read that caught up: %+v", src)
	}
}

// Each statement of the writer reaches the stored rows by their keys
// from its read's list, so a read costs its own rows however many the
// source holds.
func TestForgeHistoryWriterPlans(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for name, query := range map[string]string{
		"pull requests": forgeHistoryPRsSQL, "clear keys": forgeHistoryClearKeysSQL, "keys": forgeHistoryKeysSQL,
		"clear acts": forgeHistoryClearActsSQL, "acts": forgeHistoryActsSQL, "prune": forgeHistoryPruneSQL,
	} {
		plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+query, 1, "[]")
		t.Logf("%s:\n%s", name, plan)
		// A walk of the source's rows: a scan, or a search by the source
		// alone.
		for _, line := range strings.Split(plan, "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "SCAN forge_history") || strings.HasSuffix(line, "(source_id=?)") {
				t.Errorf("%s walks the source's rows: %s", name, line)
			}
		}
	}
}
