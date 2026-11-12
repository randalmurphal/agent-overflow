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
// - Every buffer change while a query is set (`onBufferChange`) re-searches
//   once changes pause, and at least once a second under sustained output,
//   without scrolling. Until then the result is pending, so the bar marks
//   the count it shows as updating.
// - Find owns only the selection its own search made. A re-search keeps
//   that match selected and leaves no selection where there was none. While
//   the reader holds a selection of their own it waits, pending, until that
//   selection goes or the reader steps or types a query.
// - xterm drops the selection when the rows change (the bar opening, a
//   drawer resize). Find's match is not the reader's to lose that way: the
//   re-search selects it again where it was.
// - `clear` removes the decorations, and the selection only while it is
//   find's.

import type { IBufferRange, IDisposable, IEvent } from '@xterm/xterm';
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
import { createRefreshScheduler } from '../../utils/refreshScheduler';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
import { errString } from '../../utils/errors';

/** The find key of one terminal tab in a pane. Scoped under the pane, so
 * the store drops it when the pane leaves the layout. */
export function terminalFindKey(paneId: string, terminalID: string): string {
  return paneFindRegionKey(paneId, `terminal:${terminalID}`);
}

/** Matches the addon decorates and counts; a count at it is capped. */
export const TERMINAL_FIND_HIGHLIGHT_LIMIT = 1000;
/** Quiet time after a buffer change before the re-search. */
export const TERMINAL_FIND_REFRESH_DELAY_MS = 200;
/** The longest sustained output defers a re-search. */
export const TERMINAL_FIND_REFRESH_MAX_WAIT_MS = 1000;

/** The search addon as `loadTerminalSearch` adapts it. */
export interface TerminalFindSearch {
  /** Selects and scrolls to the match after the selection. */
  findNext(term: string, options?: ISearchOptions): boolean;
  /** Selects and scrolls to the match before the selection, else the newest. */
  findPrevious(term: string, options?: ISearchOptions): boolean;
  /** `findPrevious` without scrolling, for a re-search. */
  recount(term: string, options?: ISearchOptions): boolean;
  clearDecorations(): void;
  clearActiveDecoration(): void;
  readonly onDidChangeResults: IEvent<ISearchResultChangeEvent>;
}

/** The slice of the xterm the provider reads. */
export interface TerminalFindTerminal {
  readonly cols: number;
  hasSelection(): boolean;
  getSelectionPosition(): IBufferRange | undefined;
  select(column: number, row: number, length: number): void;
  clearSelection(): void;
  readonly onSelectionChange: IEvent<void>;
  /** Fires inside a resize, before xterm drops the selection for it. */
  readonly onResize: IEvent<{ cols: number; rows: number }>;
}

/** What a surface's built terminal hands its find (`BuiltTerminal`). */
export interface TerminalFindSource {
  term: TerminalFindTerminal;
  search: TerminalFindSearch;
  /** Every change to the searched buffer. */
  onBufferChange(listener: () => void): IDisposable;
}

export interface TerminalFindOptions extends TerminalFindSource {
  /** The match grounds for the current theme. */
  decorations(): ISearchDecorationOptions;
}

export interface TerminalFind extends PaneFindProvider {
  next(): void;
  previous(): void;
  /** Repaints the matches with the current `decorations()`. */
  restyle(): void;
  /** Drops the timers and the addon and terminal listeners. */
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
 * Register once the replay is parsed (its write's callback), so an open
 * bar's query searches the restored buffer and brings its match into view.
 */
export function registerTerminalFind(key: string, source: TerminalFindSource): TerminalFindRegistration {
  const find = createTerminalFind({
    term: source.term,
    search: source.search,
    onBufferChange: source.onBufferChange,
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
  // The selection is the match find's own search made.
  let ownsSelection = false;
  // Inside a call of find's: the selection changes are its own.
  let selecting = false;
  // Inside a resize, whose selection drop is xterm's, not the reader's.
  let resizing = false;
  // Find's match as it was when a resize dropped it.
  let heldMatch: IBufferRange | undefined;

  const result: PaneFindResult = {
    get count() { return count; },
    get current() { return current; },
    get pending() { return pending; },
    get capped() { return count >= TERMINAL_FIND_HIGHLIGHT_LIMIT; },
    // The last count stays beside the spinner while output streams, rather
    // than blinking out on every write.
    progressive: true,
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

  function quietly(change: () => void): void {
    selecting = true;
    try {
      change();
    } finally {
      selecting = false;
    }
  }

  // The addon reports through `onDidChangeResults`, synchronously inside
  // the call; a throw is the bar's error until a later call succeeds.
  function run(find: (needle: string) => boolean): void {
    quietly(() => {
      try {
        heldMatch = undefined;
        ownsSelection = find(query) && term.hasSelection();
        error = null;
      } catch (err) {
        ownsSelection = false;
        reset();
        error = errString(err);
        console.warn('terminal find failed', error);
        reportFrontendDiagnostic('terminal find failed', error);
      }
    });
  }

  function refresh(): void {
    if (!query || error !== null) return;
    if (!ownsSelection && term.hasSelection()) return;
    const kept = ownsSelection;
    const held = heldMatch;
    if (kept && held && !term.hasSelection()) {
      quietly(() => term.select(held.start.x, held.start.y, (held.end.y - held.start.y) * term.cols + held.end.x - held.start.x));
    }
    // The addon repaints only for a new term; dropping its decorations
    // makes the same term new. It searches from the selection, so find's
    // match stays the current one.
    search.clearDecorations();
    run((needle) => search.recount(needle, searchOptions()));
    if (!kept) {
      quietly(() => {
        term.clearSelection();
        search.clearActiveDecoration();
      });
      ownsSelection = false;
      current = -1;
    }
    pending = false;
  }

  const refresher = createRefreshScheduler({
    name: 'terminal find',
    delayMs: TERMINAL_FIND_REFRESH_DELAY_MS,
    maxWaitMs: TERMINAL_FIND_REFRESH_MAX_WAIT_MS,
    run: () => refresh(),
  });

  const listeners: IDisposable[] = [
    search.onDidChangeResults((event) => {
      if (!query) return;
      count = event.resultCount;
      current = event.resultIndex;
      pending = false;
    }),
    options.onBufferChange(() => {
      if (!query || error !== null) return;
      pending = true;
      refresher.request();
    }),
    term.onResize(() => {
      if (!ownsSelection) return;
      heldMatch = term.getSelectionPosition() ?? heldMatch;
      resizing = true;
      queueMicrotask(() => { resizing = false; });
    }),
    term.onSelectionChange(() => {
      if (selecting || resizing) return;
      ownsSelection = false;
      // The reader's selection went: a re-search held for it may run.
      if (pending && !term.hasSelection()) refresher.request();
    }),
  ];

  function clear(): void {
    const hadQuery = query !== '';
    query = '';
    heldMatch = undefined;
    reset();
    refresher.reset();
    if (!hadQuery) return;
    quietly(() => {
      search.clearDecorations();
      if (ownsSelection) term.clearSelection();
    });
    ownsSelection = false;
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
      refresher.reset();
      // The addon starts from its selection; none starts at the newest match.
      quietly(() => term.clearSelection());
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
      // Held like any re-search while the reader holds a selection.
      pending = true;
      refresher.reset();
      refresh();
    },
    clear,
    dispose(): void {
      refresher.dispose();
      for (const listener of listeners) listener.dispose();
      listeners.length = 0;
    },
  };
}
