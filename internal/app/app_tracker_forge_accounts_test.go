package app

import (
	"errors"
	"reflect"
	"testing"

	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// The forge account methods reach the service: a person's word is stored
// and answered in the list, the people search asks the tracker, and each
// refusal says what is wrong.
func TestTrackerForgeAccountMethodsReachTheService(t *testing.T) {
	t.Parallel()
	ws := trackerToolsLinear("ENG")
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: trackerBoardSite(), linear: &ws})
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	if got, err := f.a.TrackerForgeAccounts(ctx, f.conn.ID); err != nil || len(got.Accounts) != 0 || got.More {
		t.Fatalf("TrackerForgeAccounts before any pairing = %+v, %v", got, err)
	}
	people, err := f.a.TrackerPeople(ctx, f.conn.ID, nil, "User")
	if want := []trackerapp.UserView{{ID: "acc-user1", Name: "User 1", AvatarURL: "https://avatar.example.com/acc-user1/48.png"}}; err != nil || !reflect.DeepEqual(people.Users, want) {
		t.Fatalf("TrackerPeople = %+v, %v; want %+v", people, err, want)
	}
	word := trackerapp.ForgeAccountInput{Host: "github.com", Login: "user1-dev", Who: trackerapp.ForgeAccountIsPerson, Person: "acc-user1", PersonName: "User 1"}
	want := trackerapp.ForgeAccountsView{Accounts: []trackerapp.ForgeAccountView{{
		Host: "github.com", Login: "user1-dev", Person: &trackerapp.UserView{ID: "acc-user1", Name: "User 1"}, How: trackerapp.ForgeAccountPaired,
	}}}
	if got, err := f.a.SetTrackerForgeAccount(ctx, f.conn.ID, word); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("SetTrackerForgeAccount = %+v, %v; want %+v", got, err, want)
	}
	if got, err := f.a.TrackerForgeAccounts(ctx, f.conn.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("TrackerForgeAccounts = %+v, %v; want %+v", got, err, want)
	}
	word.Who, word.Person, word.PersonName = trackerapp.ForgeAccountIsByName, "", ""
	if got, err := f.a.SetTrackerForgeAccount(ctx, f.conn.ID, word); err != nil || len(got.Accounts) != 0 {
		t.Fatalf("SetTrackerForgeAccount by name = %+v, %v; want no account left", got, err)
	}

	word.Who = "someone"
	if _, err := f.a.SetTrackerForgeAccount(ctx, f.conn.ID, word); !hasCode(err, "tracker_request_invalid") || !errors.Is(err, trackerapp.ErrInvalidRequest) {
		t.Fatalf("an unknown word = %v", err)
	}
	word.Who = trackerapp.ForgeAccountIsNoPerson
	if _, err := f.a.SetTrackerForgeAccount(ctx, "no-such-connection", word); !hasCode(err, "tracker_connection_missing") {
		t.Fatalf("a word for no connection = %v", err)
	}
	if _, err := f.a.TrackerForgeAccounts(ctx, "no-such-connection"); !hasCode(err, "tracker_connection_missing") {
		t.Fatalf("the accounts of no connection = %v", err)
	}
	if _, err := f.a.TrackerPeople(ctx, "no-such-connection", nil, "User"); !hasCode(err, "tracker_connection_missing") {
		t.Fatalf("the people of no connection = %v", err)
	}
	linear := f.addLinear(t, ws)
	if _, err := f.a.TrackerForgeAccounts(ctx, linear.ID); !hasCode(err, "tracker_request_invalid") {
		t.Fatalf("the accounts of a Linear connection = %v", err)
	}
	if _, err := f.a.TrackerPeople(ctx, linear.ID, nil, "User"); !hasCode(err, "tracker_unsupported") {
		t.Fatalf("the people of a Linear connection = %v", err)
	}
}
