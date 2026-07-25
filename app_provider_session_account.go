package main

import (
	"time"

	"agent-overflow/internal/provider"
)

// ProviderSessionAccountEvent is the account a live provider process is
// currently using for one thread. It is deliberately separate from the
// provider-global selected account: an active Codex process can keep using the
// old account while the newly selected account waits for its safe reconnect.
type ProviderSessionAccountEvent struct {
	ThreadID   string               `json:"threadId"`
	Provider   string               `json:"provider"`
	AccountID  string               `json:"accountId,omitempty"`
	Account    provider.AccountInfo `json:"account"`
	Generation uint64               `json:"generation,omitempty"`
	Connected  bool                 `json:"connected"`
}

func (a *App) providerSessionAccount(threadID string) *ProviderSessionAccountEvent {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.credentialAccountID == "" {
		return nil
	}
	event := &ProviderSessionAccountEvent{
		ThreadID:   threadID,
		Provider:   sess.provider,
		AccountID:  sess.credentialAccountID,
		Account:    sess.credentialAccount,
		Generation: sess.credentialGeneration,
		Connected:  true,
	}
	if event.Account != (provider.AccountInfo{}) {
		return event
	}
	if a.providerAccounts == nil {
		return event
	}
	account, ok := a.providerAccounts.Get(sess.provider, sess.credentialAccountID, time.Now())
	if !ok {
		return event
	}
	event.Account = provider.AccountInfo{
		Email:            account.Email,
		DisplayName:      account.DisplayName,
		SubscriptionType: account.SubscriptionType,
		TokenSource:      account.TokenSource,
		APIProvider:      account.APIProvider,
	}
	return event
}

func (a *App) emitProviderSessionAccount(threadID string) {
	if event := a.providerSessionAccount(threadID); event != nil {
		a.emit("provider:session_account", *event)
	}
}

func (a *App) emitProviderSessionDisconnected(threadID, providerName string) {
	a.emit("provider:session_account", ProviderSessionAccountEvent{
		ThreadID:  threadID,
		Provider:  providerName,
		Connected: false,
	})
}
