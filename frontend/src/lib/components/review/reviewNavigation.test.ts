import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

// Every scroll the review diff and the CI log view make is one of three
// kinds: the reader's navigation (a jump to a file, row, comment or
// section), which goes through one entry that reports it
// (readerInput.ts#reportReaderNavigation) so find ends a pending reveal;
// find's own reveal, which does not report; or keeping the reader's place
// (an anchor restored, the log's opening at its end). This reads each
// component's script and holds every scroll write to a function of one of
// those kinds, so a jump added later cannot scroll without reporting.

interface Surface {
  file: string;
  /** The functions that report navigation. */
  reporters: string[];
  /** Find's own entries: they scroll and never report. */
  findOwn: string[];
  /** Keeping the reader's place. */
  keepsPlace: string[];
  /** The component's exports. */
  exports: string[];
}

const SURFACES: Surface[] = [
  {
    file: 'ReviewDiffBody.svelte',
    reporters: ['navigateToIndex'],
    findOwn: ['revealFindPlace', 'scrollFindBy'],
    keepsPlace: ['restoreAnchor', 'restoreTop'],
    exports: ['findReadingPosition', 'revealFindPlace', 'scrollFindBy'],
  },
  {
    file: 'ReviewCILogView.svelte',
    reporters: ['takeOver'],
    findOwn: ['openFindSection', 'revealFindLine', 'scrollFindBy'],
    keepsPlace: ['placeAtEnd'],
    exports: ['findReadingLine', 'findLineText', 'findLineLoad', 'openFindSection', 'revealFindLine', 'scrollFindBy'],
  },
];

const WRITE = /\b(?:scrollToIndex|applyScrollTarget|preserveScrollAnchor|requestBottom|scrollTo|scrollBy)\(|\.(?:scrollTop|scrollLeft)\s*(?:[+-]?=)(?!=)/;

interface Fn { name: string; exported: boolean; body: string }
interface Write { line: string; within: string | null }

// The instance script's named functions, and each scroll write with the
// innermost named function it is in.
function readScript(source: string): { fns: Fn[]; writes: Write[] } {
  const open = source.indexOf('<script lang="ts">');
  const lines = source.slice(open, source.indexOf('</script>', open))
    .split('\n')
    .map((line) => (/^\s*(?:\/\/|\/\*|\*)/.test(line) ? '' : line.replace(/\s\/\/.*$/, '')));
  const fns: Fn[] = [];
  const writes: Write[] = [];
  const stack: ({ name: string; exported: boolean; body: string[] } | null)[] = [];
  let pending: { name: string; exported: boolean; parens: number } | null = null;
  for (const line of lines) {
    const named = /^\s*(export\s+)?(?:async\s+)?function\s+(\w+)/.exec(line);
    if (named) pending = { name: named[2], exported: named[1] !== undefined, parens: 0 };
    const write = WRITE.exec(line)?.index ?? -1;
    for (const open of stack) open?.body.push(line);
    for (let j = 0; j < line.length; j += 1) {
      if (j === write) writes.push({ line: line.trim(), within: [...stack].reverse().find((each) => each !== null)?.name ?? null });
      const c = line[j];
      if (pending) {
        if (c === '(') pending.parens += 1;
        else if (c === ')') pending.parens -= 1;
        else if (c === '{' && pending.parens === 0) {
          stack.push({ name: pending.name, exported: pending.exported, body: [line.slice(j)] });
          pending = null;
          continue;
        }
      }
      if (c === '{') stack.push(null);
      else if (c === '}') {
        const closed = stack.pop();
        if (closed) fns.push({ name: closed.name, exported: closed.exported, body: closed.body.join('\n') });
      }
    }
  }
  return { fns, writes };
}

describe('review surface scrolls', () => {
  it('the diff\'s keys (j/k, n/p) scroll only through the body\'s navigation', () => {
    const source = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), 'reviewDiffKeys.ts'), 'utf8');
    const lines = source.split('\n').filter((line) => !/^\s*(?:\/\/|\/\*|\*)/.test(line));
    expect(lines.filter((line) => WRITE.test(line))).toEqual([]);
    expect(lines.filter((line) => line.includes('host.navigateToIndex(')).length).toBe(2);
  });

  it.each(SURFACES.map((surface) => [surface.file, surface] as const))('%s: every scroll write reports the reader\'s navigation, is find\'s own, or keeps the reader\'s place', (_file, surface) => {
    const { fns, writes } = readScript(readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), surface.file), 'utf8'));
    const byName = new Map(fns.map((fn) => [fn.name, fn]));
    const reports = (fn: Fn) => fn.body.includes('reportReaderNavigation(');
    // The entries that report are the listed ones, and they do report.
    expect(fns.filter(reports).map((fn) => fn.name).sort()).toEqual([...surface.reporters].sort());
    const callsReporter = (fn: Fn) => surface.reporters.some((name) => new RegExp(`\\b${name}\\(`).test(fn.body));
    // Find's own never reports.
    for (const name of surface.findOwn) {
      const fn = byName.get(name);
      expect(fn, name).toBeDefined();
      expect(reports(fn!) || callsReporter(fn!), `${name} reports navigation`).toBe(false);
    }
    // Every export is classified: a new exported jump must be listed.
    expect(fns.filter((fn) => fn.exported).map((fn) => fn.name).sort()).toEqual([...surface.exports].sort());
    expect(writes.length).toBeGreaterThan(0);
    const unclassified = writes.filter(({ within }) => {
      if (within === null) return true;
      if (surface.findOwn.includes(within) || surface.keepsPlace.includes(within) || surface.reporters.includes(within)) return false;
      const fn = byName.get(within);
      return !fn || !callsReporter(fn);
    });
    expect(unclassified).toEqual([]);
  });
});
