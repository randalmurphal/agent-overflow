import { describe, expect, it } from 'vitest';
import { commandSegments, commandWordRanges } from './commandWords';

describe('commandWordRanges', () => {
  // Shape-for-shape twin of TestCommandWords in
  // `internal/usermessage/command_test.go`.
  const cases: Array<[string, string[]]> = [
    ['/workflow', ['workflow']],
    ['/workflow start the release run', ['workflow']],
    ['/workflow\nstart it', ['workflow']],
    ['/workflow\tstart it', ['workflow']],
    ['/run-2 go', ['run-2']],
    ['please run /workflow now', ['workflow']],
    [' /workflow', ['workflow']],
    ['do this:\n/workflow', ['workflow']],
    ['and then /workflow', ['workflow']],
    ['/workflow then /workflow', ['workflow', 'workflow']],
    ['check /tmp then /workflow', ['tmp', 'workflow']],
    ['workflow start', []],
    ['/', []],
    ['/ workflow', []],
    ['/tmp/scratch is where it went', []],
    ['see /tmp/scratch/workflow for it', []],
    ['/Workflow', []],
    ['/2fast', []],
    ['/-x', []],
    ['run /workflow, then stop', []],
    ['', []],
    ['/workflows are nice', ['workflows']],
  ];
  for (const [value, want] of cases) {
    it(`${JSON.stringify(value)} → [${want.join(', ')}]`, () => {
      expect(commandWordRanges(value).map((range) => range.name)).toEqual(want);
    });
  }

  it('reports the exact slice each word occupies', () => {
    const value = 'run /a then /b-2';
    for (const range of commandWordRanges(value)) {
      expect(value.slice(range.start, range.end)).toBe(`/${range.name}`);
    }
  });

  it('treats a non-breaking space as a word separator, as Go does', () => {
    expect(commandWordRanges('run\u00a0/workflow').map((r) => r.name)).toEqual(['workflow']);
  });

  it('does not treat a zero-width no-break space as one, as Go does not', () => {
    // Go's unicode.IsSpace is the Unicode White_Space property, which excludes
    // U+FEFF even though JavaScript's `\s` includes it. The word runs through.
    expect(commandWordRanges('/workflow\ufeffx')).toEqual([]);
  });
});

describe('commandSegments', () => {
  it('alternates plain and command runs and drops empty plain parts', () => {
    const value = '/workflow now and /workflow again';
    expect(commandSegments(value, commandWordRanges(value))).toEqual([
      { text: '/workflow', command: true },
      { text: ' now and ', command: false },
      { text: '/workflow', command: true },
      { text: ' again', command: false },
    ]);
  });

  it('reconstructs the value exactly, whatever the ranges', () => {
    const value = 'ask /workflow then /workflow';
    const joined = commandSegments(value, commandWordRanges(value))
      .map((segment) => segment.text)
      .join('');
    expect(joined).toBe(value);
  });

  it('returns the whole value as one plain segment when nothing matched', () => {
    expect(commandSegments('plain text', [])).toEqual([{ text: 'plain text', command: false }]);
  });
});
