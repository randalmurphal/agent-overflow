import { describe, expect, it } from 'vitest';
import {
  SLASH_COMMANDS,
  detectSlashTrigger,
  leadingSlashCommand,
  matchSlashCommands,
  slashCommandWord,
} from './slashCommands';

describe('slash command registry', () => {
  it('registers /workflow with a description that says what the agent gains', () => {
    expect(SLASH_COMMANDS.map((command) => command.name)).toEqual(['workflow']);
    expect(SLASH_COMMANDS[0].description).toMatch(/agent-overflow/);
    expect(slashCommandWord(SLASH_COMMANDS[0])).toBe('/workflow');
  });

  it('filters by prefix, in registry order', () => {
    expect(matchSlashCommands('').map((c) => c.name)).toEqual(['workflow']);
    expect(matchSlashCommands('work').map((c) => c.name)).toEqual(['workflow']);
    expect(matchSlashCommands('workflow').map((c) => c.name)).toEqual(['workflow']);
    expect(matchSlashCommands('workflows')).toEqual([]);
    expect(matchSlashCommands('deploy')).toEqual([]);
    // Names are lowercase on both sides of the wire; a capitalised word is
    // not a command, because the backend would not expand it either.
    expect(matchSlashCommands('Work')).toEqual([]);
  });
});

describe('leadingSlashCommand', () => {
  const cases: Array<[string, string | null]> = [
    ['/workflow', 'workflow'],
    ['/workflow start the release', 'workflow'],
    ['/workflow\nstart the release', 'workflow'],
    ['/workflow\tstart', 'workflow'],
    ['/workflows are nice', null],
    ['/Workflow', null],
    ['/', null],
    ['/ workflow', null],
    ['/tmp/scratch has the log', null],
    [' /workflow', null],
    ['ask about /workflow later', null],
    ['workflow', null],
    ['', null],
  ];
  for (const [value, want] of cases) {
    it(`${JSON.stringify(value)} → ${want ?? 'no command'}`, () => {
      expect(leadingSlashCommand(value)?.name ?? null).toBe(want);
    });
  }
});

describe('detectSlashTrigger', () => {
  it('opens on a leading slash and filters as the user types', () => {
    expect(detectSlashTrigger('/', 1)).toMatchObject({ query: '', start: 0, end: 1 });
    expect(detectSlashTrigger('/w', 2)).toMatchObject({ query: 'w', start: 0, end: 2 });
    expect(detectSlashTrigger('/workflow', 9)?.results.map((c) => c.name)).toEqual(['workflow']);
  });

  it('closes once nothing matches, so typing past a name just leaves text', () => {
    expect(detectSlashTrigger('/workflowish', 12)).toBeNull();
    expect(detectSlashTrigger('/deploy', 7)).toBeNull();
  });

  it('never triggers away from the start of the draft', () => {
    expect(detectSlashTrigger('hello /w', 8)).toBeNull();
    expect(detectSlashTrigger(' /w', 3)).toBeNull();
    expect(detectSlashTrigger('/workflow now /w', 16)).toBeNull();
  });

  it('closes once the caret leaves the first word', () => {
    // Caret after the space: the word is settled, the menu is done.
    expect(detectSlashTrigger('/workflow ', 10)).toBeNull();
    expect(detectSlashTrigger('/workflow do it', 15)).toBeNull();
    // Caret still inside the word filters on the prefix before it, matching
    // the @-mention rule of reading up to the caret.
    expect(detectSlashTrigger('/workflow do it', 5)).toMatchObject({ query: 'work', end: 5 });
  });

  it('refuses carets outside the value and a caret on the slash itself', () => {
    expect(detectSlashTrigger('/w', 0)).toBeNull();
    expect(detectSlashTrigger('/w', 3)).toBeNull();
    expect(detectSlashTrigger('', 0)).toBeNull();
  });
});
