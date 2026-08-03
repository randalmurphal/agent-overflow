import { describe, expect, it } from 'vitest';
import {
  fastModeContradictionText,
  fastModeReasonText,
  isFastModeContradicted,
} from './fastMode';

describe('fastModeReasonText', () => {
  it('maps every reason the CLI is known to emit', () => {
    // The enum from claude-wire.md §fast_mode_state. A miss here means
    // the UI would show a raw wire token instead of a sentence.
    const reasons = [
      'not_first_party',
      'disabled_by_env',
      'unknown',
      'model_not_allowed',
      'sdk_opt_in_required',
      'pending',
      'free',
      'preference',
      'extra_usage_disabled',
      'network_error',
    ];
    for (const reason of reasons) {
      const text = fastModeReasonText(reason);
      expect(text, reason).not.toBe('');
      expect(text, reason).not.toContain('_');
    }
  });

  it('passes an unknown reason through rather than swallowing it', () => {
    // A future CLI enum value must still reach the user: silence would
    // read as "fast mode is fine".
    expect(fastModeReasonText('some_future_reason')).toBe('some future reason');
  });

  it('returns empty for an absent reason', () => {
    expect(fastModeReasonText('')).toBe('');
    expect(fastModeReasonText('   ')).toBe('');
  });
});

describe('isFastModeContradicted', () => {
  it('is false when the thread does not ask for fast mode', () => {
    expect(isFastModeContradicted(false, { state: 'off', disabledReason: 'free' })).toBe(false);
  });

  it('is false when the provider has said nothing', () => {
    // Older CLI / no turn finished / non-Claude provider. Unknown is not
    // disabled.
    expect(isFastModeContradicted(true, undefined)).toBe(false);
  });

  it('is false when the provider reports a reason but no state', () => {
    expect(isFastModeContradicted(true, { state: '', disabledReason: 'unknown' })).toBe(false);
  });

  it('is false when the provider confirms fast mode is on', () => {
    expect(isFastModeContradicted(true, { state: 'on', disabledReason: '' })).toBe(false);
  });

  it('is true for off and for cooldown', () => {
    expect(isFastModeContradicted(true, { state: 'off', disabledReason: 'free' })).toBe(true);
    expect(isFastModeContradicted(true, { state: 'cooldown', disabledReason: '' })).toBe(true);
  });
});

describe('fastModeContradictionText', () => {
  it('names the rate-limit pause for cooldown', () => {
    expect(fastModeContradictionText({ state: 'cooldown', disabledReason: '' })).toBe(
      'Provider paused fast mode after a rate limit',
    );
  });

  it('folds the reason in when one is present', () => {
    expect(
      fastModeContradictionText({ state: 'off', disabledReason: 'sdk_opt_in_required' }),
    ).toBe('Provider reports fast mode off: the session did not opt in');
  });

  it('still explains an off state a pre-2.1.219 CLI gave no reason for', () => {
    expect(fastModeContradictionText({ state: 'off', disabledReason: '' })).toBe(
      'Provider reports fast mode off',
    );
  });

  it('says nothing for an on or unknown state', () => {
    expect(fastModeContradictionText({ state: 'on', disabledReason: '' })).toBe('');
    expect(fastModeContradictionText(undefined)).toBe('');
  });
});
