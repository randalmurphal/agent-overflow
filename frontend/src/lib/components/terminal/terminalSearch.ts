// The search addon as the find provider drives it (terminalFind.svelte.ts).
//
// - The addon re-searches by itself 200ms after every write or resize, and
//   that re-search selects a match, replacing whatever the reader selected.
//   Here it never does: the provider schedules every re-search.
// - The addon memoizes line text until a line feed, cursor move, resize or
//   15s pass, so a search after `clear()` would match the lines it removed.
//   Every search here starts from the buffer as it is.
//
// Both reach addon members outside its typings. The version is pinned in
// package.json, and terminalFind.browser.test.ts fails if the members move
// (a reader's selection through output; a re-count after clear()).

import type { Terminal } from '@xterm/xterm';
import { SearchAddon, type ISearchOptions } from '@xterm/addon-search';
import { TERMINAL_FIND_HIGHLIGHT_LIMIT, type TerminalFindSearch } from './terminalFind.svelte';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';

interface SearchAddonInternals {
  _updateMatches(): void;
  _lineCache: { value?: { _destroyLinesCache(): void } };
  findPrevious(term: string, options?: ISearchOptions, internal?: { noScroll: boolean }): boolean;
}

/** Loads the search addon into `terminal` and returns the provider's view of it. */
export function loadTerminalSearch(terminal: Terminal): TerminalFindSearch {
  const addon = new SearchAddon({ highlightLimit: TERMINAL_FIND_HIGHLIGHT_LIMIT });
  const internals = addon as unknown as SearchAddonInternals;
  if (typeof internals._updateMatches !== 'function' || !('_lineCache' in internals)) {
    reportFrontendDiagnostic('terminal find addon internals missing', 'addon-search');
  }
  internals._updateMatches = () => {};
  terminal.loadAddon(addon);

  function freshLines(): void {
    internals._lineCache.value?._destroyLinesCache();
  }

  return {
    findNext(term, options) {
      freshLines();
      return addon.findNext(term, options);
    },
    findPrevious(term, options) {
      freshLines();
      return addon.findPrevious(term, options);
    },
    recount(term, options) {
      freshLines();
      return internals.findPrevious.call(addon, term, options, { noScroll: true });
    },
    clearDecorations: () => addon.clearDecorations(),
    clearActiveDecoration: () => addon.clearActiveDecoration(),
    onDidChangeResults: (listener) => addon.onDidChangeResults(listener),
  };
}
