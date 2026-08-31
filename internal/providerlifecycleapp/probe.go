package providerlifecycleapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

func (s *Service) probeClaudeRateLimits(ctx context.Context) error {
	if s.shuttingDown() {
		return nil
	}
	if s.deps.Accounts.Reconcile != nil {
		if err := s.deps.Accounts.Reconcile(string(provider.Claude)); err != nil {
			log.Printf("claude: reconcile external account before rate-limit probe: %v", err)
			return err
		}
	}
	selection := s.selection(string(provider.Claude))
	if selection.AccountID != "" {
		err := s.deps.Accounts.RefreshUsage(ctx, string(provider.Claude), selection.AccountID)
		if err != nil {
			if errors.Is(err, claude.ErrNoCredentials) {
				return nil
			}
			log.Printf("claude: rate-limit probe: %v", err)
		}
		return err
	}
	if remaining := s.backoff.Remaining(string(provider.Claude), ""); remaining > 0 {
		return fmt.Errorf(
			"the usage endpoint rate limited this login; try again in %s",
			remaining.Round(time.Second),
		)
	}
	if s.deps.Claude.ProbeRateLimits == nil {
		return errors.New("claude rate-limit probe unavailable")
	}
	var client = s.deps.Claude.HTTPClient
	if client == nil {
		return errors.New("claude rate-limit HTTP client unavailable")
	}
	snapshot, err := s.deps.Claude.ProbeRateLimits(ctx, client())
	s.backoff.Note(string(provider.Claude), "", err)
	if err != nil {
		if errors.Is(err, claude.ErrNoCredentials) {
			return nil
		}
		log.Printf("claude: rate-limit probe: %v", err)
		return err
	}
	s.EmitSnapshot(snapshot)
	return nil
}

func (s *Service) ProbeClaudeRateLimits(ctx context.Context) error {
	return s.probeClaudeRateLimits(ctx)
}

func (s *Service) probeCodexRateLimits(ctx context.Context) error {
	if s.shuttingDown() {
		return nil
	}
	if s.deps.Accounts.Reconcile != nil {
		if err := s.deps.Accounts.Reconcile(string(provider.Codex)); err != nil {
			log.Printf("codex: reconcile external account before rate-limit probe: %v", err)
			return err
		}
	}
	selection := s.selection(string(provider.Codex))
	if selection.AccountID != "" {
		err := s.deps.Accounts.RefreshUsage(ctx, string(provider.Codex), selection.AccountID)
		if err != nil {
			log.Printf("codex: rate-limit probe: %v", err)
		}
		return err
	}
	if s.deps.Codex.Binary == nil || s.deps.Codex.ProbeConfig == nil || s.deps.Codex.Probe == nil {
		return errors.New("codex rate-limit probe unavailable")
	}
	binary := s.deps.Codex.Binary()
	var observedSnapshot *provider.RateLimitsSnapshot
	config := s.deps.Codex.ProbeConfig(binary)
	config.OnSnapshot = func(snapshot provider.RateLimitsSnapshot) {
		copy := CloneSnapshot(snapshot)
		observedSnapshot = &copy
	}
	info, err := s.deps.Codex.Probe(ctx, config)
	if err != nil {
		log.Printf("codex: rate-limit probe: %v", err)
		return err
	}
	if observedSnapshot == nil {
		return nil
	}
	if s.deps.Accounts.ResolveObserved == nil {
		return errors.New("codex rate-limit identity resolver unavailable")
	}
	accountID, updated, err := s.deps.Accounts.ResolveObserved(
		string(provider.Codex), selection.AccountID, info,
	)
	if err != nil {
		log.Printf("codex: rate-limit identity: %v", err)
		return err
	}
	if updated != nil && s.deps.Accounts.PublishObserved != nil {
		s.deps.Accounts.PublishObserved(string(provider.Codex), *updated)
	}
	observedSnapshot.AccountID = accountID
	s.EmitSnapshot(*observedSnapshot)
	return nil
}

func (s *Service) ProbeCodexRateLimits(ctx context.Context) error {
	return s.probeCodexRateLimits(ctx)
}

func (s *Service) selection(providerName string) Selection {
	if s.deps.Accounts.Selection == nil {
		return Selection{}
	}
	return s.deps.Accounts.Selection(providerName)
}
