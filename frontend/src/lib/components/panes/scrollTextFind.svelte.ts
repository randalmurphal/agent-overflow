// A find provider for a pane whose whole content is rendered in one plain
// scroll container (the plan pane): the matches are the rendered text the
// highlight utility walks, so the count, the paint and the steps agree.
// A new query starts at the first match at or below the viewport top (the
// last when none is below); a step scrolls its match into view.

import type { PaneFindProvider, PaneFindResult } from '../../stores/paneFind.svelte';
import { createFindHighlighter, type FindHighlighter, type FindMatch } from '../../utils/findHighlights';

export interface ScrollTextFindOptions {
  root(): HTMLElement | null;
  /** From `findHighlightName` for the pane. */
  name: string;
  noun(): string;
}

export interface ScrollTextFind extends PaneFindProvider {
  next(): void;
  previous(): void;
}

function startIndex(root: HTMLElement, matches: readonly FindMatch[]): number {
  if (matches.length === 0) return -1;
  const top = root.getBoundingClientRect().top;
  const below = matches.findIndex((m) => m.range.getBoundingClientRect().bottom > top);
  return below >= 0 ? below : matches.length - 1;
}

function scrollIntoView(root: HTMLElement, match: FindMatch): void {
  const view = root.getBoundingClientRect();
  const rect = match.range.getBoundingClientRect();
  if (rect.top >= view.top && rect.bottom <= view.bottom) return;
  root.scrollTop += rect.top - view.top - root.clientHeight / 3;
}

export function createScrollTextFind(options: ScrollTextFindOptions): ScrollTextFind {
  let count = $state(0);
  let current = $state(-1);
  let highlighter: FindHighlighter | null = null;
  let highlighterRoot: HTMLElement | null = null;

  const result: PaneFindResult = {
    get count() { return count; },
    get current() { return current; },
    get pending() { return false; },
  };

  function release(): void {
    highlighter?.dispose();
    highlighter = null;
    highlighterRoot = null;
    count = 0;
    current = -1;
  }

  function ensure(root: HTMLElement): FindHighlighter {
    if (highlighter && highlighterRoot === root) return highlighter;
    release();
    highlighterRoot = root;
    highlighter = createFindHighlighter({
      root,
      name: options.name,
      // Content changed under the session: keep the position, clamped.
      onMatches: (matches) => {
        count = matches.length;
        current = matches.length === 0 ? -1 : Math.min(Math.max(current, 0), matches.length - 1);
        highlighter?.setCurrent(current >= 0 ? { match: current } : null);
      },
    });
    return highlighter;
  }

  function step(delta: 1 | -1): void {
    const root = highlighterRoot;
    const matches = highlighter?.matches ?? [];
    if (!root || matches.length === 0) return;
    current = current < 0 ? 0 : (current + delta + matches.length) % matches.length;
    highlighter!.setCurrent({ match: current });
    scrollIntoView(root, matches[current]);
  }

  return {
    get noun() { return options.noun(); },
    result,
    search(query: string): void {
      const root = options.root();
      if (!root || !query.trim()) {
        release();
        return;
      }
      const h = ensure(root);
      h.setQuery(query);
      current = startIndex(root, h.matches);
      count = h.matches.length;
      h.setCurrent(current >= 0 ? { match: current } : null);
      if (current >= 0) scrollIntoView(root, h.matches[current]);
    },
    next: () => step(1),
    previous: () => step(-1),
    clear: release,
  };
}
