import { describe, expect, it } from 'vitest';
import { formatTimeSpent, parseTimeSpent } from './timeSpent';

describe('parseTimeSpent', () => {
  it.each([
    ['1h 30m', 5400],
    ['1h30m', 5400],
    ['1h', 3600],
    ['45m', 2700],
    ['1.5h', 5400],
    ['1.1h', 3960],
    ['4.15h', 14940],
    ['0.25h', 900],
    ['90', 5400],
    ['  2H 5M ', 7500],
    ['0h 30m', 1800],
  ])('reads %j as %i seconds', (text, seconds) => {
    expect(parseTimeSpent(text)).toBe(seconds);
  });

  it.each([
    '',
    '   ',
    'h',
    'm',
    '0',
    '0m',
    '0h 0m',
    '1.01h',
    '1.5m',
    '1h30',
    '30m 1h',
    '-1h',
    '1d',
    '1 hour',
    '1,5h',
    '1h 30m 10s',
    '99999999999999999h',
  ])('refuses %j', (text) => {
    expect(parseTimeSpent(text)).toBeNull();
  });
});

describe('formatTimeSpent', () => {
  it.each([
    [5400, '1h 30m'],
    [7200, '2h'],
    [2700, '45m'],
    [90, '1m 30s'],
    [3630, '1h 30s'],
    [0, '0m'],
  ])('shows %i seconds as %j', (seconds, text) => {
    expect(formatTimeSpent(seconds)).toBe(text);
  });

  it('reads back what it shows for whole minutes', () => {
    for (const seconds of [60, 2700, 3600, 5400, 30_000]) {
      expect(parseTimeSpent(formatTimeSpent(seconds))).toBe(seconds);
    }
  });
});
