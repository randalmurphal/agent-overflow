package provideraccountapp

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/providerstatus"
)

// CheckClaudeTransferAccount uses the same canonical credential/rotation lock
// as identity discovery. Never probe a copied OAuth home or trust an email alone
// when the native response says it has no login.
func (m *Manager) CheckClaudeTransferAccount(ctx context.Context, binary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := m.RunAccountProbe(ProbeRequest{ProviderName: string(provider.Claude), Context: ctx, Validate: checkClaudeTransferCredential, Probe: func(life context.Context) (provider.AccountInfo, error) {
		probeCtx, cancel := context.WithCancel(life)
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		return claude.ProbeAccount(probeCtx, m.ClaudeProbeConfig(binary, nil))
	}})
	return err
}

func checkClaudeTransferCredential(info provider.AccountInfo, credential *provideraccounts.CredentialSnapshot) error {
	if !providerstatus.ClaudeUnauthenticated(info) {
		return nil
	}
	if credential != nil && !CredentialSignedOut(string(provider.Claude), credential.Bytes()) {
		return nil
	}
	return errors.New("Sign in to Claude on this computer before receiving the conversation.")
}

// Codex exposes its auth requirement directly. Keep its response-only probe
// separate from Claude's credential-sensitive initialization transaction.
func (m *Manager) CheckCodexTransferAccount(ctx context.Context, binary string) error {
	endWork, err := m.beginWork(ctx)
	if err != nil {
		return err
	}
	defer endWork()
	lock := m.reconcileMutex(string(provider.Codex))
	if err := lock.LockContext(ctx); err != nil {
		return fmt.Errorf("wait for the codex account lock to check the transfer's Codex account: %w", err)
	}
	defer lock.Unlock()
	return codex.CheckTransferAccount(ctx, m.CodexProbeConfig(binary, nil))
}

// CheckAgyTransferAccount verifies that Antigravity on this computer is
// authenticated before accepting an incoming Antigravity conversation transfer.
func (m *Manager) CheckAgyTransferAccount(ctx context.Context, binary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	endWork, err := m.beginWork(ctx)
	if err != nil {
		return err
	}
	defer endWork()
	lock := m.reconcileMutex(string(provider.Agy))
	if err := lock.LockContext(ctx); err != nil {
		return fmt.Errorf("wait for the agy account lock to check the transfer's Antigravity account: %w", err)
	}
	defer lock.Unlock()
	_, err = m.probeCanonicalAgy(ctx, binary, m.providerCustomEnv(string(provider.Agy)))
	if err != nil {
		return fmt.Errorf("sign in to Antigravity on this computer before receiving the conversation: %w", err)
	}
	return nil
}
