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


const DAY_MS = 24 * 60 * 60 * 1000;
/**
 * A login is worth mentioning a week out, and urgent inside three days — the
 * same shape the Claude CLI uses for its own `/login to renew` line.
 */
const EXPIRY_NOTICE_DAYS = 7;
const EXPIRY_URGENT_DAYS = 3;

export interface ProviderAccountLoginExpiry {
  text: string;
  /** Styled as a warning: the login has days left, or none. */
  urgent: boolean;
  /** The session is already over; only a sign-in reconnects the account. */
  expired: boolean;
}

/**
 * The account row's line about the end of its OAuth session.
 *
 * Claude issues a refresh token with a fixed life (~30 days) that refreshing
 * does NOT extend, so every login dies on a schedule its owner cannot see from
 * inside the app. `refreshTokenExpiresAt` is that date. Past it the account is
 * already signed out in every way that matters — the next refresh answers
 * invalid_grant — so the row says so plainly.
 *
 * Deliberately QUIET outside the last week: an account with three weeks left
 * needs nothing from the user, and a permanent countdown on every card is
 * noise that teaches people to ignore the one week it matters. Accounts with
 * no recorded deadline (every Codex account, and Claude logins saved by a CLI
 * that did not record one) say nothing at all rather than guessing.
 */
export function providerAccountLoginExpiry(
  account: ManagedProviderAccount,
  now: number = Date.now(),
): ProviderAccountLoginExpiry | null {
  const expiresAt = account.refreshTokenExpiresAt ?? 0;
  if (expiresAt <= 0) return null;
  if (expiresAt <= now) return { text: 'Login expired', urgent: true, expired: true };
  // Rounded UP, so the last partial day still reads "in 1 day" rather than
  // "in 0 days" — the value is a deadline, not an elapsed count.
  const days = Math.ceil((expiresAt - now) / DAY_MS);
  if (days > EXPIRY_NOTICE_DAYS) return null;
  return {
    text: `Login expires in ${days} ${days === 1 ? 'day' : 'days'}`,
    urgent: days <= EXPIRY_URGENT_DAYS,
    expired: false,
  };
}
