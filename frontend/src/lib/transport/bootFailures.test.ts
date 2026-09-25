import { describe, expect, it } from 'vitest';
import { bootFailureText, parseBootFailures, sameBootFailures } from './bootFailures';

describe('parseBootFailures', () => {
  it('reads each failure, neutralising a field it cannot read, and drops what is not one', () => {
    expect(parseBootFailures(undefined)).toEqual([]);
    expect(parseBootFailures({ phase: 'x' })).toEqual([]);
    expect(parseBootFailures([
      null,
      'app.recover_crashed_turns',
      { phase: 'app.recover_crashed_turns', detail: 'Settling interrupted turns', error: 'database is locked' },
      { phase: 7, detail: 'Settling worktree setups', error: ['disk'] },
      { phase: 'app.nothing', detail: 42 },
    ])).toEqual([
      { phase: 'app.recover_crashed_turns', detail: 'Settling interrupted turns', error: 'database is locked' },
      { phase: '', detail: 'Settling worktree setups', error: '' },
      { phase: 'app.nothing', detail: '', error: '' },
    ]);
  });

  it('bounds the list and every string', () => {
    const many = Array.from({ length: 40 }, (_, i) => ({ phase: `p${i}`, detail: 'd', error: 'x'.repeat(5_000) }));
    const parsed = parseBootFailures(many);
    expect(parsed).toHaveLength(16);
    expect(parsed[0]!.error.length).toBe(600);
  });
});

describe('bootFailureText', () => {
  it('names each failed phase with its error, then the retry', () => {
    expect(bootFailureText([])).toBe('');
    expect(bootFailureText([
      { phase: 'app.recover_crashed_turns', detail: 'Settling interrupted turns', error: 'database is locked' },
    ])).toBe('Settling interrupted turns failed at startup: database is locked. Retrying on next start.');
    expect(bootFailureText([
      { phase: 'app.recover_crashed_turns', detail: 'Settling interrupted turns', error: 'database is locked.' },
      { phase: 'app.sweep', detail: '', error: '' },
    ])).toBe('Settling interrupted turns failed at startup: database is locked. A startup step failed. Retrying on next start.');
  });
});

describe('sameBootFailures', () => {
  it('compares every field in order', () => {
    const a = { phase: 'p', detail: 'd', error: 'e' };
    expect(sameBootFailures([a], [{ ...a }])).toBe(true);
    expect(sameBootFailures([a], [])).toBe(false);
    expect(sameBootFailures([a], [{ ...a, error: 'f' }])).toBe(false);
    expect(sameBootFailures([a], [{ ...a, phase: 'q' }])).toBe(false);
    expect(sameBootFailures([a], [{ ...a, detail: 'c' }])).toBe(false);
  });
});
