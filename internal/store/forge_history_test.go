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
	testParent = ForgeHistoryRef{RepoKey: "github:up/web", Forge: "github", Host: "github.com", Namespace: "up", Repo: "web"}
	testOther  = ForgeHistoryRef{RepoKey: "github:other/web", Forge: "github", Host: "github.com", Namespace: "other", Repo: "web"}
)

const testWindow = int64(1_000_000)

func testPR(n int64, title, branch string, updated int64, keys ...string) ForgeHistoryPR {
	return ForgeHistoryPR{
		Number: n, URL: "https://github.com/acme/web/pull/" + strings.Repeat("1", int(n%3)+1), Title: title, HeadRef: branch, HeadRepo: "R_acme",
		State: "open", Author: "alice", AuthorName: "Alice Ng", CreatedAt: updated - 10, UpdatedAt: updated, SeenAt: updated + 5, Keys: keys,
	}
}

// sourceID is the id of the stored source of ref.
func sourceID(t *testing.T, s *Store, ref ForgeHistoryRef) int64 {
	t.Helper()
	all, err := s.ListForgeHistorySources()
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range all {
		if src.RepoKey == ref.RepoKey {
			return src.ID
		}
	}
	t.Fatalf("%s is not stored", ref.RepoKey)
	return 0
}

