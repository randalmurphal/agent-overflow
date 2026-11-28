package app

import (
	"context"

	"agent-overflow/internal/trackerapp"
)

// Who each forge account is in a connection's tracker, as a project's
// Points and credit page lists it and a person corrects it
// (trackerapp/forge_accounts.go). Each method names its connection first,
// so its client routes it to the computer that holds the connection.

// TrackerForgeAccounts lists the forge accounts whose approvals and
// comments the connection's credit reads, each with the tracker person
// it counts for and how that is known. It reads this computer's copy and
// makes no tracker or forge request.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerForgeAccounts(ctx context.Context, connectionID string) (trackerapp.ForgeAccountsView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.ForgeAccountsView{}, err
	}
	out, err := svc.ForgeAccounts(connectionID)
	return out, trackerError(err)
}

// SetTrackerForgeAccount records who a person says a forge account of the
// connection is (a person of its tracker, no person, or whoever its name
// matches) and answers the accounts as they then are. The connection's
// boards whose credit reads the forge count again when it changed who
// the account is.
//
//ao:scope tracker:operate
//ao:route home
func (a *App) SetTrackerForgeAccount(ctx context.Context, connectionID string, account trackerapp.ForgeAccountInput) (trackerapp.ForgeAccountsView, error) {
	svc, err := a.trackerService()
	if err != nil {
		return trackerapp.ForgeAccountsView{}, err
	}
	out, err := svc.SetForgeAccount(connectionID, account)
	return out, trackerError(err)
}

// TrackerPeople lists the people of the connection's tracker text
// matches, for picking who a forge account is. projects are the tracker
// projects whose assignable people answer when the site's own search
// names none.
//
//ao:scope tracker:read
//ao:route home
func (a *App) TrackerPeople(ctx context.Context, connectionID string, projects []string, text string) (trackerapp.UserListView, error) {
	return trackerFormRead(a, ctx, "TrackerPeople", "Finding the people of the tracker", func(ctx context.Context, svc *trackerapp.Service) (trackerapp.UserListView, error) {
		return svc.People(ctx, connectionID, projects, text)
	})
}
