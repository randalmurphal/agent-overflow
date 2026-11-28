package trackerapp

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

var forgeOrigin = store.ForgeHistoryRef{RepoKey: "github:acme/web", Forge: "github", Host: "github.com", Namespace: "acme", Repo: "web"}

// forgeFixture is an evidence fixture of one done ticket (20001, WEB-1)
// whose connection is verified as User 1 and whose credit reads the
// repository acme/web, read whole by the token of the forge account "me",
// with one pull request that names WEB-1 and acts on it: an approval by
// "me", by "user2" (User 2), by "ghost" (Ghost Dev) and a comment by
// "nameless", whom the forge gives no name. The tracker knows User 2 and
// no Ghost Dev; asked collects the names it is asked for.
func forgeFixture(t *testing.T) (m *mirrorFixture, cs *countingStore, asked func() []string) {
	t.Helper()
	m, cs, _ = evidenceFixture(t, 1)
	if _, _, err := m.st.RecordTrackerConnectionVerification(m.conn.ID, store.TrackerVerification{AccountID: string(user1.ID), Name: user1.Name, VerifiedAt: 1}); err != nil {
		t.Fatal(err)
	}
	// The service reads a verification it did not write at its next hold
	// of the connection, as it does after its own (Verify).
	m.svc.opMu.Lock()
	m.svc.forgetHolds()
	m.svc.opMu.Unlock()
	if _, _, err := m.st.SetForgeHistorySources([]store.ForgeHistoryRef{forgeOrigin}, []store.ForgeHistoryUse{{ConnectionID: m.conn.ID, RepoKey: forgeOrigin.RepoKey}}); err != nil {
		t.Fatal(err)
	}
	m.applyForge(t, store.ForgeHistoryRead{Viewer: "me", Walk: []byte(`{}`), HasReach: true,
		PRs: []store.ForgeHistoryPR{{Number: 7, URL: "https://github.com/acme/web/pull/7", Title: "WEB-1 Change", HeadRef: "change", HeadRepo: "R_acme", State: "merged",
			Author: "user1", CreatedAt: 1_798_000_000_000, UpdatedAt: 1_798_000_000_000, SeenAt: 1_798_000_000_000, Keys: []string{"WEB-1"}}},
		Acts: []store.ForgeHistoryAct{forgeAct("me", "User 1"), forgeAct("user2", "User 2"), forgeAct("ghost", "Ghost Dev"),
			{Number: 7, Kind: store.ForgeActCommented, Login: "nameless", At: 1_798_000_000_000}}})
	var names []string
	m.p.set(func(p *scripted) {
		p.personNamed = func(name string) (tracker.User, bool, error) {
			names = append(names, name)
			if name == user2.Name {
				return user2, true, nil
			}
			return tracker.User{}, false, nil
		}
	})
	return m, cs, func() []string { return slices.Clone(names) }
}

func forgeAct(login, name string) store.ForgeHistoryAct {
	return store.ForgeHistoryAct{Number: 7, Kind: store.ForgeActApproved, Login: login, Name: name, At: 1_798_000_000_000}
}

// applyForge applies read to the fixture's repository and returns what
// it changed.
func (m *mirrorFixture) applyForge(t *testing.T, read store.ForgeHistoryRead) store.ForgeHistoryChange {
	t.Helper()
	sources := must[[]store.ForgeHistorySource](t)(m.st.ListForgeHistorySources())
	read.SourceID, read.Source, read.ReadAt, read.WindowStart = sources[0].ID, forgeOrigin, 1_799_000_000_000, 1
	return must[store.ForgeHistoryChange](t)(m.st.ApplyForgeHistory(read))
}