func applyForge(t *testing.T, s *Store, read ForgeHistoryRead) {
	t.Helper()
	if read.SourceID == 0 {
		read.SourceID = sourceID(t, s, read.Source)
	}
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

func forgeActs(t *testing.T, s *Store, ref ForgeHistoryRef, number int64) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT a.kind, a.login, a.name, a.at FROM forge_history_acts a
		JOIN forge_history_sources src ON src.id = a.source_id WHERE src.repo_key = ? AND a.number = ? ORDER BY a.kind, a.login`, ref.RepoKey, number)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind int
		var login, name string
		var at int64
		if err := rows.Scan(&kind, &login, &name, &at); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.TrimSpace(strings.Join([]string{[]string{"", "approved", "commented"}[kind], login, strconv.FormatInt(at, 10), name}, " ")))
	}
	return out
}

// workRows are the numbers ForgeHistoryForWork lists for key and origins.
func workRows(t *testing.T, s *Store, key string, origins ...ForgeHistoryRef) []string {
	t.Helper()
	return workRowsNamed(t, s, key, nil, origins...)
}

// workRowsNamed are the numbers ForgeHistoryForWork lists for key,
// numbers and origins.
func workRowsNamed(t *testing.T, s *Store, key string, numbers []ForgeHistoryNumber, origins ...ForgeHistoryRef) []string {
	t.Helper()
	var keys []string
	for _, o := range origins {
		keys = append(keys, o.RepoKey)
	}
	work, err := s.ForgeHistoryForWork(key, keys, numbers)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]string{}
	for _, src := range work.Sources {
		byID[src.ID] = src.RepoKey
	}
	var out []string
	for _, r := range work.Rows {
		out = append(out, byID[r.SourceID]+"#"+strconv.FormatInt(r.Number, 10))
	}
	return out
}

// A pull request is stored with its keys and the start of the request
// that read it; its acts are only added: a later read that does not name
// an act (a dismissed approval, a comment past the read's page) or names
// a person by login alone removes nothing.
func TestForgeHistoryStoresPullRequestsByKeyAndOnlyAddsActs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
		t.Fatal(err)
	}
	pr := testPR(7, "WEB-1 Retry", "web-1-retry", 2_000_000, "WEB-1")
	pr.SeenAt = 3_900_000
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{pr}, Walk: []byte(`{}`), OwnHead: "R_acme", ReadAt: 4_000_000, Acts: []ForgeHistoryAct{
		{Number: 7, Kind: ForgeActApproved, Login: "bob", Name: "Bob Li", At: 2_000_000},
		{Number: 7, Kind: ForgeActCommented, Login: "carol", At: 1_900_000},
		{Number: 7, Kind: ForgeActCommented, Login: "dave", Name: "Dave Yu", At: 1_950_000},
	}})
	work, err := s.ForgeHistoryForWork("web-1", []string{testOrigin.RepoKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(work.Rows) != 1 || work.Rows[0].Number != 7 || work.Rows[0].Title != "WEB-1 Retry" || work.Rows[0].SeenAt != 3_900_000 || len(work.Sources) != 1 {
		t.Fatalf("by key = %+v", work)
	}

	// A re-read: bob's approval was dismissed, dave is named by login
	// alone, carol's comment is past the page, and the title names WEB-2.
	pr = testPR(7, "WEB-2 Retry", "retry", 2_100_000, "WEB-2")
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{pr}, Acts: []ForgeHistoryAct{
		{Number: 7, Kind: ForgeActCommented, Login: "dave", At: 1_950_000},
	}})
	if got := forgeActs(t, s, testOrigin, 7); !slices.Equal(got, []string{"approved bob 2000000 Bob Li", "commented carol 1900000", "commented dave 1950000 Dave Yu"}) {
		t.Fatalf("acts after the re-read = %v", got)
	}
	for key, want := range map[string][]string{"WEB-1": nil, "WEB-2": {"github:acme/web#7"}} {
		if got := workRows(t, s, key, testOrigin); !slices.Equal(got, want) {
			t.Fatalf("%s lists %v, want %v", key, got, want)
		}
	}
}

// Work's read lists, beside the pull requests that name the key, those it
// names by repository and number, whatever they name, under the same
// scope: an origin's, and a parent's whose head is the origin.
func TestForgeHistoryWorkReadsTheNumbersItNames(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin, testParent, testOther}); err != nil {
		t.Fatal(err)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, OwnHead: "R_acme", Parent: testParent, Walk: []byte(`{}`),
		PRs: []ForgeHistoryPR{testPR(1, "WEB-2 Retitled", "x", 2_000_000, "WEB-2"), testPR(2, "WEB-1 Named", "y", 2_000_000, "WEB-1")}})
	theirs := testPR(5, "Theirs", "z", 2_000_000)
	theirs.HeadRepo = "R_someone"
	applyForge(t, s, ForgeHistoryRead{Source: testParent, OwnHead: "R_up", PRs: []ForgeHistoryPR{testPR(4, "From the fork", "w", 2_000_000), theirs}})
	applyForge(t, s, ForgeHistoryRead{Source: testOther, OwnHead: "R_other", PRs: []ForgeHistoryPR{testPR(6, "Elsewhere", "v", 2_000_000)}})
	numbers := []ForgeHistoryNumber{{testOrigin.RepoKey, 1}, {testOrigin.RepoKey, 2}, {testParent.RepoKey, 4}, {testParent.RepoKey, 5}, {testOther.RepoKey, 6}, {testOrigin.RepoKey, 9}}
	if got := workRowsNamed(t, s, "WEB-1", numbers, testOrigin); !slices.Equal(got, []string{"github:acme/web#1", "github:acme/web#2", "github:up/web#4"}) {
		t.Fatalf("the origin's Work lists %v", got)
	}
}

// The reader's lookup answers what the source stores of the numbers it
// holds, and nothing of the others.
func TestForgeHistoryStoredAnswersTheUpdateAndSeenTimes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
		t.Fatal(err)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{testPR(3, "a", "a", 2_000_000)}})
	got, err := s.StoredForgeHistoryPRs(sourceID(t, s, testOrigin), []int64{3, 4})
	if err != nil || len(got) != 1 || got[3] != (StoredForgeHistoryPR{UpdatedAt: 2_000_000, SeenAt: 2_000_005}) {
		t.Fatalf("stored = %v, %v", got, err)
	}
	sources, err := s.ForgeHistorySourcesForWork([]string{testOrigin.RepoKey, testOther.RepoKey})
	if err != nil || len(sources) != 1 || sources[0].RepoKey != testOrigin.RepoKey {
		t.Fatalf("Work's sources = %+v, %v", sources, err)
	}
}

// An act stream's acts add each person's earliest act to stored pull
// requests, keep a name a read did not give, and an act whose pull
// request neither the read nor the store holds fails the write whole:
// the reader resolves every act's pull request before it moves past it.
func TestForgeHistoryActsAddTheEarliestAndRefuseOneOfNoPullRequest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
		t.Fatal(err)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{testPR(3, "WEB-1 a", "a", 2_000_000, "WEB-1")}})
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Acts: []ForgeHistoryAct{
		{Number: 3, Kind: ForgeActApproved, Login: "bob", Name: "Bob Li", At: 2_500_000},
		{Number: 3, Kind: ForgeActApproved, Login: "bob", At: 2_400_000},
	}})
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Acts: []ForgeHistoryAct{{Number: 3, Kind: ForgeActApproved, Login: "bob", At: 2_600_000}}})
	if got := forgeActs(t, s, testOrigin, 3); !slices.Equal(got, []string{"approved bob 2400000 Bob Li"}) {
		t.Fatalf("acts = %v", got)
	}
	err := s.ApplyForgeHistory(ForgeHistoryRead{SourceID: sourceID(t, s, testOrigin), Source: testOrigin, ReadAt: 6_000_000, WindowStart: testWindow,
		Walk: []byte(`{"moved":true}`), Acts: []ForgeHistoryAct{{Number: 99, Kind: ForgeActApproved, Login: "bob", At: 2_400_000}}})
	if err == nil || !strings.Contains(err.Error(), "pull request 99") {
		t.Fatalf("an act of a pull request never stored = %v", err)
	}
	if all, err := s.ListForgeHistorySources(); err != nil || string(all[0].Walk) != "{}" || all[0].ReadAt != 5_000_000 {
		t.Fatalf("the refused write moved the source: %+v, %v", all, err)
	}
	// A list re-read with no acts keeps the stored ones.
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, PRs: []ForgeHistoryPR{testPR(3, "WEB-1 a", "a", 2_700_000, "WEB-1")}})
	if got := forgeActs(t, s, testOrigin, 3); !slices.Equal(got, []string{"approved bob 2400000 Bob Li"}) {
		t.Fatalf("acts after a list re-read = %v", got)
	}
}

// A parent is stored whole, every head's pull requests with their acts,
// and Work's read lists of it only those whose head is one of Work's
// origins, and none of a repository Work's origins do not name.
func TestForgeHistoryWorkReadsItsOriginsAndTheirHeadsInAParent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin, testParent, testOther}); err != nil {
		t.Fatal(err)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, OwnHead: "R_acme", Parent: testParent, Walk: []byte(`{}`)})
	fork := testPR(10, "WEB-1 from the fork", "web-1-fix", 2_000_000, "WEB-1")
	other := testPR(11, "WEB-1 from elsewhere", "web-1-other", 2_000_000, "WEB-1")
	other.HeadRepo = "R_someone"
	applyForge(t, s, ForgeHistoryRead{Source: testParent, OwnHead: "R_up", PRs: []ForgeHistoryPR{fork, other}, Acts: []ForgeHistoryAct{
		{Number: 10, Kind: ForgeActApproved, Login: "bob", At: 2_000_000},
		{Number: 11, Kind: ForgeActApproved, Login: "bob", At: 2_000_000},
	}})
	applyForge(t, s, ForgeHistoryRead{Source: testOther, OwnHead: "R_other", PRs: []ForgeHistoryPR{testPR(12, "WEB-1 another repository", "x", 2_000_000, "WEB-1")}})

	if got := workRows(t, s, "WEB-1", testOrigin); !slices.Equal(got, []string{"github:up/web#10"}) {
		t.Fatalf("the fork's Work lists %v; want the parent's pull request whose head is the fork", got)
	}
	if got := workRows(t, s, "WEB-1", testParent); !slices.Equal(got, []string{"github:up/web#10", "github:up/web#11"}) {
		t.Fatalf("the parent's own Work lists %v; want both", got)
	}
	if got := workRows(t, s, "WEB-1", testOther); !slices.Equal(got, []string{"github:other/web#12"}) {
		t.Fatalf("the other repository's Work lists %v", got)
	}
	work, err := s.ForgeHistoryForWork("WEB-1", []string{testOrigin.RepoKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, src := range work.Sources {
		keys = append(keys, src.RepoKey)
	}
	if !slices.Equal(keys, []string{"github:acme/web", "github:up/web"}) || work.Sources[0].Parent != testParent {
		t.Fatalf("the fork's sources = %+v", work.Sources)
	}
	if got := forgeActs(t, s, testParent, 11); !slices.Equal(got, []string{"approved bob 2000000"}) {
		t.Fatalf("the other head's acts = %v", got)
	}

	// The parent no longer read as a fork's parent but as an origin of its
	// own: its id and rows stay.
	before := sourceID(t, s, testParent)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testParent}); err != nil {
		t.Fatal(err)
	}
	if id := sourceID(t, s, testParent); id != before {
		t.Fatalf("the parent's source was made again: %d, was %d", id, before)
	}
	if got := workRows(t, s, "WEB-1", testParent); len(got) != 2 {
		t.Fatalf("after the change the parent lists %v", got)
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
	if got := workRows(t, s, "WEB-1", testOrigin); !slices.Equal(got, []string{"github:acme/web#2", "github:acme/web#3"}) {
		t.Fatalf("after the prune: %v", got)
	}
	id := sourceID(t, s, testOrigin)
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
	err = s.ApplyForgeHistory(ForgeHistoryRead{SourceID: id, Source: testOrigin, ReadAt: 1, WindowStart: 1})
	if !errors.Is(err, ErrForgeHistoryGone) {
		t.Fatalf("a read of a removed source = %v", err)
	}
}

// A read that began before its source was removed lands in no source
// made again for the same repository: the new one has a new id, which
// the write checks, so it is not marked read with the old walk.
func TestForgeHistoryAReadOfARemovedSourceLandsInNoNewOne(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
		t.Fatal(err)
	}
	old := sourceID(t, s, testOrigin)
	if _, err := s.SetForgeHistorySources(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetForgeHistorySources([]ForgeHistoryRef{testOrigin}); err != nil {
		t.Fatal(err)
	}
	err := s.ApplyForgeHistory(ForgeHistoryRead{SourceID: old, Source: testOrigin, Walk: []byte(`{}`), ReadAt: 2, WindowStart: 1})
	if !errors.Is(err, ErrForgeHistoryGone) {
		all, _ := s.ListForgeHistorySources()
		t.Fatalf("a read begun before the source was removed = %v: %+v", err, all)
	}
	if src, ok, err := s.GetForgeHistorySource(sourceID(t, s, testOrigin)); err != nil || !ok || src.HasReach || src.ReadAt != 0 {
		t.Fatalf("the new source = %+v, %v, %v", src, ok, err)
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
		src, ok, err := s.GetForgeHistorySource(sourceID(t, s, testOrigin))
		if err != nil || !ok {
			t.Fatalf("source = %v, %v", ok, err)
		}
		return src
	}
	if src := source(); src.HasReach || src.Covers(5_000_000, testWindow) || !src.Owed {
		t.Fatalf("a source never read: %+v", src)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Walk: []byte(`{"at":1}`), Reach: 3_000_000, HasReach: true, Owed: true, OwnHead: "R_acme", Parent: testParent})
	src := source()
	margin := ForgeHistoryClockMargin.Milliseconds()
	switch {
	case !src.HasReach || src.Reach != 3_000_000 || !src.Owed || string(src.Walk) != `{"at":1}` || src.OwnHead != "R_acme" || src.Parent != testParent:
		t.Fatalf("after a first read: %+v", src)
	case !src.Covers(3_000_000+margin, testWindow) || src.Covers(3_000_000+margin-1, testWindow):
		t.Fatal("a source covers a ticket by its reach and the margin")
	}
	// A failed read that read nothing keeps the walk.
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, FailureKind: "rate_limited", Failure: "GitHub rate limit reached",
		FailureDetail: []byte(`{"reserve":true}`), ReadAt: 6_000_000})
	src = source()
	if src.Reach != 3_000_000 || src.Failure != "GitHub rate limit reached" || string(src.FailureDetail) != `{"reserve":true}` ||
		src.FailedAt != 6_000_000 || src.ReadAt != 6_000_000 || !src.Owed {
		t.Fatalf("after a failure: %+v", src)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Walk: []byte(`{"at":2}`), Reach: 0, HasReach: true, ReadAt: 7_000_000})
	src = source()
	switch {
	case src.Failure != "" || src.FailureDetail != nil || src.FailedAt != 0 || src.Owed || src.Reach != 0:
		t.Fatalf("after a full read: %+v", src)
	case !src.Covers(testWindow, testWindow) || src.Covers(testWindow-1, testWindow):
		t.Fatal("a full read covers every ticket created inside the window, and none before it")
	}
	// A read that proves no reach covers nothing until one does.
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Walk: []byte(`{"at":3}`), Owed: true, ReadAt: 8_000_000})
	if src = source(); src.HasReach || src.Covers(5_000_000, testWindow) {
		t.Fatalf("after a read that has not reached the present: %+v", src)
	}
	applyForge(t, s, ForgeHistoryRead{Source: testOrigin, Walk: []byte(`{"at":4}`), HasReach: true, ReadAt: 9_000_000})
	if src = source(); !src.HasReach || !src.Covers(5_000_000, testWindow) {
		t.Fatalf("after a read that reached it: %+v", src)
	}
}

// Each statement of the writer, Work's read, and the reader's question of
// what the store holds of a page, reaches the stored rows by their keys
// from its list or the ticket's key, so a read costs its own rows however
// many the source holds.
func TestForgeHistoryWriterPlans(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, c := range []struct {
		name, query string
		args        []any
		// probe is a search a plan must make by a pull request's whole key.
		probe string
	}{
		{"pull requests", forgeHistoryPRsSQL, []any{1, "[]", 1}, ""},
		{"clear keys", forgeHistoryClearKeysSQL, []any{1, "[]"}, "forge_history_keys"},
		{"keys", forgeHistoryKeysSQL, []any{1, "[]"}, ""},
		{"orphan acts", forgeHistoryOrphanActSQL, []any{1, "[]"}, "p"},
		{"acts", forgeHistoryActsSQL, []any{1, "[]"}, ""},
		{"prune", forgeHistoryPruneSQL, []any{1, 1}, ""},
		{"stored", forgeHistoryStoredSQL, []any{1, "[]"}, "p"},
		{"work sources", forgeHistoryWorkSourcesSQL, []any{"[]"}, ""},
		{"work rows", forgeHistoryWorkRowsSQL, []any{"WEB-1", "[]", "[]"}, "p"},
	} {
		plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+c.query, c.args...)
		t.Logf("%s:\n%s", c.name, plan)
		found := c.probe == ""
		for _, line := range strings.Split(plan, "\n") {
			line = strings.TrimSpace(line)
			// A walk of a table: a scan of anything but a bound list (a
			// JSON list, or Work's origins o and candidates c), or a search
			// by the source alone.
			if strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "VIRTUAL TABLE") && line != "SCAN o" && line != "SCAN c" ||
				strings.HasSuffix(line, "(source_id=?)") {
				t.Errorf("%s walks the source's rows: %s", c.name, line)
			}
			if strings.HasPrefix(line, "SEARCH "+c.probe+" ") && strings.Contains(line, "source_id=? AND number=?") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not reach %s by a pull request's key", c.name, c.probe)
		}
	}
}
