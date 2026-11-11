// The terminal find provider over a scripted search addon: what it asks the
// addon, and how the addon's answers become the bar's result. The addon's
// own matching, stepping and painting run in terminalFind.browser.test.ts
// against a real xterm.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { IDisposable } from '@xterm/xterm';
import type { ISearchOptions, ISearchResultChangeEvent } from '@xterm/addon-search';
import {
  TERMINAL_FIND_HIGHLIGHT_LIMIT,
  createTerminalFind,
  registerTerminalFind,
  terminalFindKey,
  type TerminalFindSearch,
  type TerminalFindTerminal,
} from './terminalFind.svelte';
import {
  closePaneFind,
  getPaneFindProvider,
  isPaneFindOpen,
  openPaneFind,
  paneOfFindKey,
  resetPaneFindForTest,
  setPaneFindQuery,
  stepPaneFind,
} from '../../stores/paneFind.svelte';

function emitter<T>() {
  const listeners = new Set<(value: T) => void>();
  const event = (listener: (value: T) => void): IDisposable => {
    listeners.add(listener);
    return { dispose: () => listeners.delete(listener) };
  };
  return {
    event,
    fire: (value: T) => { for (const listener of listeners) listener(value); },
    get size() { return listeners.size; },
  };
}

/** An addon that answers every call with the next scripted result. */
function fakeAddon() {
  const results = emitter<ISearchResultChangeEvent>();
  const calls: Array<{ op: string; term?: string; options?: ISearchOptions }> = [];
  let answer: ISearchResultChangeEvent = { resultIndex: -1, resultCount: 0 };
  let failure: Error | null = null;
  const find = (op: string) => (term: string, options?: ISearchOptions): boolean => {
    calls.push({ op, term, options });
    if (failure) throw failure;
    results.fire(answer);
    return answer.resultCount > 0;
  };
  const search: TerminalFindSearch = {
    findNext: find('next'),
    findPrevious: find('previous'),
    clearDecorations: () => { calls.push({ op: 'clearDecorations' }); },
    onDidChangeResults: results.event,
  };
  const writes = emitter<void>();
  const resizes = emitter<{ cols: number; rows: number }>();
  const term: TerminalFindTerminal = {
    clearSelection: () => { calls.push({ op: 'clearSelection' }); },
    onWriteParsed: writes.event,
    onResize: resizes.event,
  };
  return {
    search,
    term,
    calls,
    results,
    writes,
    resizes,
    answer(resultIndex: number, resultCount: number) { answer = { resultIndex, resultCount }; },
    fail(err: Error | null) { failure = err; },
    ops: () => calls.map((c) => c.op),
  };
}

const DECORATIONS = { matchOverviewRuler: '#111111', activeMatchColorOverviewRuler: '#222222', matchBackground: '#111111', activeMatchBackground: '#222222' };

function setup() {
  const addon = fakeAddon();
  const find = createTerminalFind({ term: addon.term, search: addon.search, decorations: () => DECORATIONS });
  return { addon, find };
}

beforeEach(() => {
  resetPaneFindForTest();
});

afterEach(() => {
  resetPaneFindForTest();
  vi.restoreAllMocks();
});

