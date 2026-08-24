import { describe, expect, it } from 'vitest';
import {
  interceptedCommandRange,
  leadingCommandArgument,
  leadingCommandName,
  parseInterceptedCommand,
} from './composerCommandParse';

describe('leadingCommandName', () => {
  // Mirrors, case for case, TestStartsWithCommandShapedWord's inputs in
  // internal/provider/claude/slash_guard.go. The two parsers are parallel by
  // hand, so this table is the parity check: a message AO calls a command and
  // the CLI does not (or the reverse) is the bug this catches.
  const cases: Array<[string, string | null]> = [
    ['/usage', 'usage'],
    ['/usage now', 'usage'],
    ['/usage\nnow', 'usage'],
    ['/mcp__linear__create_issue', 'mcp__linear__create_issue'],
    ['/plugin:command', 'plugin:command'],
    ['/re-run', 're-run'],
    ['/Usage', 'Usage'],
    // No leading trim: the CLI tests the raw string, so a space in front
    // already makes it prose.
    [' /usage', null],
    ['ask about /usage', null],
    ['/etc/hosts is world-readable', null],
    ['/usr/bin/env', null],
    ['/usage,', null],
    ['/', null],
    ['/ usage', null],
    ['usage', null],
    ['', null],
  ];
  for (const [value, want] of cases) {
    it(`${JSON.stringify(value)} → ${want ?? 'not a command'}`, () => {
      expect(leadingCommandName(value)).toBe(want);
    });
  }
});

describe('leadingCommandArgument', () => {
  it('returns the trimmed remainder, empty for a bare call', () => {
    expect(leadingCommandArgument('/model')).toBe('');
    expect(leadingCommandArgument('/model   ')).toBe('');
    expect(leadingCommandArgument('/model  opus 5 ')).toBe('opus 5');
    expect(leadingCommandArgument('/rename\nShip the parser')).toBe('Ship the parser');
    expect(leadingCommandArgument('not a command')).toBe('');
  });
});

describe('parseInterceptedCommand', () => {
  const intercepted = new Set(['model', 'clear', 'review']);

  it('matches a registered name at position 0 and splits the argument', () => {
    expect(parseInterceptedCommand('/model', intercepted)).toEqual({ name: 'model', arg: '' });
    expect(parseInterceptedCommand('/model sonnet', intercepted)).toEqual({
      name: 'model',
      arg: 'sonnet',
    });
    expect(parseInterceptedCommand('/review branch main', intercepted)).toEqual({
      name: 'review',
      arg: 'branch main',
    });
  });

  it('leaves an unregistered or mid-sentence command alone', () => {
    expect(parseInterceptedCommand('/usage', intercepted)).toBeNull();
    // Mid-sentence is prose and WILL be sent — interception is position-0 only.
    expect(parseInterceptedCommand('run /clear afterwards', intercepted)).toBeNull();
    expect(parseInterceptedCommand('/models', intercepted)).toBeNull();
  });
});

describe('interceptedCommandRange', () => {
  const intercepted = new Set(['model', 'clear']);

  it('covers the leading word only', () => {
    expect(interceptedCommandRange('/model sonnet', intercepted)).toEqual({
      name: 'model',
      start: 0,
      end: 6,
    });
    expect('/model sonnet'.slice(0, 6)).toBe('/model');
  });

  it('paints nothing mid-sentence, where interception does not fire', () => {
    expect(interceptedCommandRange('then /clear it', intercepted)).toBeNull();
    expect(interceptedCommandRange('/usage', intercepted)).toBeNull();
  });
});
