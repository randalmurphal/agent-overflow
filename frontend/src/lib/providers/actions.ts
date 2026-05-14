import { RecheckClaudeAccount, RecheckCodexAccount } from '../stores/bindings';
import type { ProviderID } from '../types/providers';

export interface ProviderAccountInfo {
  subscriptionType?: string;
  tokenSource?: string;
  apiProvider?: string;
}

export async function recheckProviderAccount(
  provider: ProviderID,
): Promise<ProviderAccountInfo> {
  switch (provider) {
    case 'claude':
      return await RecheckClaudeAccount() as ProviderAccountInfo;
    case 'codex':
      return await RecheckCodexAccount() as ProviderAccountInfo;
  }
}

export function recheckResultClearsAuthBanner(
  provider: ProviderID,
  account: ProviderAccountInfo,
): boolean {
  if (provider === 'claude') {
    return Boolean(account.subscriptionType || account.tokenSource);
  }
  return true;
}
