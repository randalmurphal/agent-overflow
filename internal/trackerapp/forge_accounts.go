package trackerapp

import (
	"context"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// Who a forge account is: the list a person reads of the accounts a
// connection's credit counts (ForgeAccounts), and the person's own word
// on one (SetForgeAccount), which credit reads before the name match the
// people unit makes (run.forgePeople, store tracker_forge_pairings).

// How a forge account's person is known (ForgeAccountView.How).
const (
	// ForgeAccountPaired: a person said who it is.
	ForgeAccountPaired = "paired"
	// ForgeAccountNotAPerson: a person said it is no person's (a bot);
	// its acts are left out of credit.
	ForgeAccountNotAPerson = "not_a_person"
	// ForgeAccountOwn: it is the account the app reads the forge as, so
	// it is the connection's own person.
	ForgeAccountOwn = "own"
	// ForgeAccountByName: one person of the tracker has the name the
	// forge gives it.
	ForgeAccountByName = "name"
	// ForgeAccountNobody: no person of the tracker, or more than one, has
	// the name the forge gives it; its acts are listed and count nothing.
	ForgeAccountNobody = "nobody"
	// ForgeAccountUnchecked: the tracker has not been asked yet.
	ForgeAccountUnchecked = "unchecked"
)

// ForgeAccountView is a forge account that acted on a pull request a
// connection's credit reads: its login on its host, the forge's name for
// it, the tracker person its acts count for (none for nobody) and how
// that is known.
type ForgeAccountView struct {
	Host   string    `json:"host"`
	Login  string    `json:"login"`
	Name   string    `json:"name,omitempty"`
	Person *UserView `json:"person,omitempty"`
	How    string    `json:"how"`
}

// ForgeAccountsView is a connection's forge accounts, the ones that are
// nobody's first; More reports that more accounts acted than it lists.
type ForgeAccountsView struct {
	Accounts []ForgeAccountView `json:"accounts"`
	More     bool               `json:"more,omitempty"`
}

// ForgeAccounts lists the forge accounts connection id's credit reads
// with who each is (store ListMirrorForgeAccounts), from this computer's
// copy: it asks no tracker and no forge. A connection of a tracker credit
// does not count is refused with ErrInvalidRequest.
func (s *Service) ForgeAccounts(id string) (ForgeAccountsView, error) {
	if err := s.enter(); err != nil {
		return ForgeAccountsView{}, err
	}
	defer s.leave()
	row, err := s.creditConnection(id)
	if err != nil {
		return ForgeAccountsView{}, err
	}
	return s.forgeAccounts(row)
}

func (s *Service) forgeAccounts(row store.TrackerConnection) (ForgeAccountsView, error) {
	accounts, more, err := s.store.ListMirrorForgeAccounts(row.ID)
	if err != nil {
		return ForgeAccountsView{}, fmt.Errorf("trackerapp: list the forge accounts of %s: %w", row.ID, err)
	}
	out := ForgeAccountsView{Accounts: make([]ForgeAccountView, len(accounts)), More: more}
	for i, a := range accounts {
		v := ForgeAccountView{Host: a.Host, Login: a.Login, Name: a.Name}
		switch {
		case a.Paired && a.Person == "":
			v.How = ForgeAccountNotAPerson
		case a.Paired:
			v.How = ForgeAccountPaired
		case !a.Checked:
			v.How = ForgeAccountUnchecked
		case a.Person == "":
			v.How = ForgeAccountNobody
		case a.Own && a.Person == row.VerifiedAccountID:
			v.How = ForgeAccountOwn
		default:
			v.How = ForgeAccountByName
		}
		if a.Person != "" {
			v.Person = &UserView{ID: a.Person, Name: a.PersonName}
		}
		out.Accounts[i] = v
	}
	return out, nil
}

// creditConnection reads connection id for a forge account call: one of
// a tracker whose credit the app counts. A connection that is not stored
// is refused with ErrNotFound, any other with ErrInvalidRequest.
func (s *Service) creditConnection(id string) (store.TrackerConnection, error) {
	if id == "" {
		return store.TrackerConnection{}, fmt.Errorf("%w: a forge account call needs a connection", ErrInvalidRequest)
	}
	row, err := s.get(id)
	if err != nil {
		return store.TrackerConnection{}, err
	}
	if !capabilitiesOf(row.Kind).Evidence {
		return store.TrackerConnection{}, fmt.Errorf("%w: credit is not counted for a %s connection, so it has no forge accounts", ErrInvalidRequest, row.Kind)
	}
	return row, nil
}

// Who a person says a forge account is (ForgeAccountInput.Who).
const (
	// ForgeAccountIsPerson pairs it with ForgeAccountInput.Person.
	ForgeAccountIsPerson = "person"
	// ForgeAccountIsNoPerson leaves its acts out of credit.
	ForgeAccountIsNoPerson = "not_a_person"
	// ForgeAccountIsByName removes the person's word, so the account is
	// whoever its name matches again.
	ForgeAccountIsByName = "by_name"
)

// ForgeAccountInput is a person's word on who the forge account Login on
// Host is: Who, with the tracker Person and the name they were picked by
// for ForgeAccountIsPerson.
type ForgeAccountInput struct {
	Host       string `json:"host"`
	Login      string `json:"login"`
	Who        string `json:"who"`
	Person     string `json:"person,omitempty"`
	PersonName string `json:"person_name,omitempty"`
}

// SetForgeAccount records who a person says a forge account of
// connection id is and returns the accounts as they then are. A change
// of who the account is advances the totals of the connection's boards
// whose credit reads the forge in the write's own transaction, and has
// them count again (tellRecount). An input that names no account, a Who
// that is none of the three, a person where none is taken or none where
// one is, is refused with ErrInvalidRequest before any write.
func (s *Service) SetForgeAccount(id string, in ForgeAccountInput) (ForgeAccountsView, error) {
	if err := s.enter(); err != nil {
		return ForgeAccountsView{}, err
	}
	defer s.leave()
	row, err := s.creditConnection(id)
	if err != nil {
		return ForgeAccountsView{}, err
	}
	person := in.Who == ForgeAccountIsPerson
	switch {
	case in.Host == "" || in.Login == "":
		return ForgeAccountsView{}, fmt.Errorf("%w: a forge account is named by its host and login", ErrInvalidRequest)
	case in.Who != ForgeAccountIsPerson && in.Who != ForgeAccountIsNoPerson && in.Who != ForgeAccountIsByName:
		return ForgeAccountsView{}, fmt.Errorf("%w: %q does not say who a forge account is", ErrInvalidRequest, in.Who)
	case person != (in.Person != "") || !person && in.PersonName != "":
		return ForgeAccountsView{}, fmt.Errorf("%w: a person is named for %q and for nothing else", ErrInvalidRequest, ForgeAccountIsPerson)
	}
	counting, err := s.forgeBoards(row)
	if err != nil {
		return ForgeAccountsView{}, err
	}
	boards := make([]string, len(counting))
	for i, b := range counting {
		boards[i] = b.BoardID
	}
	var changed bool
	if in.Who == ForgeAccountIsByName {
		changed, err = s.store.DeleteTrackerForgePairing(id, in.Host, in.Login, boards)
	} else {
		changed, err = s.store.SetTrackerForgePairing(store.TrackerForgePairing{ConnectionID: id, Host: in.Host, Login: in.Login, Person: in.Person, PersonName: in.PersonName}, boards)
	}
	if err != nil {
		return ForgeAccountsView{}, fmt.Errorf("trackerapp: say who %s on %s is for %s: %w", in.Login, in.Host, id, err)
	}
	if changed && len(boards) > 0 {
		s.tellRecount(id)
	}
	return s.forgeAccounts(row)
}

// People lists the people of connection id's tracker whose name or email
// starts with text, for picking who a forge account is
// (tracker.Provider.People): projects are the tracker projects whose
// assignable people answer when the site names none.
func (s *Service) People(ctx context.Context, id string, projects []string, text string) (UserListView, error) {
	return ask(s, ctx, id, func(r *run) (UserListView, error) {
		in := make([]tracker.ProjectID, len(projects))
		for i, p := range projects {
			in[i] = tracker.ProjectID(p)
		}
		l, err := r.p.People(r.ctx, in, text)
		out := UserListView{Users: make([]UserView, len(l.Users)), More: l.More}
		for i, u := range l.Users {
			out.Users[i] = UserView{ID: string(u.ID), Name: u.Name, AvatarURL: u.AvatarURL}
		}
		return out, call(err)
	})
}