describe('createTerminalFind', () => {
  it('searches the trimmed query case-insensitively from the newest match, with decorations', () => {
    const { addon, find } = setup();
    addon.answer(2, 3);
    find.search('  needle ');
    expect(addon.ops()).toEqual(['clearSelection', 'previous']);
    expect(addon.calls[1]).toMatchObject({ term: 'needle', options: { caseSensitive: false, decorations: DECORATIONS } });
    expect(find.result).toMatchObject({ count: 3, current: 2, pending: false, capped: false, error: null });
  });

  it('does not search again for a query that trims to the current one', () => {
    const { addon, find } = setup();
    addon.answer(0, 1);
    find.search('needle');
    find.search('needle ');
    expect(addon.ops()).toEqual(['clearSelection', 'previous']);
  });

  it('steps down with next and up with previous, and reports where it landed', () => {
    const { addon, find } = setup();
    addon.answer(2, 3);
    find.search('needle');
    addon.answer(0, 3);
    find.next();
    expect(addon.calls.at(-1)).toMatchObject({ op: 'next', term: 'needle' });
    expect(find.result.current).toBe(0);
    addon.answer(2, 3);
    find.previous();
    expect(addon.calls.at(-1)).toMatchObject({ op: 'previous', term: 'needle' });
    expect(find.result.current).toBe(2);
  });

  it('reports no match, and steps nothing without a query', () => {
    const { addon, find } = setup();
    find.next();
    find.previous();
    expect(addon.calls).toEqual([]);
    addon.answer(-1, 0);
    find.search('absent');
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
  });

  it('reports a count at the highlight limit as capped', () => {
    const { addon, find } = setup();
    addon.answer(-1, TERMINAL_FIND_HIGHLIGHT_LIMIT);
    find.search('e');
    expect(find.result).toMatchObject({ count: TERMINAL_FIND_HIGHLIGHT_LIMIT, current: -1, capped: true });
  });

  it('clear removes the decorations and the match selection, once', () => {
    const { addon, find } = setup();
    addon.answer(0, 1);
    find.search('needle');
    addon.calls.length = 0;
    find.clear();
    expect(addon.ops()).toEqual(['clearDecorations', 'clearSelection']);
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false, error: null });
    // A second clear has no match selection of its own to remove: the
    // reader's selection stays.
    find.clear();
    expect(addon.ops()).toEqual(['clearDecorations', 'clearSelection']);
  });

  it('an empty query clears', () => {
    const { addon, find } = setup();
    addon.answer(0, 1);
    find.search('needle');
    addon.calls.length = 0;
    find.search('   ');
    expect(addon.ops()).toEqual(['clearDecorations', 'clearSelection']);
    expect(find.result.count).toBe(0);
  });

  it('marks the count pending on new output or a resize until the addon answers again', () => {
    const { addon, find } = setup();
    addon.writes.fire();
    expect(find.result.pending).toBe(false);
    addon.answer(1, 2);
    find.search('needle');
    addon.writes.fire();
    // Progressive: the bar keeps showing the last count beside the spinner.
    expect(find.result).toMatchObject({ count: 2, pending: true, progressive: true });
    // The addon's own re-search after the writes.
    addon.results.fire({ resultIndex: 1, resultCount: 5 });
    expect(find.result).toMatchObject({ count: 5, current: 1, pending: false });
    addon.resizes.fire({ cols: 40, rows: 10 });
    expect(find.result.pending).toBe(true);
    addon.results.fire({ resultIndex: 0, resultCount: 4 });
    expect(find.result).toMatchObject({ count: 4, current: 0, pending: false });
  });

  it('ignores an answer the addon scheduled before a clear', () => {
    const { addon, find } = setup();
    addon.answer(0, 1);
    find.search('needle');
    find.clear();
    addon.results.fire({ resultIndex: 0, resultCount: 7 });
    expect(find.result).toMatchObject({ count: 0, current: -1 });
  });

  it('shows a failed search as its error and reports it, until a later call succeeds', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const { addon, find } = setup();
    addon.fail(new Error('Cannot use addon until it has been loaded'));
    find.search('needle');
    expect(find.result).toMatchObject({ count: 0, current: -1, error: 'Cannot use addon until it has been loaded' });
    expect(warn).toHaveBeenCalledWith('terminal find failed', 'Cannot use addon until it has been loaded');
    // Output while failed does not read as a pending recount.
    addon.writes.fire();
    expect(find.result.pending).toBe(false);
    addon.fail(null);
    addon.answer(0, 1);
    find.next();
    expect(find.result).toMatchObject({ count: 1, current: 0, error: null });
  });

  it('restyle repaints the current query with the current decorations', () => {
    const addon = fakeAddon();
    let decorations = DECORATIONS;
    const find = createTerminalFind({ term: addon.term, search: addon.search, decorations: () => decorations });
    find.restyle();
    expect(addon.calls).toEqual([]);
    addon.answer(0, 1);
    find.search('needle');
    addon.calls.length = 0;
    decorations = { ...DECORATIONS, matchBackground: '#333333' };
    find.restyle();
    expect(addon.ops()).toEqual(['clearDecorations', 'previous']);
    expect(addon.calls[1].options?.decorations?.matchBackground).toBe('#333333');
  });

  it('dispose drops every listener', () => {
    const { addon, find } = setup();
    expect(addon.results.size + addon.writes.size + addon.resizes.size).toBe(3);
    find.dispose();
    expect(addon.results.size + addon.writes.size + addon.resizes.size).toBe(0);
  });
});

describe('registerTerminalFind', () => {
  const tab1 = terminalFindKey('pane-1', 't1');
  const tab2 = terminalFindKey('pane-1', 't2');

  it('keys each terminal under its pane, apart from its siblings', () => {
    expect(paneOfFindKey(tab1)).toBe('pane-1');
    expect(tab1).not.toBe(tab2);
    expect(terminalFindKey('pane-2', 't1')).not.toBe(tab1);
  });

  it('drives the addon through the pane find store, and release clears it', () => {
    const addon = fakeAddon();
    const registration = registerTerminalFind(tab1, addon.term, addon.search);
    expect(openPaneFind(tab1)).toBe(true);
    addon.answer(1, 2);
    setPaneFindQuery(tab1, 'needle');
    addon.answer(0, 2);
    stepPaneFind(tab1, 1);
    expect(getPaneFindProvider(tab1)?.result).toMatchObject({ count: 2, current: 0 });
    addon.calls.length = 0;
    registration.release();
    expect(addon.ops()).toEqual(['clearDecorations', 'clearSelection']);
    expect(getPaneFindProvider(tab1)).toBeNull();
    expect(addon.results.size + addon.writes.size + addon.resizes.size).toBe(0);
  });

  it('a tab switch leaves each terminal its own provider, bar state and matches', () => {
    const first = fakeAddon();
    const releaseFirst = registerTerminalFind(tab1, first.term, first.search);
    openPaneFind(tab1);
    first.answer(0, 1);
    setPaneFindQuery(tab1, 'alpha');
    // Switching away unmounts the first body; the second mounts.
    releaseFirst.release();
    const second = fakeAddon();
    registerTerminalFind(tab2, second.term, second.search);
    expect(isPaneFindOpen(tab2)).toBe(false);
    expect(second.calls).toEqual([]);
    expect(first.ops()).toContain('clearDecorations');
  });

  it('a re-created terminal searches its open query again, and the old release leaves it', () => {
    const before = fakeAddon();
    const old = registerTerminalFind(tab1, before.term, before.search);
    openPaneFind(tab1);
    before.answer(0, 1);
    setPaneFindQuery(tab1, 'needle');
    const after = fakeAddon();
    after.answer(0, 3);
    registerTerminalFind(tab1, after.term, after.search);
    expect(after.calls.at(-1)).toMatchObject({ op: 'previous', term: 'needle' });
    expect(getPaneFindProvider(tab1)?.result.count).toBe(3);
    old.release();
    expect(getPaneFindProvider(tab1)?.result.count).toBe(3);
    closePaneFind(tab1);
    expect(after.ops().slice(-2)).toEqual(['clearDecorations', 'clearSelection']);
  });
});
