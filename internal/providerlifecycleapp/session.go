package providerlifecycleapp

import (
	"encoding/json"
	"log"
	"time"

	"agent-overflow/internal/provider"
)

// PrepareEvent attributes account-scoped quota and records process activity
// before root performs triage or any downstream event reaction.
func (s *Service) PrepareEvent(
	threadID, sessionToken string,
	event provider.ProviderEvent,
) provider.ProviderEvent {
	if event.Kind == provider.EventRateLimits {
		event = s.AttributeRateLimits(event, threadID, sessionToken)
	}
	if s.deps.Sessions.RecordActivity != nil {
		s.deps.Sessions.RecordActivity(
			threadID, sessionToken, event.Kind, event.Content, time.Now(),
		)
	}
	return event
}

func (s *Service) AttributeRateLimits(
	event provider.ProviderEvent,
	threadID, sessionToken string,
) provider.ProviderEvent {
	if s.deps.Sessions.Account == nil || len(event.Meta) == 0 {
		return event
	}
	runtime, ok := s.deps.Sessions.Account(threadID)
	if !ok || runtime.SessionToken != sessionToken || runtime.CredentialAccountID == "" {
		return event
	}
	var snapshot provider.RateLimitsSnapshot
	if err := json.Unmarshal(event.Meta, &snapshot); err != nil || snapshot.AccountID != "" {
		return event
	}
	snapshot.AccountID = runtime.CredentialAccountID
	meta, err := json.Marshal(snapshot)
	if err != nil {
		log.Printf("provider events: attribute rate limits for thread %s: %v", threadID, err)
		return event
	}
	event.Meta = meta
	return event
}

func (s *Service) RecordActivity(threadID, sessionToken string, kind provider.EventKind, content string) {
	if s.deps.Sessions.RecordActivity != nil {
		s.deps.Sessions.RecordActivity(threadID, sessionToken, kind, content, time.Now())
	}
}

// SessionAccount projects the account one live provider process is using.
func (s *Service) SessionAccount(threadID string) (SessionAccount, bool) {
	if s.deps.Sessions.Account == nil {
		return SessionAccount{}, false
	}
	runtime, ok := s.deps.Sessions.Account(threadID)
	if !ok || runtime.CredentialAccountID == "" {
		return SessionAccount{}, false
	}
	result := SessionAccount{
		ThreadID: threadID, Provider: runtime.Provider,
		AccountID:  runtime.CredentialAccountID,
		Account:    runtime.CredentialAccount,
		Generation: runtime.CredentialGeneration,
	}
	if result.Account != (provider.AccountInfo{}) || s.deps.Accounts.Account == nil {
		return result, true
	}
	account, found := s.deps.Accounts.Account(runtime.Provider, runtime.CredentialAccountID)
	if !found {
		return result, true
	}
	result.Account = provider.AccountInfo{
		Email: account.Email, DisplayName: account.DisplayName,
		SubscriptionType: account.SubscriptionType,
		TokenSource:      account.TokenSource, APIProvider: account.APIProvider,
	}
	return result, true
}
