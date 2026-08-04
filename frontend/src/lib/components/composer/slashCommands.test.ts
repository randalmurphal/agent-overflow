import { describe, expect, it } from 'vitest';
import {
  SLASH_COMMANDS,
  slashCommandMatches,
  slashCommandWord,
} from './slashCommands';

describe('slash command registry', () => {
  it('registers /workflow with a description that says what the agent gains', () => {
    expect(SLASH_COMMANDS.map((command) => command.name)).toEqual(['workflow']);
    expect(SLASH_COMMANDS[0].description).toMatch(/agent-overflow/);
    expect(slashCommandWord(SLASH_COMMANDS[0])).toBe('/workflow');
  });

});

describe('slashCommandMatches', () => {
  // Mirrors, case for case, TestCommandWordsPicksTheFirstRegistered in
  // `internal/usermessage/command_test.go`. The matchers are parallel by hand,
  // so these tables are the parity check — a rule changed on one side and not
  // the other shows up as a failing twin.
  const cases: Array<[string, string | null]> = [
    ['/workflow', 'workflow'],
    ['/workflow start the release', 'workflow'],
    ['/workflow\nstart the release', 'workflow'],
    ['/workflow\tstart', 'workflow'],
    ['ask about /workflow later', 'workflow'],
    [' /workflow', 'workflow'],
    ['line one\n/workflow', 'workflow'],
    ['first /tmp then /workflow', 'workflow'],
    ['/workflow and again /workflow', 'workflow'],
    ['/workflows are nice', null],
    ['/Workflow', null],
    ['/', null],
    ['/ workflow', null],
    ['/tmp/scratch has the log', null],
    ['see /tmp/scratch/workflow', null],
    ['a/workflow is not a word start', null],
    ['workflow', null],
    ['', null],
  ];
  for (const [value, want] of cases) {
    it(`${JSON.stringify(value)} → ${want ?? 'no command'}`, () => {
      expect(slashCommandMatches(value)[0]?.command.name ?? null).toBe(want);
    });
  }

  it('returns every occurrence, in order, with the range each one occupies', () => {
    const value = '/workflow now and /workflow again';
    expect(slashCommandMatches(value).map((match) => [match.start, match.end])).toEqual([
      [0, 9],
      [18, 27],
    ]);
    expect(value.slice(18, 27)).toBe('/workflow');
  });

  it('skips an unregistered word rather than stopping at it', () => {
    expect(slashCommandMatches('check /tmp then /workflow').map((m) => m.start)).toEqual([16]);
  });
});
