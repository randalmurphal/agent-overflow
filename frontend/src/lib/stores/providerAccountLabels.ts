import type { ProviderID } from '../types/providers';
import type { ManagedProviderAccount } from './bindings';
import { getProviderDefinition } from '../providers/catalog';


/** Display name for a provider, as every account surface spells it. */
export function providerLabel(provider: ProviderID): string {
  return getProviderDefinition(provider).label;
}


/** The name a card, row, or confirm dialog shows for an account. */
export function providerAccountName(account: ManagedProviderAccount): string {
  return account.displayName || account.email || account.subscriptionType || 'Saved account';
}


/**
 * The organization/workspace an account belongs to, for the subline that
 * tells two same-email accounts apart. Claude accounts carry a display name;
 * Codex workspaces are known only by an opaque id, shown truncated. Blank
 * means the organization is unknown (legacy account, API-key auth) and the
 * subline is simply omitted.
 */
export function providerAccountOrgLabel(account: ManagedProviderAccount): string {
  if (account.orgName) return account.orgName;
  const orgId = account.orgId ?? '';
  if (!orgId) return '';
  return orgId.length > 14 ? `${orgId.slice(0, 12)}…` : orgId;
}


/**
 * The accessible name for the control that selects an account. Both surfaces
 * (settings card, picker row) render the same three states, so they name them
 * with the same words.
 */
export function providerAccountActionLabel(account: ManagedProviderAccount): string {
  const name = providerAccountName(account);
  if (account.needsLogin) return `Sign in again to ${name}`;
  if (account.active) return `${name} is active`;
  return `Switch to ${name}`;
}