// storeStrings answers query, one text column, on the fixture's store
// file.
func (f *fixture) storeStrings(t *testing.T, query string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.file+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		out = append(out, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return out
}

// forgeMatches are the matches the fixture's mirror holds, "login=person".
func (m *mirrorFixture) forgeMatches(t *testing.T) []string {
	t.Helper()
	return m.storeStrings(t, fmt.Sprintf(`SELECT login || '=' || person FROM tracker_cache_forge_people WHERE root_id = %d ORDER BY login`, m.root()))
}

func (m *mirrorFixture) peopleTick(t *testing.T) Outcome {
	t.Helper()
	return must[Outcome](t)(m.svc.SyncLinked(t.Context(), m.conn.ID, LinkedDue{People: true}, Background))
}

// The people unit matches the forge accounts that acted on the pull
// requests credit reads: the account whose token read the repository is
// the connection's own person with no search, an account the forge names
// is the one person of that name, and one it does not name, or whose name
// no person has, is nobody. The matches have an open board's dashboards
// count again, and nothing is asked again until one is due.
func TestThePeopleUnitMatchesForgeAccountsToTrackerPeople(t *testing.T) {
	m, cs, asked := forgeFixture(t)
	m.countCredit(t, m.conn.ID, true)
	m.subscribeShown(t, m.project())
	n := m.boardEventCount()
	if out := m.peopleTick(t); out.Failure != nil || len(out.Ran) != 1 || out.Ran[0].Unit != UnitPeople || out.Ran[0].More {
		t.Fatalf("the people tick %+v", out)
	}
	if got, want := asked(), []string{"Ghost Dev", "User 2"}; !slices.Equal(got, want) {
		t.Fatalf("the tracker was asked for %q; want %q", got, want)
	}
	if got, want := m.forgeMatches(t), []string{"ghost=", "me=acc-user1", "nameless=", "user2=acc-user2"}; !slices.Equal(got, want) {
		t.Fatalf("matches %q; want %q", got, want)
	}
	if !m.recountSince(n) {
		t.Fatal("the matches did not have the open board's dashboards count again")
	}
	// The person a search found is named in the mirror's catalog.
	catalog := must[store.MirrorCatalog](t)(m.st.GetMirrorCatalog(m.root(), []string{string(user2.ID)}, nil))
	if len(catalog.Users) != 1 || catalog.Users[0].Name != user2.Name {
		t.Fatalf("catalog %+v; want User 2", catalog.Users)
	}
	cs.take()
	n = m.boardEventCount()
	if out := m.peopleTick(t); out.Failure != nil {
		t.Fatalf("the second tick %+v", out)
	}
	if calls := cs.take(); len(asked()) != 2 || calls["SetMirrorForgePeople"] != 0 || calls["ListMirrorForgePeopleDue"] != 1 || m.recountSince(n) {
		t.Fatalf("a tick with no match due asked %q, store calls %v, recount %t", asked(), calls, m.recountSince(n))
	}
}

// A match that names the same person as before has no board count again.
func TestAMatchThatChangesNoPersonTellsNoBoard(t *testing.T) {
	m, _, _ := forgeFixture(t)
	m.countCredit(t, m.conn.ID, true)
	m.subscribeShown(t, m.project())
	m.peopleTick(t)
	// The forge names User 2's account otherwise; the tracker's person of
	// that name is still User 2.
	m.p.set(func(p *scripted) {
		p.personNamed = func(string) (tracker.User, bool, error) { return user2, true, nil }
	})
	m.applyForge(t, store.ForgeHistoryRead{Acts: []store.ForgeHistoryAct{forgeAct("user2", "User Two")}})
	n := m.boardEventCount()
	if out := m.peopleTick(t); out.Failure != nil {
		t.Fatalf("the tick %+v", out)
	}
	if m.recountSince(n) {
		t.Fatal("a match to the same person had the dashboards count again")
	}
	if got := m.storeStrings(t, `SELECT name FROM tracker_cache_forge_people WHERE login = 'user2'`); !slices.Equal(got, []string{"User Two"}) {
		t.Fatalf("the match is for the name %q; want the forge's new name", got)
	}
}

// A search that fails stores the matches made before it and fails the
// tick; the next tick asks for the rest.
func TestAFailedSearchKeepsTheMatchesBeforeIt(t *testing.T) {
	m, _, asked := forgeFixture(t)
	m.countCredit(t, m.conn.ID, true)
	fail := true
	m.p.set(func(p *scripted) {
		found := p.personNamed
		p.personNamed = func(name string) (tracker.User, bool, error) {
			if name == user2.Name && fail {
				return tracker.User{}, false, &trackerapi.TransientError{Err: errors.New("reset")}
			}
			return found(name)
		}
	})
	out := m.peopleTick(t)
	if out.Failure == nil || len(out.Ran) != 1 || out.Ran[0].More {
		t.Fatalf("the tick %+v; want it failed, with no more asked of this round", out)
	}
	if got, want := m.forgeMatches(t), []string{"ghost=", "me=acc-user1", "nameless="}; !slices.Equal(got, want) {
		t.Fatalf("matches after the failure %q; want %q", got, want)
	}
	fail = false
	if out := m.peopleTick(t); out.Failure != nil {
		t.Fatalf("the next tick %+v", out)
	}
	if got, want := m.forgeMatches(t), []string{"ghost=", "me=acc-user1", "nameless=", "user2=acc-user2"}; !slices.Equal(got, want) {
		t.Fatalf("matches %q; want %q", got, want)
	}
	if got := asked(); !slices.Equal(got, []string{"Ghost Dev", "User 2"}) {
		t.Fatalf("asked %q; want Ghost Dev once and User 2 once it answered", got)
	}
}

// The unit lists and asks nothing while no project counts credit, while
// no rule counts a comment or an approval, and of a tracker that reads no
// evidence.
func TestThePeopleUnitReadsNothingWhenNoCreditReadsTheForge(t *testing.T) {
	for name, set := range map[string]func(t *testing.T, m *mirrorFixture){
		"credit is off": func(t *testing.T, m *mirrorFixture) { m.countCredit(t, m.conn.ID, false) },
		"no rule counts a forge act": func(t *testing.T, m *mirrorFixture) {
			m.writeSetupFile(t, m.project(), `{"credit": {"version": 1, "on": true, "rules": [{"from": 0, "review_share": 10, "review_need": 1,
				"review_actions": ["logged_time"], "hours_per_point": 8}], "support": []}}`)
			m.svc.setupMu.Lock()
			clear(m.svc.setups)
			m.svc.setupMu.Unlock()
		},
		"the tracker reads no evidence": func(t *testing.T, m *mirrorFixture) {
			m.countCredit(t, m.conn.ID, true)
			m.p.set(func(p *scripted) { p.caps.Evidence = false })
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, cs, asked := forgeFixture(t)
			set(t, m)
			cs.take()
			if out := m.peopleTick(t); out.Failure != nil {
				t.Fatalf("the tick %+v", out)
			}
			if calls := cs.take(); calls["ListMirrorForgePeopleDue"]+calls["SetMirrorForgePeople"] != 0 || len(asked()) != 0 {
				t.Fatalf("store calls %v, asked %q", calls, asked())
			}
		})
	}
}

