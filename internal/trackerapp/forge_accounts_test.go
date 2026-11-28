package trackerapp

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// accountsOf lists a view's accounts as "login how person|name".
func accountsOf(v ForgeAccountsView) []string {
	out := make([]string, len(v.Accounts))
	for i, a := range v.Accounts {
		person := "-"
		if a.Person != nil {
			person = a.Person.ID + "|" + a.Person.Name
		}
		out[i] = fmt.Sprintf("%s %s %s", a.Login, a.How, person)
	}
	return out
}

// The forge accounts a connection's credit reads are listed with who
// each is and how that is known: not asked of the tracker yet, then the
// connection's own, the one person of its name, or nobody.
func TestForgeAccountsSayWhoEachAccountIs(t *testing.T) {
	m, _, _ := forgeFixture(t)
	m.countCredit(t, m.conn.ID, true)
	got := must[ForgeAccountsView](t)(m.svc.ForgeAccounts(m.conn.ID))
	if want := []string{"ghost unchecked -", "nameless unchecked -", "me unchecked -", "user2 unchecked -"}; !slices.Equal(accountsOf(got), want) || got.More {
		t.Fatalf("before the tracker is asked %q; want %q", accountsOf(got), want)
	}
	if got.Accounts[0].Host != "github.com" || got.Accounts[0].Name != "Ghost Dev" || got.Accounts[1].Name != "" {
		t.Fatalf("accounts %+v; want each with its host and the forge's name for it", got.Accounts[:2])
	}
	m.peopleTick(t)
	got = must[ForgeAccountsView](t)(m.svc.ForgeAccounts(m.conn.ID))
	want := []string{"ghost nobody -", "nameless nobody -", "me own acc-user1|User 1", "user2 name acc-user2|User 2"}
	if !slices.Equal(accountsOf(got), want) {
		t.Fatalf("after the tracker is asked %q; want %q", accountsOf(got), want)
	}
}

// A person's word on a forge account is stored, shown in the list it
// answers with, and counts at once: a change of who the account is
// advances the totals of the boards whose credit reads the forge and has
// them count again, and the same word again changes nothing. The people
// unit asks the tracker for no account a person paired.
func TestAPersonsWordOnAForgeAccountCountsAtOnce(t *testing.T) {
	m, _, asked := forgeFixture(t)
	m.countCredit(t, m.conn.ID, true)
	m.subscribeShown(t, m.project())
	say := func(in ForgeAccountInput) (view ForgeAccountsView, told bool) {
		t.Helper()
		n, totals := m.boardEventCount(), m.totals(t)
		view = must[ForgeAccountsView](t)(m.svc.SetForgeAccount(m.conn.ID, in))
		if told = m.recountSince(n); told != (m.totals(t) == totals+1) || !told && m.totals(t) != totals {
			t.Fatalf("recount %t with the totals %d -> %d", told, totals, m.totals(t))
		}
		return view, told
	}
	ghost := ForgeAccountInput{Host: "github.com", Login: "ghost", Who: ForgeAccountIsPerson, Person: "acc-user3", PersonName: "User 3"}
	view, told := say(ghost)
	if !told || !slices.Contains(accountsOf(view), "ghost paired acc-user3|User 3") {
		t.Fatalf("pairing ghost: told %t, accounts %q", told, accountsOf(view))
	}
	if _, told := say(ghost); told {
		t.Fatal("the same pairing again had the boards count again")
	}
	m.peopleTick(t)
	if got, want := asked(), []string{"User 2"}; !slices.Equal(got, want) {
		t.Fatalf("the tracker was asked for %q; want %q, the paired account left alone", got, want)
	}
	view, told = say(ForgeAccountInput{Host: "github.com", Login: "nameless", Who: ForgeAccountIsNoPerson})
	if want := []string{"ghost paired acc-user3|User 3", "nameless not_a_person -", "me own acc-user1|User 1", "user2 name acc-user2|User 2"}; !told || !slices.Equal(accountsOf(view), want) {
		t.Fatalf("no person for nameless: told %t, accounts %q; want %q", told, accountsOf(view), want)
	}
	view, told = say(ForgeAccountInput{Host: "github.com", Login: "ghost", Who: ForgeAccountIsByName})
	if !told || accountsOf(view)[0] != "ghost unchecked -" {
		t.Fatalf("ghost by name again: told %t, accounts %q", told, accountsOf(view))
	}
	if _, told := say(ForgeAccountInput{Host: "github.com", Login: "ghost", Who: ForgeAccountIsByName}); told {
		t.Fatal("by name with no pairing had the boards count again")
	}
}

