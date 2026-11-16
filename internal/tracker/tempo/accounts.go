package tempo

import (
	"context"
	"fmt"

	"agent-overflow/internal/tracker/trackerapi"
)

// AccountStatus is a Tempo account's status.
type AccountStatus string

// The account statuses Tempo documents.
const (
	AccountOpen     AccountStatus = "OPEN"
	AccountClosed   AccountStatus = "CLOSED"
	AccountArchived AccountStatus = "ARCHIVED"
)

// Account is a Tempo account, the value of an account work attribute.
type Account struct {
	ID     int64
	Key    string // what a worklog's account attribute stores
	Name   string
	Status AccountStatus
	Global bool
}

type accountJSON struct {
	ID     int64  `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Global bool   `json:"global"`
}

// maxAccounts bounds one Accounts listing.
const maxAccounts = 20_000

// AccountQuery selects accounts. An empty query lists every account.
type AccountQuery struct {
	// Statuses keeps accounts in any of these statuses; empty keeps all.
	Statuses []AccountStatus
	// Keys keeps only these accounts; empty keeps all.
	Keys []string
}

// Accounts lists the accounts q selects, refusing a listing of more than
// 20,000. The list is not cached: an account picker reads it when it
// opens.
func (c *Client) Accounts(ctx context.Context, q AccountQuery) ([]Account, error) {
	out, err := c.accounts(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("tempo: list accounts: %w", err)
	}
	return out, nil
}

func (c *Client) accounts(ctx context.Context, q AccountQuery) ([]Account, error) {
	body := struct {
		Statuses []AccountStatus `json:"statuses,omitempty"`
		Keys     []string        `json:"keys,omitempty"`
	}{q.Statuses, q.Keys}
	var out []Account
	err := pages(ctx, c.http, trackerapi.Request{Method: "POST", Path: "4/accounts/search", Body: body, ReadOnly: true},
		func(list []accountJSON) error {
			if len(out)+len(list) > maxAccounts {
				return fmt.Errorf("more than %d accounts match, the most one listing holds", maxAccounts)
			}
			for _, a := range list {
				out = append(out, Account{ID: a.ID, Key: a.Key, Name: a.Name, Status: AccountStatus(a.Status), Global: a.Global})
			}
			return nil
		})
	return out, err
}
