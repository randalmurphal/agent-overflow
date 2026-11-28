package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// mirrorConnection is the connection root mirrors.
func mirrorConnection(t *testing.T, s *Store, root int64) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT connection_id FROM tracker_cache_roots WHERE id = ?`, root).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// forgeCreditSources makes wanted the forge sources and used, of them,
// the repositories root's connection's credit reads.
func forgeCreditSources(t *testing.T, s *Store, root int64, wanted []ForgeHistoryRef, used ...ForgeHistoryRef) {
	t.Helper()
	conn := mirrorConnection(t, s, root)
	uses := make([]ForgeHistoryUse, 0, len(used))
	for _, u := range used {
		uses = append(uses, ForgeHistoryUse{ConnectionID: conn, RepoKey: u.RepoKey})
	}
	if _, _, err := s.SetForgeHistorySources(wanted, uses); err != nil {
		t.Fatal(err)
	}
}

// forgeWhole is a read of ref that read every update of the window it
// keeps (reach 0) and failed nothing, with prs and acts.
func forgeWhole(ref ForgeHistoryRef, prs []ForgeHistoryPR, acts ...ForgeHistoryAct) ForgeHistoryRead {
	return ForgeHistoryRead{Source: ref, PRs: prs, Acts: acts, Walk: []byte(`{}`), HasReach: true, WindowStart: 1}
}

// keyPR is pull request n of a ticket key, updated at 900.
func keyPR(n int64, key string) ForgeHistoryPR {
	return testPR(n, key+" Change", "change", 900, key)
}

func approved(n int64, login, name string, at int64) ForgeHistoryAct {
	return ForgeHistoryAct{Number: n, Kind: ForgeActApproved, Login: login, Name: name, At: at}
}

func commented(n int64, login, name string, at int64) ForgeHistoryAct {
	return ForgeHistoryAct{Number: n, Kind: ForgeActCommented, Login: login, Name: name, At: at}
}

// matchForge records who each forge account of github.com is in root's
// tracker: login, then the person ("" for nobody), in pairs.
func matchForge(t *testing.T, s *Store, root int64, pairs ...string) {
	t.Helper()
	var matches []MirrorForgeMatch
	for i := 0; i < len(pairs); i += 2 {
		matches = append(matches, MirrorForgeMatch{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: pairs[i]}, Person: pairs[i+1]})
	}
	if _, err := s.SetMirrorForgePeople(root, nextMirrorRead(s, root), matches, MirrorCatalog{}, 1, nil); err != nil {
		t.Fatal(err)
	}
}

// reviewsOf are ticket's review rows of rows.
func reviewsOf(rows []string, ticket string) []string {
	var out []string
	for _, r := range creditRowsOf(rows, ticket) {
		if strings.Contains(r, " review ") {
			out = append(out, r)
		}
	}
	return out
}

// sourceState sets what credit reads of ref's source.
func sourceState(t *testing.T, s *Store, ref ForgeHistoryRef, reach any, actsOwed int, failure string, goodAt int64) {
	t.Helper()
	kind, failedAt := "", 0
	if failure != "" {
		kind, failedAt = "auth", 1
	}
	mustExec(t, s.db, `UPDATE forge_history_sources SET reach = ?, acts_owed = ?, failure_kind = ?, failure = ?, failed_at = ?, good_at = ? WHERE repo_key = ?`,
		reach, actsOwed, kind, failure, failedAt, goodAt, ref.RepoKey)
}

const unmatchedNote = "no person in the tracker is matched to this forge account"

// An approval or a comment on a pull request that names a ticket's key
// counts for the tracker person its forge account is matched to, with
// what the tracker holds of the person: User 2 commented in the tracker
// and approved on the forge, which is two kinds. The finisher's own
// approval counts nothing, an act after the sprint's end is not the
// sprint's, and an account matched to nobody is a row of its own, by the
// forge's name for it, that counts nothing and is not a ticket left out
// of the pace.
func TestCreditCountsForgeActsForThePeopleTheirAccountsAre(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	regroup := creditRegroup(t, creditRules(true))
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin}, testOrigin)
	applyForge(t, s, forgeWhole(testOrigin, nil))
	before := paceOf(t, s, root, "10", regroup)
	applyForge(t, s, forgeWhole(testOrigin, []ForgeHistoryPR{keyPR(7, "WEB-10001")},
		approved(7, "user2", "User 2", 700), commented(7, "user4", "User 4", 750), approved(7, "user1", "User 1", 760),
		approved(7, "ghost", "Ghost Dev", 770), commented(7, "nameless", "", 780), approved(7, "user3", "User 3", 960)))
	matchForge(t, s, root, "user1", "acc-user1", "user2", "acc-user2", "user3", "acc-user3", "user4", "acc-user4", "ghost", "")
	rows, people := creditOf(t, s, root, regroup)
	want := []string{
		"20 800 10001 finished acc-user1 5.0 1.0 5.0 - - - - - - -",
		"20 800 10001 review - 0.0 0.0 5.0 10 2 approved - - - " + unmatchedNote + " [Ghost Dev (ghost on github.com)]",
		"20 800 10001 review - 0.0 0.0 5.0 10 2 commented - - - " + unmatchedNote + " [nameless on github.com]",
		"20 800 10001 review acc-user2 0.5 0.1 5.0 10 2 commented, approved 1 - - -",
		"20 800 10001 review acc-user3 0.5 0.1 5.0 10 2 commented, logged time 1 - - -",
		"20 800 10001 review acc-user4 0.0 0.0 5.0 10 2 commented 0 - - -",
	}
	if got := creditRowsOf(rows, "10001"); !reflect.DeepEqual(got, want) {
		t.Fatalf("credit\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Contains(people, "acc-user4") {
		t.Fatalf("people %v; want the person a forge act alone names", people)
	}
	after := paceOf(t, s, root, "10", regroup)
	if len(before) != 1 || len(after) != 1 || before[0].Review != 0.5 || after[0].Review != 1 || after[0].ReviewShares != 0.2 || after[0].Unread != before[0].Unread {
		t.Fatalf("pace %+v, then %+v; want User 2's review counted and no more tickets left out", before, after)
	}

	// A rule that counts approvals alone reads the forge too.
	only := []MirrorCreditRule{{ReviewShare: 10, ReviewNeed: 1, Approved: true, HoursPerPoint: 4}}
	rows, _ = creditOf(t, s, root, creditRegroup(t, only))
	if got, want := reviewsOf(rows, "10001")[2], "20 800 10001 review acc-user2 0.5 0.1 5.0 10 1 commented, approved 1 - - -"; got != want {
		t.Fatalf("approvals alone\n%s\nwant\n%s", got, want)
	}
}

// A rule that counts neither comments nor approvals reads no forge act
// and is kept from no ticket by a repository that is not read.
func TestARuleThatCountsNoForgeActReadsNone(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin, testOther}, testOrigin, testOther)
	applyForge(t, s, forgeWhole(testOrigin, []ForgeHistoryPR{keyPR(7, "WEB-10001")}, approved(7, "user2", "User 2", 700), approved(7, "ghost", "Ghost Dev", 770)))
	matchForge(t, s, root, "user2", "acc-user2")
	rules := []MirrorCreditRule{{ReviewShare: 10, ReviewNeed: 1, LoggedTime: true, HoursPerPoint: 4}}
	rows, _ := creditOf(t, s, root, creditRegroup(t, rules))
	want := []string{
		"20 800 10001 review acc-user2 0.0 0.0 5.0 10 1 commented 0 - - -",
		"20 800 10001 review acc-user3 0.5 0.1 5.0 10 1 commented, logged time 1 - - -",
	}
	if got := reviewsOf(rows, "10001"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A ticket's reviews are read from the forge only when every repository
// the connection's credit reads covers it (forgeCreditCoversSQL); else it
// has one review row, not counted, that names the first such repository
// and why. A repository whose reads fail keeps a sprint that closed since
// its last good read uncounted, and an open sprint counted with what it
// holds.
func TestCreditCountsATicketOnlyWhenEveryRepositoryCoversIt(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	regroup := creditRegroup(t, creditRules(true))
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin}, testOrigin)
	applyForge(t, s, forgeWhole(testOrigin, []ForgeHistoryPR{keyPR(7, "WEB-10001"), keyPR(8, "WEB-10005")},
		approved(7, "user2", "User 2", 700), approved(8, "user2", "User 2", 1400)))
	matchForge(t, s, root, "user2", "acc-user2")
	const created = int64(1_000_000)
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET created_at = ? WHERE root_id = ? AND issue_id IN ('10001', '10005')`, created, root)
	margin := ForgeHistoryClockMargin.Milliseconds()
	notRead := "20 800 10001 review - - - 5.0 10 2 - - - - pull requests of acme/web are not read yet"
	const refused = "GitHub refused the token"
	for _, c := range []struct {
		name     string
		reach    any
		actsOwed int
		failure  string
		goodAt   int64
		// want is the ticket's one review row, "" when its reviews count.
		want string
	}{
		{"no stream of the walk has proved a reach", nil, 0, "", 0, notRead},
		{"the whole window is read", int64(0), 0, "", 0, ""},
		{"acts of a pull request it read are unread", int64(0), 1, "", 0, notRead},
		{"read back to the margin before the ticket's creation", created - margin, 0, "", 0, ""},
		{"read back to just short of it", created - margin + 1, 0, "", 0, notRead},
		{"failing since before the sprint's end", int64(0), 0, refused, 950 + margin - 1,
			"20 800 10001 review - - - 5.0 10 2 - - - - pull requests of acme/web could not be read: " + refused},
		{"failing, with a good read the margin after the sprint's end", int64(0), 0, refused, 950 + margin, ""},
	} {
		sourceState(t, s, testOrigin, c.reach, c.actsOwed, c.failure, c.goodAt)
		rows, _ := creditOf(t, s, root, regroup)
		got := reviewsOf(rows, "10001")
		if c.want != "" && !reflect.DeepEqual(got, []string{c.want}) {
			t.Errorf("%s: reviews\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), c.want)
		}
		if counted := slices.Contains(got, "20 800 10001 review acc-user2 0.5 0.1 5.0 10 2 commented, approved 1 - - -"); c.want == "" && (!counted || len(got) != 2) {
			t.Errorf("%s: reviews\n%s\nwant User 2's and User 3's counted", c.name, strings.Join(got, "\n"))
		}
		// 10005 is done in the open sprint: a failing repository still
		// counts what it holds of it.
		open := reviewsOf(rows, "10005")
		if covered := c.reach != nil && c.actsOwed == 0 && c.reach.(int64) <= created-margin; covered != slices.Contains(open, "21 - 10005 review acc-user2 0.0 0.0 2.0 10 2 approved 0 - - -") {
			t.Errorf("%s: the open sprint's reviews\n%s\nwant User 2's approval counted: %t", c.name, strings.Join(open, "\n"), covered)
		}
	}
}

