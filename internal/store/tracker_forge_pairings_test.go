package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// v180 adds the pairings: one of an account of a connection, with a
// person and the name they were picked by or with neither, removed with
// its connection.
func TestTheForgePairingsMigrationAddsThePairings(t *testing.T) {
	db := migrateThrough(t, forgePairingsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES ('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	migrateFromThrough(t, db, forgePairingsMigrationVersion-1, forgePairingsMigrationVersion)
	for name, stmt := range map[string]string{
		"a pairing of no connection":   `INSERT INTO tracker_forge_pairings(connection_id, host, login, person) VALUES ('none', 'github.com', 'user2', 'acc-user2')`,
		"a pairing of no host":         `INSERT INTO tracker_forge_pairings(connection_id, host, login, person) VALUES ('j1', '', 'user2', 'acc-user2')`,
		"a pairing of no login":        `INSERT INTO tracker_forge_pairings(connection_id, host, login, person) VALUES ('j1', 'github.com', '', 'acc-user2')`,
		"a name for no person":         `INSERT INTO tracker_forge_pairings(connection_id, host, login, person_name) VALUES ('j1', 'github.com', 'user2', 'User 2')`,
		"a second pairing of one":      `INSERT INTO tracker_forge_pairings(connection_id, host, login) VALUES ('j1', 'github.com', 'bot'), ('j1', 'github.com', 'bot')`,
		"a pairing with a null person": `INSERT INTO tracker_forge_pairings(connection_id, host, login, person) VALUES ('j1', 'github.com', 'user3', NULL)`,
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	mustExec(t, db, `INSERT INTO tracker_forge_pairings(connection_id, host, login, person, person_name) VALUES ('j1', 'github.com', 'user2', 'acc-user2', 'User 2')`)
	mustExec(t, db, `INSERT INTO tracker_forge_pairings(connection_id, host, login) VALUES ('j1', 'github.com', 'bot')`)
	assertRows(t, db, `SELECT group_concat(login || '=' || person || '|' || person_name, ' ') FROM (SELECT login, person, person_name FROM tracker_forge_pairings ORDER BY login)`,
		"bot=| user2=acc-user2|User 2")
	mustExec(t, db, `DELETE FROM tracker_connections WHERE id = 'j1'`)
	assertRows(t, db, `SELECT count(*) FROM tracker_forge_pairings`, "0")
}

func pair(t *testing.T, s *Store, conn, login, person, name string) bool {
	t.Helper()
	changed, err := s.SetTrackerForgePairing(TrackerForgePairing{ConnectionID: conn, Host: "github.com", Login: login, Person: person, PersonName: name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

// A pairing is stored for an account of a stored connection, replaces the
// account's earlier one, and reports a change only of who the account is.
func TestSetTrackerForgePairingReportsWhoChanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	conn := mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user1@example.com", 1)).ID
	for name, p := range map[string]TrackerForgePairing{
		"no connection":        {Host: "github.com", Login: "user2", Person: "acc-user2"},
		"no host":              {ConnectionID: conn, Login: "user2", Person: "acc-user2"},
		"no login":             {ConnectionID: conn, Host: "github.com", Person: "acc-user2"},
		"a name for no person": {ConnectionID: conn, Host: "github.com", Login: "user2", PersonName: "User 2"},
	} {
		if _, err := s.SetTrackerForgePairing(p, nil); !errors.Is(err, ErrInvalidForgePairing) {
			t.Errorf("%s: %v; want ErrInvalidForgePairing", name, err)
		}
	}
	if _, err := s.SetTrackerForgePairing(TrackerForgePairing{ConnectionID: "gone", Host: "github.com", Login: "user2", Person: "acc-user2"}, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a pairing of a connection that is not stored: %v; want sql.ErrNoRows", err)
	}
	if !pair(t, s, conn, "user2", "acc-user2", "User 2") {
		t.Fatal("the first pairing reported no change")
	}
	if pair(t, s, conn, "user2", "acc-user2", "User Two") {
		t.Fatal("a new name for the same person reported a change")
	}
	if !pair(t, s, conn, "user2", "acc-user3", "User 3") || !pair(t, s, conn, "user2", "", "") || pair(t, s, conn, "user2", "", "") {
		t.Fatal("want a change for another person and for no person, and none for the same again")
	}
	if !pair(t, s, conn, "bot", "", "") {
		t.Fatal("a first pairing as no person reported no change")
	}
	got, err := s.ListTrackerForgePairings(conn)
	want := []TrackerForgePairing{{ConnectionID: conn, Host: "github.com", Login: "bot"}, {ConnectionID: conn, Host: "github.com", Login: "user2"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("pairings %+v, %v; want %+v", got, err, want)
	}
	if deleted, err := s.DeleteTrackerForgePairing(conn, "github.com", "bot", nil); err != nil || !deleted {
		t.Fatalf("delete: %t, %v", deleted, err)
	}
	if deleted, err := s.DeleteTrackerForgePairing(conn, "github.com", "bot", nil); err != nil || deleted {
		t.Fatalf("delete again: %t, %v; want none", deleted, err)
	}
	if got, err := s.ListTrackerForgePairings(conn); err != nil || len(got) != 1 || got[0].Login != "user2" {
		t.Fatalf("pairings after the delete %+v, %v", got, err)
	}
}

// A change of who an account is advances the totals of the boards it
// names on the connection's mirror, in the pairing's own transaction: a
// change no board could be told of is not stored, and a pairing that
// changes nobody advances none.
func TestAForgePairingAdvancesTheTotalsOfTheBoardsItNames(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	conn := mirrorConnection(t, s, root)
	totals := func() [2]int64 { return [2]int64{boardTotals(t, s, root, "10"), boardTotals(t, s, root, "11")} }
	was := totals()
	set := func(person string, boards ...string) bool {
		t.Helper()
		changed, err := s.SetTrackerForgePairing(TrackerForgePairing{ConnectionID: conn, Host: "github.com", Login: "user2", Person: person}, boards)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if !set("acc-user2", "10") || totals() != [2]int64{was[0] + 1, was[1]} {
		t.Fatalf("a first pairing: totals %v from %v; want board 10 advanced once", totals(), was)
	}
	if set("acc-user2", "10") || totals() != [2]int64{was[0] + 1, was[1]} {
		t.Fatalf("the same pairing again: totals %v; want none advanced", totals())
	}
	if !set("acc-user3", "10", "11", "no-such-board") || totals() != [2]int64{was[0] + 2, was[1] + 1} {
		t.Fatalf("another person: totals %v; want both boards advanced", totals())
	}
	if deleted, err := s.DeleteTrackerForgePairing(conn, "github.com", "user2", []string{"11"}); err != nil || !deleted || totals() != [2]int64{was[0] + 2, was[1] + 2} {
		t.Fatalf("the pairing removed: %t, %v, totals %v; want board 11 advanced", deleted, err, totals())
	}
	if deleted, err := s.DeleteTrackerForgePairing(conn, "github.com", "user2", []string{"11"}); err != nil || deleted || totals() != [2]int64{was[0] + 2, was[1] + 2} {
		t.Fatalf("no pairing removed: %t, %v, totals %v; want none advanced", deleted, err, totals())
	}

	// The advance fails: the pairing is not stored either.
	mustExec(t, s.db, `CREATE TRIGGER fail_totals BEFORE UPDATE OF totals_revision ON tracker_cache_boards BEGIN SELECT raise(ABORT, 'no totals'); END`)
	if _, err := s.SetTrackerForgePairing(TrackerForgePairing{ConnectionID: conn, Host: "github.com", Login: "user2", Person: "acc-user2"}, []string{"10"}); err == nil {
		t.Fatal("a pairing whose boards could not be advanced was stored")
	}
	if got, err := s.ListTrackerForgePairings(conn); err != nil || len(got) != 0 {
		t.Fatalf("pairings after the failed write %+v, %v; want none", got, err)
	}
	mustExec(t, s.db, `DROP TRIGGER fail_totals`)
	set("acc-user2", "10")
	mustExec(t, s.db, `CREATE TRIGGER fail_totals BEFORE UPDATE OF totals_revision ON tracker_cache_boards BEGIN SELECT raise(ABORT, 'no totals'); END`)
	if _, err := s.DeleteTrackerForgePairing(conn, "github.com", "user2", []string{"10"}); err == nil {
		t.Fatal("a pairing was removed with its boards not advanced")
	}
	if got, err := s.ListTrackerForgePairings(conn); err != nil || len(got) != 1 {
		t.Fatalf("pairings after the failed removal %+v, %v; want the pairing kept", got, err)
	}
	mustExec(t, s.db, `DROP TRIGGER fail_totals`)

	// A connection with no mirror stores the pairing and advances nothing.
	bare := mustCreateTrackerConnection(t, s, jiraConnection("https://example2.atlassian.net", "user1@example.com", 2)).ID
	if changed, err := s.SetTrackerForgePairing(TrackerForgePairing{ConnectionID: bare, Host: "github.com", Login: "user2", Person: "acc-user2"}, []string{"10"}); err != nil || !changed {
		t.Fatalf("a pairing of a connection with no mirror: %t, %v", changed, err)
	}
	if got := totals(); got != [2]int64{was[0] + 3, was[1] + 2} {
		t.Fatalf("totals %v after a pairing of another connection; want them as they were", got)
	}
}

// Credit counts a forge act for the person its account is paired with
// before the one its name matches: a pairing counts an account no name
// matches, moves one a name matched to another person, and names a person
// the mirror never read as picked; an account paired as no person has no
// row at all, and an account with neither is still listed as unmatched.
// The match unit is owed none of the paired accounts. Another connection's
// word on the same accounts is not this one's.
func TestCreditCountsAForgeActForThePersonItsAccountIsPairedWith(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	conn := mirrorConnection(t, s, root)
	regroup := creditRegroup(t, creditRules(true))
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin}, testOrigin)
	applyForge(t, s, forgeWhole(testOrigin, []ForgeHistoryPR{keyPR(7, "WEB-10001")},
		approved(7, "user2", "User 2", 700), approved(7, "ghost", "Ghost Dev", 710), commented(7, "ci", "CI", 720),
		approved(7, "stranger", "Stranger", 730), commented(7, "nine", "Nine", 740), approved(7, "nine", "Nine", 741)))
	matchForge(t, s, root, "user2", "acc-user2", "ghost", "", "ci", "", "stranger", "", "nine", "")
	const (
		user2    = "20 800 10001 review acc-user2 0.5 0.1 5.0 10 2 commented, approved 1 - - -"
		user3    = "20 800 10001 review acc-user3 0.5 0.1 5.0 10 2 commented, logged time 1 - - -"
		stranger = "20 800 10001 review - 0.0 0.0 5.0 10 2 approved - - - " + unmatchedNote + " [Stranger (stranger on github.com)]"
	)
	other := mustCreateTrackerConnection(t, s, jiraConnection("https://example2.atlassian.net", "user1@example.com", 2)).ID
	pair(t, s, other, "stranger", "acc-user4", "User 4")
	pair(t, s, other, "user2", "", "")
	rows, _ := creditOf(t, s, root, regroup)
	if got := reviewsOf(rows, "10001"); len(got) != 6 || got[3] != stranger || got[4] != user2 || got[5] != user3 {
		t.Fatalf("before any pairing of the connection\n%s", strings.Join(got, "\n"))
	}

	pair(t, s, conn, "ghost", "acc-user4", "User 4") // no name matched it
	pair(t, s, conn, "user2", "acc-user3", "Third")  // a name matched it to another
	pair(t, s, conn, "ci", "", "")                   // no person
	pair(t, s, conn, "nine", "acc-user9", "User Nine")
	rows, people := creditOf(t, s, root, regroup)
	want := []string{
		stranger,
		"20 800 10001 review acc-user2 0.0 0.0 5.0 10 2 commented 0 - - -",
		"20 800 10001 review acc-user3 0.5 0.1 5.0 10 2 commented, approved, logged time 1 - - -",
		"20 800 10001 review acc-user4 0.0 0.0 5.0 10 2 approved 0 - - -",
		"20 800 10001 review acc-user9 0.5 0.1 5.0 10 2 commented, approved 1 - - -",
	}
	if got := reviewsOf(rows, "10001"); !reflect.DeepEqual(got, want) {
		t.Fatalf("with the pairings\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Contains(people, "acc-user9") {
		t.Fatalf("people %v; want the paired person the mirror never read", people)
	}
	export := exportConn(t)
	if _, ok, err := s.ExportMirrorBoard(context.Background(), export, root, "10", regroup, []string{"credit"}, 0, time.Now().UnixMilli(), MirrorLoggedDays{}, CardShapeForTest); err != nil || !ok {
		t.Fatalf("export: %v, %v", ok, err)
	}
	if got := exportRows(t, export, `SELECT id || '=' || name FROM main.people WHERE id IN ('acc-user9', 'acc-user3') ORDER BY id`); !slices.Equal(got, []string{"acc-user3=User 3", "acc-user9=User Nine"}) {
		t.Fatalf("people named %v; want the mirror's name for one it read and the picked name for one it never read", got)
	}
	if got, _ := dueLogins(t, s, root, 10_000_000_000_000, ""); !slices.Equal(got, []string{"stranger=Stranger"}) {
		t.Fatalf("due = %v; want only the account no person paired", got)
	}

	// A pairing removed: the account is whoever its name matches again.
	if deleted, err := s.DeleteTrackerForgePairing(conn, "github.com", "user2", nil); err != nil || !deleted {
		t.Fatalf("delete: %t, %v", deleted, err)
	}
	rows, _ = creditOf(t, s, root, regroup)
	if got := reviewsOf(rows, "10001"); !slices.Contains(got, user2) || !slices.Contains(got, user3) {
		t.Fatalf("after the pairing is removed\n%s", strings.Join(got, "\n"))
	}
}

// The accounts a connection's credit reads are listed with who each is,
// the ones that are nobody's first, then by the forge's name: paired
// (with a person, or as no person), matched by name with the person's
// name, the token's own, not matched, and not checked yet. A pairing of
// an account with no act now is listed too, and a connection with no
// mirror lists its pairings alone.
func TestListMirrorForgeAccountsSaysWhoEachAccountIs(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	conn := mirrorConnection(t, s, root)
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin, testOther}, testOrigin)
	read := forgeWhole(testOrigin, []ForgeHistoryPR{keyPR(7, "WEB-10001"), keyPR(8, "WEB-424242")},
		approved(7, "user2", "User 2", 700), commented(7, "ghost", "Ghost Dev", 710), commented(7, "ci", "", 720), approved(7, "me", "User 1", 730),
		approved(7, "new", "Newcomer", 740), approved(7, "zed", "Zed", 750), approved(8, "elsewhere", "Elsewhere", 700))
	read.Viewer = "me"
	applyForge(t, s, read)
	applyForge(t, s, ForgeHistoryRead{Source: testOther, PRs: []ForgeHistoryPR{keyPR(3, "WEB-10001")}, Acts: []ForgeHistoryAct{approved(3, "outsider", "Outsider", 700)}, WindowStart: 1})
	if _, err := s.SetMirrorForgePeople(root, nextMirrorRead(s, root), []MirrorForgeMatch{
		{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "user2", Name: "User 2"}, Person: "acc-user2"},
		{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "me", Name: "User 1"}, Person: "acc-user1"},
		{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "ghost", Name: "Ghost Dev"}},
		{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "ci"}},
		{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "zed", Name: "Zed"}},
	}, MirrorCatalog{}, 1, nil); err != nil {
		t.Fatal(err)
	}
	pair(t, s, conn, "ci", "", "")
	pair(t, s, conn, "zed", "acc-user9", "User Nine")
	pair(t, s, conn, "left", "acc-user3", "User 3")
	list := func(conn string) ([]string, bool) {
		t.Helper()
		accounts, more, err := s.ListMirrorForgeAccounts(conn)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range accounts {
			out = append(out, fmt.Sprintf("%s|%s|own=%t paired=%t checked=%t|%s|%s", a.Login, a.Name, a.Own, a.Paired, a.Checked, a.Person, a.PersonName))
		}
		return out, more
	}
	got, more := list(conn)
	want := []string{
		"ghost|Ghost Dev|own=false paired=false checked=true||",
		"new|Newcomer|own=false paired=false checked=false||",
		"ci||own=false paired=true checked=true||",
		"left||own=false paired=true checked=false|acc-user3|User 3",
		"me|User 1|own=true paired=false checked=true|acc-user1|User 1",
		"user2|User 2|own=false paired=false checked=true|acc-user2|User 2",
		"zed|Zed|own=false paired=true checked=true|acc-user9|User Nine",
	}
	if !reflect.DeepEqual(got, want) || more {
		t.Fatalf("accounts, more %t\n%s\nwant\n%s", more, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A connection with no mirror lists its pairings.
	bare := mustCreateTrackerConnection(t, s, jiraConnection("https://example2.atlassian.net", "user1@example.com", 2)).ID
	pair(t, s, bare, "user2", "acc-user2", "User 2")
	if got, more := list(bare); !reflect.DeepEqual(got, []string{"user2||own=false paired=true checked=false|acc-user2|User 2"}) || more {
		t.Fatalf("a connection with no mirror lists %v, more %t", got, more)
	}

	// One listing is a page of the accounts that acted, and every pairing.
	var many []ForgeHistoryAct
	for i := range MirrorForgeAccountsPage {
		many = append(many, commented(7, fmt.Sprintf("dev%03d", i), "", 700))
	}
	applyForge(t, s, forgeWhole(testOrigin, nil, many...))
	accounts, more, err := s.ListMirrorForgeAccounts(conn)
	if err != nil || !more || len(accounts) != MirrorForgeAccountsPage+1 || !slices.ContainsFunc(accounts, func(a MirrorForgeAccount) bool { return a.Login == "left" }) {
		t.Fatalf("%d accounts, more %t, %v; want a page of the accounts that acted and the pairing with no act", len(accounts), more, err)
	}
}
