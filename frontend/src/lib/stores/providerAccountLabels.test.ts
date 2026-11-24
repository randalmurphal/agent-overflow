// The account row's login-expiry line. Claude refresh tokens die on a fixed
// schedule that refreshing does not extend, so the thresholds here are the only
// warning a user gets before the account stops working. They are asserted at
// their exact boundaries: one day of drift either way is the difference between
// a useful heads-up and a card that lies.

import { describe, expect, it } from 'vitest';
import {
  providerAccountLoginExpiry,
  providerAccountOrgLabel,
  type ProviderAccountLoginExpiry,
} from './providerAccountLabels';
import type { ManagedProviderAccount } from './bindings';
import { account } from '../../test/helpers/providerAccounts';

describe('providerAccountOrgLabel', () => {
  const workspaceId = '5f0c2a9e-1b7d-4c1e-9a33-0d2b7e6f4a10';

  it.each<[string, Partial<ManagedProviderAccount>, string]>([
    ['shows a reported Codex workspace name instead of its id', { orgId: workspaceId, orgName: 'Acme Team' }, 'Acme Team'],
    ['falls back to the truncated id once the name is blank', { orgId: workspaceId, orgName: '' }, '5f0c2a9e-1b7…'],
    ['falls back to the truncated id once the name is no longer reported', { orgId: workspaceId }, '5f0c2a9e-1b7…'],
    ['says nothing when neither name nor id is known', {}, ''],
  ])('%s', (_name, fields, label) => {
    expect(providerAccountOrgLabel(account({ provider: 'codex', ...fields }))).toBe(label);
  });
});

const DAY = 24 * 60 * 60 * 1000;
const NOW = Date.UTC(2026, 0, 15, 12, 0, 0);

describe('providerAccountLoginExpiry', () => {
  const expired = { text: 'Login expired', urgent: true, expired: true };

  it.each<[string, number | undefined, ProviderAccountLoginExpiry | null]>([
    // Every Codex account, and Claude logins saved before the field existed.
    ['says nothing for an account with no recorded deadline', undefined, null],
    ['says nothing for a zero deadline', 0, null],
    ['stays quiet with 30 days left', NOW + 30 * DAY, null],
    ['stays quiet with 8 days left', NOW + 8 * DAY, null],
    ['stays quiet one minute before the notice window opens', NOW + 7 * DAY + 60_000, null],
    ['counts the last week down from 7d without the alarm', NOW + 7 * DAY, { text: 'Login expires in 7d', urgent: false, expired: false }],
    ['counts 4d down without the alarm', NOW + 4 * DAY, { text: 'Login expires in 4d', urgent: false, expired: false }],
    ['marks 3d left urgent', NOW + 3 * DAY, { text: 'Login expires in 3d', urgent: true, expired: false }],
    ['marks 2d left urgent', NOW + 2 * DAY, { text: 'Login expires in 2d', urgent: true, expired: false }],
    // 29 hours is "1d 5h", not a rounded-up "2 days".
    ['states 29 hours exactly', NOW + 29 * 60 * 60 * 1000, { text: 'Login expires in 1d 5h', urgent: true, expired: false }],
    ['states 90 minutes exactly', NOW + 90 * 60 * 1000, { text: 'Login expires in 1h 30m', urgent: true, expired: false }],
    ['states under a minute', NOW + 30_000, { text: 'Login expires in <1m', urgent: true, expired: false }],
    // At the instant itself the next refresh already answers invalid_grant.
    ['reports the deadline instant as expired', NOW, expired],
    ['reports one millisecond past the deadline as expired', NOW - 1, expired],
    ['reports a month-old deadline as expired', NOW - 30 * DAY, expired],
  ])('%s', (_name, refreshTokenExpiresAt, result) => {
    expect(providerAccountLoginExpiry(account({ refreshTokenExpiresAt }), NOW)).toEqual(result);
  });

  it('defaults to the current clock', () => {
    expect(providerAccountLoginExpiry(account({ refreshTokenExpiresAt: Date.now() - DAY }))).toEqual(
      { text: 'Login expired', urgent: true, expired: true },
    );
    // The clock moves between the two Date.now() reads; the slack keeps the
    // span from landing a second short of two days.
    expect(
      providerAccountLoginExpiry(account({ refreshTokenExpiresAt: Date.now() + 2 * DAY + 30_000 })),
    ).toEqual({ text: 'Login expires in 2d', urgent: true, expired: false });
  });
});
