import { RecheckClaudeAccount, RecheckCodexAccount } from '../stores/bindings';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import type { ProviderID } from '../types/providers';

export interface ProviderAccountInfo {
  subscriptionType?: string;
  tokenSource?: string;
  apiProvider?: string;
}

export async function recheckProviderAccount(
  provider: ProviderID,
  backend: BackendKey = HOME_BACKEND,
): Promise<ProviderAccountInfo> {
  switch (provider) {
    // claude-tui reuses claude's binary and account, so it rechecks the same
    // Claude account.
    case 'claude':
    case 'claude-tui':
      return await withBackendTarget(backend, () => RecheckClaudeAccount()) as ProviderAccountInfo;
    case 'codex':
      return await withBackendTarget(backend, () => RecheckCodexAccount()) as ProviderAccountInfo;
  }
}

export function recheckResultClearsAuthBanner(
  provider: ProviderID,
  account: ProviderAccountInfo,
): boolean {
  if (provider === 'claude' || provider === 'claude-tui') {
    return Boolean(account.subscriptionType || account.tokenSource);
  }
  return true;
}