// totals is the totals revision of the fixture's board, -1 once the
// connection has no mirror.
func (m *mirrorFixture) totals(t *testing.T) int64 {
	t.Helper()
	if m.root() == 0 {
		return -1
	}
	return int64(m.storeCount(t, fmt.Sprintf(`SELECT totals_revision FROM tracker_cache_boards WHERE root_id = %d AND board_id = '%s'`, m.root(), testBoard)))
}

// A change of the forge history tells the connection's boards whose
// credit reads it: the pull requests of a ticket a board counts name its
// row to the open board, with its totals advanced; keys of no ticket tell
// nobody; and a change of what a repository covers has every such board's
// dashboards count again.
func TestForgeCreditChangedTellsTheBoardsThatCountIt(t *testing.T) {
	m, cs, _ := forgeFixture(t)
	m.countCredit(t, m.conn.ID, true)
	m.subscribeShown(t, m.project())

	n, totals := m.boardEventCount(), m.totals(t)
	if err := m.svc.ForgeCreditChanged(m.conn.ID, []string{"WEB-1", "WEB-99"}, false); err != nil {
		t.Fatal(err)
	}
	if got := m.rowsToldSince(n); !slices.Equal(got, ids("20001")) || m.recountSince(n) || m.totals(t) != totals+1 {
		t.Fatalf("told rows %v, recount %t, totals %d -> %d; want ticket 20001's row and the totals advanced once", got, m.recountSince(n), totals, m.totals(t))
	}

	n, totals = m.boardEventCount(), m.totals(t)
	cs.take()
	for _, keys := range [][]string{{"WEB-99"}, nil} {
		if err := m.svc.ForgeCreditChanged(m.conn.ID, keys, false); err != nil {
			t.Fatal(err)
		}
	}
	if m.boardEventCount() != n || m.totals(t) != totals {
		t.Fatalf("keys of no ticket told the board: events %d -> %d, totals %d -> %d", n, m.boardEventCount(), totals, m.totals(t))
	}
	if calls := cs.take(); calls["AdvanceMirrorForgeCredit"] != 2 {
		t.Fatalf("store calls %v; want each change asked of the store", calls)
	}

	if err := m.svc.ForgeCreditChanged(m.conn.ID, nil, true); err != nil {
		t.Fatal(err)
	}
	if !m.recountSince(n) || len(m.rowsToldSince(n)) != 0 || m.totals(t) != totals+1 {
		t.Fatalf("a change of every ticket's credit: recount %t, rows %v, totals %d -> %d", m.recountSince(n), m.rowsToldSince(n), totals, m.totals(t))
	}
}

