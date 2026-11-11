// The find provider of one xterm, over its search addon
// (stores/paneFind.svelte.ts). The addon matches, selects, scrolls and
// paints; this module turns its answers into the bar's result.
//
// - A query is trimmed and matched case-insensitively as plain text. Each
//   query change starts again at the newest match (the bottom of the
//   buffer); `next` steps down toward newer output, `previous` up, and both
//   wrap.
// - Matches are counted in buffer order up to the addon's highlight limit,
//   reported as capped.
// - New output or a resize while a query is set makes the addon search again
//   once writes pause; until its answer the result is pending, so the bar
//   never shows a count the buffer has moved past.
// - `clear` removes the decorations and the match selection.

import type { IDisposable, IEvent } from '@xterm/xterm';
import type {
  ISearchDecorationOptions,
  ISearchOptions,
  ISearchResultChangeEvent,
} from '@xterm/addon-search';
import {
  paneFindRegionKey,
  registerPaneFindProvider,
  type PaneFindProvider,
  type PaneFindResult,
} from '../../stores/paneFind.svelte';
import { getResolvedTheme } from '../../stores/themeMode.svelte';
import { getXtermFindDecorations } from './terminalTheme';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
import { errString } from '../../utils/errors';

/** The find key of one terminal tab in a pane. Scoped under the pane, so
 * the store drops it when the pane leaves the layout. */
export function terminalFindKey(paneId: string, terminalID: string): string {
  return paneFindRegionKey(paneId, `terminal:${terminalID}`);
}

/** Matches the addon decorates and counts; a count at it is capped. */
export const TERMINAL_FIND_HIGHLIGHT_LIMIT = 1000;

/** The slice of the search addon the provider drives. */
export interface TerminalFindSearch {
  findNext(term: string, options?: ISearchOptions): boolean;
  findPrevious(term: string, options?: ISearchOptions): boolean;
  clearDecorations(): void;
  readonly onDidChangeResults: IEvent<ISearchResultChangeEvent>;
}

/** The slice of the xterm the provider reads. */
export interface TerminalFindTerminal {
  clearSelection(): void;
  readonly onWriteParsed: IEvent<void>;
  readonly onResize: IEvent<{ cols: number; rows: number }>;
}

export interface TerminalFindOptions {
  term: TerminalFindTerminal;
  search: TerminalFindSearch;
  /** The match grounds for the current theme. */
  decorations(): ISearchDecorationOptions;
}

export interface TerminalFind extends PaneFindProvider {
  next(): void;
  previous(): void;
  /** Repaints the matches with the current `decorations()`. */
  restyle(): void;
  /** Drops the addon and terminal listeners. */
  dispose(): void;
}

/** A terminal surface's registered find. */
export interface TerminalFindRegistration {
  /** Repaints the matches after a theme change. */
  restyle(): void;
  /** Unregisters, clears the matches and drops the listeners. Call before
   * the xterm is disposed. */
  release(): void;
}

/**
 * Builds the find of a surface's xterm, painted from the app theme, and
 * registers it under `key` (a layout pane id or `terminalFindKey`).
 * Register once the replay is written, so an open bar's query searches the
 * restored buffer.
 */
export function registerTerminalFind(
  key: string,
  term: TerminalFindTerminal,
  search: TerminalFindSearch,
): TerminalFindRegistration {
  const find = createTerminalFind({
    term,
    search,
    decorations: () => getXtermFindDecorations(getResolvedTheme()),
  });
  const unregister = registerPaneFindProvider(key, find);
  return {
    restyle: () => find.restyle(),
    release() {
      unregister();
      find.dispose();
    },
  };
}

export function createTerminalFind(options: TerminalFindOptions): TerminalFind {
  const { term, search } = options;
  let query = '';
  let count = $state(0);
  let current = $state(-1);
  let pending = $state(false);
  let error = $state<string | null>(null);

  const result: PaneFindResult = {
    get count() { return count; },
    get current() { return current; },
    get pending() { return pending; },
    get capped() { return count >= TERMINAL_FIND_HIGHLIGHT_LIMIT; },
    get error() { return error; },
  };

  function reset(): void {
    count = 0;
    current = -1;
    pending = false;
    error = null;
  }

  function searchOptions(): ISearchOptions {
    return { caseSensitive: false, decorations: options.decorations() };
  }

  // The addon reports through `onDidChangeResults`, synchronously inside
  // the call; a throw is the bar's error until a later call succeeds.
  function run(find: (needle: string) => void): void {
    try {
      find(query);
      error = null;
    } catch (err) {
      reset();
      error = errString(err);
      console.warn('terminal find failed', error);
      reportFrontendDiagnostic('terminal find failed', error);
    }
  }

  const listeners: IDisposable[] = [
    search.onDidChangeResults((event) => {
      // A re-search the addon scheduled before `clear` answers after it.
      if (!query) return;
      count = event.resultCount;
      current = event.resultIndex;
      pending = false;
    }),
    term.onWriteParsed(() => markStale()),
    term.onResize(() => markStale()),
  ];

  function markStale(): void {
    if (query && error === null) pending = true;
  }

  function clear(): void {
    const hadQuery = query !== '';
    query = '';
    reset();
    if (!hadQuery) return;
    search.clearDecorations();
    term.clearSelection();
  }

  return {
    noun: 'terminal',
    result,
    search(raw: string): void {
      const next = raw.trim();
      if (next === query) return;
      if (!next) {
        clear();
        return;
      }
      query = next;
      pending = false;
      // The addon starts from its selection; none starts at the newest match.
      term.clearSelection();
      run((needle) => search.findPrevious(needle, searchOptions()));
    },
    next(): void {
      if (query) run((needle) => search.findNext(needle, searchOptions()));
    },
    previous(): void {
      if (query) run((needle) => search.findPrevious(needle, searchOptions()));
    },
    restyle(): void {
      if (!query || error !== null) return;
      // The addon repaints only for a new term; dropping its decorations
      // makes the same term new. The selection keeps the current match.
      search.clearDecorations();
      run((needle) => search.findPrevious(needle, searchOptions()));
    },
    clear,
    dispose(): void {
      for (const listener of listeners) listener.dispose();
      listeners.length = 0;
    },
  };
}