// The row of a ticket some repository does not cover names the first of
// them by its path and counts the rest, a failure before a repository not
// yet read only by that order.
func TestTheUnreadRowNamesTheFirstRepositoryAndCountsTheRest(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	regroup := creditRegroup(t, creditRules(true))
	all := []ForgeHistoryRef{testOrigin, testOther, testParent}
	forgeCreditSources(t, s, root, all, all...)
	note := func() string {
		t.Helper()
		rows, _ := creditOf(t, s, root, regroup)
		got := reviewsOf(rows, "10001")
		if len(got) != 1 {
			t.Fatalf("reviews\n%s\nwant one row not counted", strings.Join(got, "\n"))
		}
		return strings.TrimPrefix(got[0], "20 800 10001 review - - - 5.0 10 2 - - - - ")
	}
	if got, want := note(), "pull requests of acme/web and 2 more repositories are not read yet"; got != want {
		t.Fatalf("note %q; want %q", got, want)
	}
	sourceState(t, s, testOrigin, int64(0), 0, "", 0)
	sourceState(t, s, testOther, int64(0), 0, "GitHub refused the token", 0)
	if got, want := note(), "pull requests of other/web and 1 more repository could not be read: GitHub refused the token"; got != want {
		t.Fatalf("note %q; want %q", got, want)
	}
	sourceState(t, s, testOther, int64(0), 0, "", 0)
	if got, want := note(), "pull requests of up/web are not read yet"; got != want {
		t.Fatalf("note %q; want %q", got, want)
	}
}

