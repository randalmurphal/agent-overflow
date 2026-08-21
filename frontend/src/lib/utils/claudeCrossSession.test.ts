import { describe, expect, it } from 'vitest';
import {
  CLAUDE_CROSS_SESSION_INBOUND_OPTIONS,
  crossSessionPatch,
  effectiveInbound,
} from './claudeCrossSession';

describe('cross-session inbound options', () => {
  // "hold" is a legal Claude Code value and an illegal Agent Overflow one:
  // it waits for an approval a session driven headlessly cannot present,
  // and the backend refuses it at the save. Offering it would let the user
  // configure a silent black hole.
  it('never offers "hold"', () => {
    expect(CLAUDE_CROSS_SESSION_INBOUND_OPTIONS.map((o) => o.value)).toEqual(['accept', 'refuse']);
  });

  // There is no empty option either: this list only renders once the
  // feature is on, and "on but unset" is exactly the mode-parity state the
  // setting exists to avoid.
  it('offers no "let Claude Code decide" entry', () => {
    for (const option of CLAUDE_CROSS_SESSION_INBOUND_OPTIONS) {
      expect(option.value).not.toBe('');
      expect(option.description.length).toBeGreaterThan(0);
    }
  });
});

describe('effectiveInbound', () => {
  it('resolves an enabled-but-unset session to accept', () => {
    expect(effectiveInbound({ enabled: true })).toBe('accept');
    expect(effectiveInbound({ enabled: true, inbound: '' })).toBe('accept');
  });

  it('says nothing at all while disabled', () => {
    expect(effectiveInbound({})).toBe('');
    expect(effectiveInbound({ inbound: 'accept' })).toBe('');
  });
});

describe('crossSessionPatch', () => {
  it('writes both halves together so one cannot drop the other', () => {
    expect(crossSessionPatch(true, 'refuse')).toEqual({ enabled: true, inbound: 'refuse' });
  });

  it('never enables without an explicit policy', () => {
    expect(crossSessionPatch(true, '')).toEqual({ enabled: true, inbound: 'accept' });
  });

  // Turning the feature off must not silently discard the policy the user
  // picked — turning it back on should find it again.
  it('keeps the stored policy while disabled', () => {
    expect(crossSessionPatch(false, 'refuse')).toEqual({ inbound: 'refuse' });
    expect(crossSessionPatch(false, '')).toEqual({});
  });
});