// A connection whose credit does not read the forge, one that is off and
// one that is gone are told nothing, and their totals do not move.
func TestForgeCreditChangedTellsNoBoardThatDoesNotCountIt(t *testing.T) {
	for name, set := range map[string]func(t *testing.T, m *mirrorFixture){
		"credit is off": func(t *testing.T, m *mirrorFixture) { m.countCredit(t, m.conn.ID, false) },
		"the connection is off": func(t *testing.T, m *mirrorFixture) {
			m.countCredit(t, m.conn.ID, true)
			if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, cs, _ := forgeFixture(t)
			m.countCredit(t, m.conn.ID, true)
			m.subscribeShown(t, m.project())
			set(t, m)
			n, totals := m.boardEventCount(), m.totals(t)
			cs.take()
			for _, all := range []bool{false, true} {
				if err := m.svc.ForgeCreditChanged(m.conn.ID, []string{"WEB-1"}, all); err != nil {
					t.Fatal(err)
				}
			}
			if m.recountSince(n) || len(m.rowsToldSince(n)) != 0 || m.totals(t) != totals {
				t.Fatalf("recount %t, rows %v, totals %d -> %d; want nothing told", m.recountSince(n), m.rowsToldSince(n), totals, m.totals(t))
			}
			if calls := cs.take(); calls["AdvanceMirrorForgeCredit"] != 0 {
				t.Fatalf("store calls %v; want no advance asked", calls)
			}
		})
	}
	m, cs, _ := forgeFixture(t)
	if err := m.svc.ForgeCreditChanged("no-such-connection", []string{"WEB-1"}, true); err != nil {
		t.Fatalf("a connection that is gone: %v", err)
	}
	// A tracker credit does not count, with a mirror and a project that
	// counts credit on its site.
	linear := m.addLinear()
	m.listBoards(linear.ID, testBoard)
	m.countCredit(t, linear.ID, true)
	if _, _, err := m.st.GetOrCreateMirrorRoot(linear.ID); err != nil {
		t.Fatal(err)
	}
	cs.take()
	if err := m.svc.ForgeCreditChanged(linear.ID, nil, true); err != nil {
		t.Fatal(err)
	}
	if calls := cs.take(); calls["AdvanceMirrorForgeCredit"] != 0 {
		t.Fatalf("store calls %v; want no advance for a tracker credit does not count", calls)
	}
}

