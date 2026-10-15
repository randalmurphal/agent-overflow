// The account row's login-expiry line. Claude refresh tokens die on a fixed
// schedule that refreshing does not extend, so the thresholds here are the only
// warning a user gets before the account stops working. They are asserted at
// their exact boundaries: one day of drift either way is the difference between
// a useful heads-up and a card that lies.

import { describe, expect, it } from 'vitest';
import { providerAccountLoginExpiry } from './providerAccountLabels';
import { account } from '../../test/helpers/providerAccounts';

const DAY = 24 * 60 * 60 * 1000;
const NOW = Date.UTC(2026, 0, 15, 12, 0, 0);

function expiry(refreshTokenExpiresAt: number) {
  return providerAccountLoginExpiry(account({ refreshTokenExpiresAt }), NOW);
}

describe('providerAccountLoginExpiry', () => {
  it('says nothing for an account with no recorded deadline', () => {
    // Every Codex account, and Claude logins saved before the field existed.
    expect(providerAccountLoginExpiry(account(), NOW)).toBeNull();
    expect(expiry(0)).toBeNull();
  });

  it('stays quiet while the login has more than a week left', () => {
    expect(expiry(NOW + 30 * DAY)).toBeNull();
    expect(expiry(NOW + 8 * DAY)).toBeNull();
    // Just past the notice window: still quiet, one minute before it opens.
    expect(expiry(NOW + 7 * DAY + 60_000)).toBeNull();
  });

  it('counts the last week down without raising the alarm', () => {
    expect(expiry(NOW + 7 * DAY)).toEqual({
      text: 'Login expires in 7d',
      urgent: false,
      expired: false,
    });
    expect(expiry(NOW + 4 * DAY)).toEqual({
      text: 'Login expires in 4d',
      urgent: false,
      expired: false,
    });
  });

  it('marks the last three days urgent', () => {
    expect(expiry(NOW + 3 * DAY)).toEqual({
      text: 'Login expires in 3d',
      urgent: true,
      expired: false,
    });
    expect(expiry(NOW + 2 * DAY)).toEqual({
      text: 'Login expires in 2d',
      urgent: true,
      expired: false,
    });
  });

  it('states the exact remaining time', () => {
    // 29 hours is "1d 5h", not a rounded-up "2 days".
    expect(expiry(NOW + 29 * 60 * 60 * 1000)).toEqual({
      text: 'Login expires in 1d 5h',
      urgent: true,
      expired: false,
    });
    expect(expiry(NOW + 90 * 60 * 1000)).toEqual({
      text: 'Login expires in 1h 30m',
      urgent: true,
      expired: false,
    });
    expect(expiry(NOW + 30_000)).toEqual({
      text: 'Login expires in <1m',
      urgent: true,
      expired: false,
    });
  });

  it('reports an elapsed deadline as expired, boundary included', () => {
    // At the instant itself the next refresh already answers invalid_grant.
    expect(expiry(NOW)).toEqual({ text: 'Login expired', urgent: true, expired: true });
    expect(expiry(NOW - 1)).toEqual({ text: 'Login expired', urgent: true, expired: true });
    expect(expiry(NOW - 30 * DAY)).toEqual({
      text: 'Login expired',
      urgent: true,
      expired: true,
    });
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