// Credit reads the repositories the connection uses whole and, of the
// parent of one that is a fork, the pull requests whose head is the fork:
// another fork's pull request in the parent, and a repository the
// connection does not use, count nothing and keep no ticket unread, while
// the parent itself must cover the ticket.
func TestCreditReadsAForksPullRequestsInItsParent(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	regroup := creditRegroup(t, creditRules(true))
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin, testParent, testOther}, testOrigin)
	fork := forgeWhole(testOrigin, nil)
	fork.OwnHead, fork.Parent = "R_acme", testParent
	applyForge(t, s, fork)
	theirs := keyPR(10, "WEB-10001")
	theirs.HeadRepo = "R_else"
	applyForge(t, s, forgeWhole(testParent, []ForgeHistoryPR{keyPR(9, "WEB-10001"), theirs}, approved(9, "user2", "User 2", 700), approved(10, "user4", "User 4", 700)))
	other := ForgeHistoryRead{Source: testOther, PRs: []ForgeHistoryPR{keyPR(3, "WEB-10001")}, Acts: []ForgeHistoryAct{approved(3, "user4", "User 4", 700)}, WindowStart: 1}
	applyForge(t, s, other)
	// Only the account that acted on the fork's pull request is due a match.
	if got, _ := dueLogins(t, s, root, 10_000_000_000, ""); !slices.Equal(got, []string{"user2=User 2"}) {
		t.Fatalf("due = %v; want the account on the fork's pull request alone", got)
	}
	matchForge(t, s, root, "user2", "acc-user2", "user4", "acc-user4")
	rows, _ := creditOf(t, s, root, regroup)
	want := []string{
		"20 800 10001 review acc-user2 0.5 0.1 5.0 10 2 commented, approved 1 - - -",
		"20 800 10001 review acc-user3 0.5 0.1 5.0 10 2 commented, logged time 1 - - -",
	}
	if got := reviewsOf(rows, "10001"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	sourceState(t, s, testParent, nil, 0, "", 0)
	rows, _ = creditOf(t, s, root, regroup)
	if got, want := reviewsOf(rows, "10001"), []string{"20 800 10001 review - - - 5.0 10 2 - - - - pull requests of up/web are not read yet"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with the parent unread\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// When who finished a ticket is not known, a forge act of a person who
// had it is not counted as a review and is part of the row that says so,
// as the person's acts in the tracker are (10009 of
// TestTheActionsOfAPossibleFinisherAreNotCounted, with User 2 acting only
// on the forge).
func TestAForgeActOfAPossibleFinisherIsNotCounted(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	r := sectionRow("10009", "10009", "10001", 5, "20")
	r.Card.UpdatedAt, r.Card.AssigneeID = 900, "acc-user2"
	mustWriteRowPage(t, s, root, webPage("10", r), nil)
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{{IssueID: "10009",
		Changes: []MirrorChange{{Kind: MirrorChangeAssignee, At: 300, From: "acc-user1", To: "acc-user2"}}}}), MirrorCatalog{}, mirrorReadOf(root, 30)); err != nil {
		t.Fatal(err)
	}
	replaceActs(t, s, root, 40, nil,
		MirrorIssueActs{IssueID: "10009", Source: MirrorActComment, ReadFor: 900, Acts: []MirrorAct{act("c9", "acc-user3", 600, 0)}},
		MirrorIssueActs{IssueID: "10009", Source: MirrorActWorklog, ReadFor: 900})
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin}, testOrigin)
	applyForge(t, s, forgeWhole(testOrigin, nil))
	const unknown = "who had it when it finished is not known"
	rows, _ := creditOf(t, s, root, creditRegroup(t, creditRules(true)))
	if got, want := reviewsOf(rows, "10009"), []string{"20 - 10009 review acc-user3 0.0 0.0 5.0 10 2 commented 0 - - -"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before the forge acts\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	applyForge(t, s, forgeWhole(testOrigin, []ForgeHistoryPR{keyPR(7, "WEB-10009")}, approved(7, "user2", "User 2", 700), approved(7, "user3", "User 3", 710)))
	matchForge(t, s, root, "user2", "acc-user2", "user3", "acc-user3")
	rows, _ = creditOf(t, s, root, creditRegroup(t, creditRules(true)))
	want := []string{
		"20 - 10009 review - - - 5.0 10 2 - - - - " + unknown,
		"20 - 10009 review acc-user3 0.5 0.1 5.0 10 2 commented, approved 1 - - -",
	}
	if got := reviewsOf(rows, "10009"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// boardTotals is the totals revision of board of root.
func boardTotals(t *testing.T, s *Store, root int64, board string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(`SELECT totals_revision FROM tracker_cache_boards WHERE root_id = ? AND board_id = ?`, root, board).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// dueLogins are the accounts ListMirrorForgePeopleDue lists, each with a
// star when it is the account whose token reads its repository.
func dueLogins(t *testing.T, s *Store, root, now int64, viewer string) ([]string, bool) {
	t.Helper()
	due, more, err := s.ListMirrorForgePeopleDue(root, now, viewer)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range due {
		login := d.Login
		if d.Viewer {
			login += "*"
		}
		if d.Name != "" {
			login += "=" + d.Name
		}
		out = append(out, login)
	}
	return out, more
}

// The accounts due a match are the ones that acted on a pull request the
// connection's credit reads that names a ticket the mirror holds: never
// matched, named otherwise by the forge since, last checked a day ago, or
// the token's own account while it is matched to another person than the
// connection's. A match takes its account off the list, and changes what
// credit counts only when the person differs.
func TestForgePeopleAreDueUntilMatchedAndAgainADayLater(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin, testOther}, testOrigin)
	read := forgeWhole(testOrigin, []ForgeHistoryPR{keyPR(7, "WEB-10001"), keyPR(8, "WEB-424242")},
		approved(7, "user2", "User 2", 700), commented(7, "ghost", "Ghost Dev", 710), commented(7, "nameless", "", 720), approved(7, "me", "User 1", 730),
		approved(8, "stranger", "Stranger", 700))
	read.Viewer = "me"
	applyForge(t, s, read)
	applyForge(t, s, ForgeHistoryRead{Source: testOther, PRs: []ForgeHistoryPR{keyPR(3, "WEB-10001")}, Acts: []ForgeHistoryAct{approved(3, "outsider", "Outsider", 700)}, WindowStart: 1})
	const now = int64(10_000_000_000)
	day := mirrorForgePeopleRecheck.Milliseconds()
	if got, more := dueLogins(t, s, root, now, "acc-user1"); !slices.Equal(got, []string{"ghost=Ghost Dev", "me*=User 1", "nameless", "user2=User 2"}) || more {
		t.Fatalf("due = %v, more %t", got, more)
	}
	if got, _ := dueLogins(t, s, root, now, ""); !slices.Equal(got, []string{"ghost=Ghost Dev", "me=User 1", "nameless", "user2=User 2"}) {
		t.Fatalf("due with no account of the connection's own = %v", got)
	}
	match := func(at int64, person map[string]string) bool {
		t.Helper()
		var matches []MirrorForgeMatch
		for _, d := range []MirrorForgePerson{{Host: "github.com", Login: "ghost", Name: "Ghost Dev"}, {Host: "github.com", Login: "me", Name: "User 1"},
			{Host: "github.com", Login: "nameless"}, {Host: "github.com", Login: "user2", Name: "User 2"}} {
			matches = append(matches, MirrorForgeMatch{MirrorForgePerson: d, Person: person[d.Login]})
		}
		changed, err := s.SetMirrorForgePeople(root, nextMirrorRead(s, root), matches,
			MirrorCatalog{Users: []MirrorUser{{ID: "acc-user9", Name: "User 9", Data: []byte(`{}`)}}}, at, []string{"10"})
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	totals, other := boardTotals(t, s, root, "10"), boardTotals(t, s, root, "11")
	people := map[string]string{"me": "acc-user1", "user2": "acc-user2"}
	if !match(now, people) || boardTotals(t, s, root, "10") != totals+1 || boardTotals(t, s, root, "11") != other {
		t.Fatalf("the first matches: totals %d -> %d, the other board's %d -> %d; want the credit board's advanced once",
			totals, boardTotals(t, s, root, "10"), other, boardTotals(t, s, root, "11"))
	}
	assertRows(t, s.db, fmt.Sprintf(`SELECT name FROM tracker_cache_users WHERE root_id = %d AND user_id = 'acc-user9'`, root), "User 9")
	if got, _ := dueLogins(t, s, root, now+day, "acc-user1"); len(got) != 0 {
		t.Fatalf("due a day after the match less a moment = %v", got)
	}
	if got, _ := dueLogins(t, s, root, now+day+1, "acc-user1"); len(got) != 4 {
		t.Fatalf("due a day after the match = %v; want all four again", got)
	}
	// The same people, checked again: nothing credit counts changed.
	if match(now+day+1, people) || boardTotals(t, s, root, "10") != totals+1 {
		t.Fatalf("matching the same people again changed credit: totals %d", boardTotals(t, s, root, "10"))
	}
	// The forge names an account otherwise: it is due at once.
	applyForge(t, s, forgeWhole(testOrigin, nil, approved(7, "user2", "User Two", 700)))
	if got, _ := dueLogins(t, s, root, now+day+2, "acc-user1"); !slices.Equal(got, []string{"user2=User Two"}) {
		t.Fatalf("due after a rename = %v", got)
	}
	// The connection is another account's now: the token's account is due.
	if got, _ := dueLogins(t, s, root, now+day+2, "acc-user9"); !slices.Equal(got, []string{"me*=User 1", "user2=User Two"}) {
		t.Fatalf("due for another account of the connection = %v", got)
	}
	people["user2"] = "acc-user3"
	if !match(now+day+3, people) || boardTotals(t, s, root, "10") != totals+2 {
		t.Fatalf("a match to another person did not advance the totals: %d", boardTotals(t, s, root, "10"))
	}

	// One listing is a page: the never-matched first.
	var many []ForgeHistoryAct
	for i := range mirrorForgePeoplePage + 2 {
		many = append(many, commented(7, fmt.Sprintf("dev%02d", i), "", 700))
	}
	applyForge(t, s, forgeWhole(testOrigin, nil, many...))
	got, more := dueLogins(t, s, root, now+2*day+4, "acc-user1")
	if len(got) != mirrorForgePeoplePage || !more || got[0] != "dev00" || got[len(got)-1] != fmt.Sprintf("dev%02d", mirrorForgePeoplePage-1) {
		t.Fatalf("a page of %d, more %t: %v", len(got), more, got)
	}
}

// A match names an account once, with a time and a read of the root.
func TestSetMirrorForgePeopleRefusesAnInvalidMatch(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	one := MirrorForgeMatch{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "user2"}, Person: "acc-user2"}
	for name, c := range map[string]struct {
		matches []MirrorForgeMatch
		at      int64
	}{
		"no host":  {[]MirrorForgeMatch{{MirrorForgePerson: MirrorForgePerson{Login: "user2"}}}, 1},
		"no login": {[]MirrorForgeMatch{{MirrorForgePerson: MirrorForgePerson{Host: "github.com"}}}, 1},
		"repeated": {[]MirrorForgeMatch{one, one}, 1},
		"no time":  {[]MirrorForgeMatch{one}, 0},
	} {
		if _, err := s.SetMirrorForgePeople(root, nextMirrorRead(s, root), c.matches, MirrorCatalog{}, c.at, nil); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_forge_people`, "0")
}

// A change of the forge history advances the totals of the boards that
// count credit and hold a ticket of the keys it names whose credit can
// read a forge act: one done now, or a member of a closed sprint. Every
// such board, with all.
func TestAdvanceMirrorForgeCreditTellsTheBoardsWhoseCreditItMoves(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	regroup := creditRegroup(t, creditRules(true))
	boards := []MirrorActsBoard{{BoardID: "10", Regroup: regroup}, {BoardID: "11", Regroup: regroup}}
	totals := func() [2]int64 { return [2]int64{boardTotals(t, s, root, "10"), boardTotals(t, s, root, "11")} }
	was := totals()
	// 10001 is done in the closed sprint, 10005 done now in the open one,
	// 10006 a member of the closed sprint that is not done now (it may
	// have been at the sprint's end), 10003 is not done and in no sprint,
	// and no ticket is WEB-999.
	mustWriteRowPage(t, s, root, webPage("10", sectionRow("10006", "10006", "1", 1, "20")), nil)
	was = totals()
	issues, err := s.AdvanceMirrorForgeCredit(root, boards, []string{"WEB-10001", "WEB-10003", "WEB-10005", "WEB-10006", "WEB-999"}, false)
	if err != nil || !slices.Equal(issues, []string{"10001", "10005", "10006"}) || totals() != [2]int64{was[0] + 1, was[1]} {
		t.Fatalf("issues %v, %v, totals %v -> %v; want 10001, 10005 and 10006 and board 10 advanced once", issues, err, was, totals())
	}
	was = totals()
	for name, c := range map[string]struct {
		boards []MirrorActsBoard
		keys   []string
		all    bool
	}{
		"keys of no ticket credit counts": {boards, []string{"WEB-10003", "WEB-999"}, false},
		"no keys":                         {boards, nil, false},
		"no board":                        {nil, []string{"WEB-10001"}, true},
	} {
		if issues, err := s.AdvanceMirrorForgeCredit(root, c.boards, c.keys, c.all); err != nil || len(issues) != 0 || totals() != was {
			t.Errorf("%s: issues %v, %v, totals %v -> %v; want nothing told", name, issues, err, was, totals())
		}
	}
	if issues, err := s.AdvanceMirrorForgeCredit(root, boards, nil, true); err != nil || len(issues) != 0 || totals() != [2]int64{was[0] + 1, was[1] + 1} {
		t.Fatalf("all: issues %v, %v, totals %v -> %v; want every board advanced once", issues, err, was, totals())
	}
}

// The repositories whose last read failed are the ones the connection's
// credit reads, a fork's parent included, by their path.
func TestMirrorForgeFailuresListsTheConnectionsFailingRepositories(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin, testParent, testOther}, testOrigin)
	fork := forgeWhole(testOrigin, nil)
	fork.OwnHead, fork.Parent = "R_acme", testParent
	applyForge(t, s, fork)
	for _, ref := range []ForgeHistoryRef{testParent, testOther} {
		sourceState(t, s, ref, nil, 0, "GitHub refused the token for "+ref.Namespace, 0)
	}
	failed, err := s.MirrorForgeFailures(root)
	if want := []MirrorForgeFailure{{Repo: "up/web", Failure: "GitHub refused the token for up"}}; err != nil || !slices.Equal(failed, want) {
		t.Fatalf("failures %+v, %v; want %+v", failed, err, want)
	}
	sourceState(t, s, testOrigin, int64(0), 0, "GitHub is unreachable", 0)
	failed, err = s.MirrorForgeFailures(root)
	if err != nil || len(failed) != 2 || failed[0].Repo != "acme/web" || failed[1].Repo != "up/web" {
		t.Fatalf("failures %+v, %v; want acme/web then up/web", failed, err)
	}
}