// The credit sentence names the repositories whose pull requests could
// not be read, the first by its path with its failure and the rest
// counted, after the evidence read's own failure; a project whose rules
// count no forge act says nothing of them.
func TestTheCreditSentenceNamesTheRepositoriesThatCouldNotBeRead(t *testing.T) {
	m, _, _ := forgeFixture(t)
	m.countCredit(t, m.conn.ID, true)
	if got := m.evidenceFailureOf(t); got != "" {
		t.Fatalf("with every repository read, the sentence's failure is %q", got)
	}
	m.applyForge(t, store.ForgeHistoryRead{FailureKind: "auth", Failure: "GitHub refused the token."})
	const one = "Pull requests of acme/web could not be read: GitHub refused the token."
	if got := m.evidenceFailureOf(t); got != one {
		t.Fatalf("the sentence's failure is %q; want %q", got, one)
	}
	// Two more repositories fail, each later by its path.
	refs := []store.ForgeHistoryRef{forgeOrigin}
	uses := []store.ForgeHistoryUse{{ConnectionID: m.conn.ID, RepoKey: forgeOrigin.RepoKey}}
	for _, ns := range []string{"beta", "zeta"} {
		ref := store.ForgeHistoryRef{RepoKey: "github:" + ns + "/web", Forge: "github", Host: "github.com", Namespace: ns, Repo: "web"}
		refs, uses = append(refs, ref), append(uses, store.ForgeHistoryUse{ConnectionID: m.conn.ID, RepoKey: ref.RepoKey})
	}
	if _, _, err := m.st.SetForgeHistorySources(refs, uses); err != nil {
		t.Fatal(err)
	}
	for i, src := range must[[]store.ForgeHistorySource](t)(m.st.ListForgeHistorySources()) {
		if src.RepoKey == forgeOrigin.RepoKey {
			continue
		}
		if _, err := m.st.ApplyForgeHistory(store.ForgeHistoryRead{SourceID: src.ID, Source: refs[slices.IndexFunc(refs, func(r store.ForgeHistoryRef) bool { return r.RepoKey == src.RepoKey })],
			ReadAt: 1_799_000_000_000, WindowStart: 1, FailureKind: "network", Failure: fmt.Sprintf("GitHub is unreachable (%d).", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := m.evidenceFailureOf(t), "Pull requests of acme/web and 2 more repositories could not be read: GitHub refused the token."; got != want {
		t.Fatalf("the sentence's failure is %q; want %q", got, want)
	}
	// The evidence read's own failure comes first.
	m.p.set(func(p *scripted) {
		p.evidence = func(tracker.EvidenceQuery) (tracker.Evidence, error) {
			return tracker.Evidence{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
	})
	out := must[Outcome](t)(m.svc.SyncLinked(t.Context(), m.conn.ID, LinkedDue{Evidence: true}, Background))
	if out.Failure == nil {
		t.Fatal("the failing evidence read did not fail its tick")
	}
	got := m.evidenceFailureOf(t)
	if at := strings.Index(got, " Pull requests of acme/web and 2 more repositories could not be read: "); at <= 0 || !strings.Contains(got[:at], out.Failure.Message) {
		t.Fatalf("the sentence's failure is %q; want the evidence read's failure %q, then the repositories", got, out.Failure.Message)
	}
	// A project whose rules count no forge act says the evidence read's alone.
	m.writeSetupFile(t, m.project(), `{"credit": {"version": 1, "on": true, "rules": [{"from": 0, "review_share": 10, "review_need": 1,
		"review_actions": ["logged_time"], "hours_per_point": 8}], "support": []}}`)
	m.svc.setupMu.Lock()
	clear(m.svc.setups)
	m.svc.setupMu.Unlock()
	if got := m.evidenceFailureOf(t); strings.Contains(got, "Pull requests") || !strings.Contains(got, out.Failure.Message) {
		t.Fatalf("with no rule counting a forge act, the sentence's failure is %q", got)
	}
}