// A word that names no account, says nothing the app knows, names a
// person where none is taken or none where one is, or is for a
// connection that is not stored or whose tracker credit does not count,
// is refused before any write.
func TestAnInvalidWordOnAForgeAccountIsRefused(t *testing.T) {
	m, cs, _ := forgeFixture(t)
	m.countCredit(t, m.conn.ID, true)
	linear := m.addLinear()
	cs.take()
	for name, c := range map[string]struct {
		conn string
		in   ForgeAccountInput
	}{
		"no host":                  {m.conn.ID, ForgeAccountInput{Login: "ghost", Who: ForgeAccountIsNoPerson}},
		"no login":                 {m.conn.ID, ForgeAccountInput{Host: "github.com", Who: ForgeAccountIsNoPerson}},
		"no word":                  {m.conn.ID, ForgeAccountInput{Host: "github.com", Login: "ghost"}},
		"an unknown word":          {m.conn.ID, ForgeAccountInput{Host: "github.com", Login: "ghost", Who: "someone"}},
		"a person with no ID":      {m.conn.ID, ForgeAccountInput{Host: "github.com", Login: "ghost", Who: ForgeAccountIsPerson, PersonName: "User 3"}},
		"a person for no person":   {m.conn.ID, ForgeAccountInput{Host: "github.com", Login: "ghost", Who: ForgeAccountIsNoPerson, Person: "acc-user3"}},
		"a name for by name":       {m.conn.ID, ForgeAccountInput{Host: "github.com", Login: "ghost", Who: ForgeAccountIsByName, PersonName: "User 3"}},
		"no connection":            {"", ForgeAccountInput{Host: "github.com", Login: "ghost", Who: ForgeAccountIsNoPerson}},
		"a tracker without credit": {linear.ID, ForgeAccountInput{Host: "github.com", Login: "ghost", Who: ForgeAccountIsNoPerson}},
	} {
		if _, err := m.svc.SetForgeAccount(c.conn, c.in); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v; want ErrInvalidRequest", name, err)
		}
	}
	for _, who := range []string{ForgeAccountIsNoPerson, ForgeAccountIsByName} {
		if _, err := m.svc.SetForgeAccount("no-such-connection", ForgeAccountInput{Host: "github.com", Login: "ghost", Who: who}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s for a connection not stored: %v; want ErrNotFound", who, err)
		}
	}
	if _, err := m.svc.ForgeAccounts("no-such-connection"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the accounts of a connection not stored: %v; want ErrNotFound", err)
	}
	if calls := cs.take(); calls["SetTrackerForgePairing"]+calls["DeleteTrackerForgePairing"] != 0 {
		t.Fatalf("store calls %v; want no write for a refused word", calls)
	}
	for _, conn := range []string{"", linear.ID} {
		if _, err := m.svc.ForgeAccounts(conn); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("the accounts of %q: %v; want ErrInvalidRequest", conn, err)
		}
	}
}

// The people search asks the connection's tracker for the text in the
// project and answers its people; a failure of the tracker is the
// person's to read.
func TestPeopleAsksTheTracker(t *testing.T) {
	m, _, _ := forgeFixture(t)
	var asked []string
	fail := false
	m.p.set(func(p *scripted) {
		p.people = func(projects []tracker.ProjectID, text string) (tracker.UserList, error) {
			asked = append(asked, fmt.Sprint(projects)+"/"+text)
			if fail {
				return tracker.UserList{}, &trackerapi.StatusError{Status: 500}
			}
			return tracker.UserList{Users: []tracker.User{{ID: "acc-user3", Name: "User 3", AvatarURL: "https://example.com/a.png", Active: true}}, More: true}, nil
		}
	})
	got := must[UserListView](t)(m.svc.People(t.Context(), m.conn.ID, []string{"10000", "10001"}, "use"))
	want := UserListView{Users: []UserView{{ID: "acc-user3", Name: "User 3", AvatarURL: "https://example.com/a.png"}}, More: true}
	if !reflect.DeepEqual(got, want) || !slices.Equal(asked, []string{"[10000 10001]/use"}) {
		t.Fatalf("People = %+v after asking %q; want %+v", got, asked, want)
	}
	fail = true
	if _, err := m.svc.People(t.Context(), m.conn.ID, []string{"10000"}, "use"); err == nil {
		t.Fatal("a failed search answered")
	} else if _, ok := errors.AsType[*CallFailure](err); !ok {
		t.Fatalf("a failed search: %v; want a call failure", err)
	}
}
