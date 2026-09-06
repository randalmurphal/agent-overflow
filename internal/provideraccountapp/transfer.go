package provideraccountapp

import (
	"context"
	"errors"

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
	_, err := m.RunAccountProbe(ProbeRequest{ProviderName: string(provider.Claude), Validate: checkClaudeTransferCredential, Probe: func(life context.Context) (provider.AccountInfo, error) {
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
	if credential != nil && !CredentialSignedOut(string(provider.Claude), credential.Data) {
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
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return codex.CheckTransferAccount(ctx, m.CodexProbeConfig(binary, nil))
}
