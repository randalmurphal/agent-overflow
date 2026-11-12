// Terminal find's paint: a layer over an xterm's grid that draws a
// translucent rectangle per run of matched cells (terminalFind.svelte.ts).
// Find does not use xterm's decorations: xterm 6 can leave a decoration
// registered for the terminal's life when the parser disposes its marker
// (a redraw, a deleted line, trimming, a reflow), while a bare marker
// leaves nothing behind.
//
// - Each run is a marker on its row plus its columns, so it follows its row
//   as output inserts, deletes and scrolls lines; a run whose row is gone
//   (its marker disposed) is not drawn. A column change makes every run's columns stale,
//   so nothing is drawn until find paints again after its re-search.
// - The layer is a child of the mount, after xterm's element, so it draws
//   over the cells and under the surface's find bar; each draw places the
//   rectangles on `.xterm-screen` from the layer's own origin. It takes no
//   pointer events, so a drag on a highlighted cell selects as anywhere else.
// - It draws after each paint and after every xterm render (output,
//   scrolling, resizes), only while find has runs to paint. A draw walks
//   every run, at most the runs of the match limit, and keeps a DOM node
//   only for each run on a row in the viewport, reusing them across draws.

import type { IDisposable, IMarker, Terminal } from '@xterm/xterm';
import type { TerminalFindColors } from './terminalTheme';

/** Class names on the drawn rectangles. */
export const TERMINAL_FIND_MATCH_CLASS = 'terminal-find-match';
export const TERMINAL_FIND_ACTIVE_CLASS = 'terminal-find-active';

/** Matched cells on one row: the row's marker, the first column and the
 * column count. */
export interface TerminalFindRun {
  marker: IMarker;
  x: number;
  width: number;
}

export interface TerminalFindLayer {
  /** Draws each match's runs, those of `matches[active]` in the current
   * ground and the others in the match ground, until the next paint or
   * clear. */
  paint(matches: readonly (readonly TerminalFindRun[])[], active: number, colors: TerminalFindColors): void;
  /** Removes the layer and stops listening to the terminal. */
  clear(): void;
}

export function createTerminalFindLayer(mount: HTMLElement, terminal: Terminal): TerminalFindLayer {
  let matches: readonly (readonly TerminalFindRun[])[] = [];
  let active = -1;
  let colors: TerminalFindColors = {};
  let columns = 0;
  let layer: HTMLDivElement | null = null;
  let rendered: IDisposable | null = null;
  const nodes: HTMLDivElement[] = [];

  function place(node: HTMLDivElement, isActive: boolean, left: number, top: number, width: number, height: number): void {
    const className = isActive ? TERMINAL_FIND_ACTIVE_CLASS : TERMINAL_FIND_MATCH_CLASS;
    if (node.className !== className) node.className = className;
    node.style.background = (isActive ? colors.current : colors.match) ?? '';
    node.style.left = `${left}px`;
    node.style.top = `${top}px`;
    node.style.width = `${width}px`;
    node.style.height = `${height}px`;
  }

  function draw(): void {
    if (!layer) return;
    const screen = terminal.element?.querySelector('.xterm-screen');
    let shown = 0;
    if (screen && terminal.cols === columns && terminal.rows > 0) {
      const grid = screen.getBoundingClientRect();
      // The layer fills its containing block; place the cells from its origin.
      const box = layer.getBoundingClientRect();
      const cellWidth = grid.width / terminal.cols;
      const cellHeight = grid.height / terminal.rows;
      const left = grid.left - box.left;
      const top = grid.top - box.top;
      const viewport = terminal.buffer.active.viewportY;
      for (let index = 0; index < matches.length; index += 1) {
        for (const run of matches[index]) {
          // A disposed marker's line is -1, out of view.
          const row = run.marker.line - viewport;
          if (row < 0 || row >= terminal.rows) continue;
          let node = nodes[shown];
          if (!node) {
            node = document.createElement('div');
            node.style.position = 'absolute';
            nodes.push(node);
            layer.appendChild(node);
          }
          place(node, index === active, left + run.x * cellWidth, top + row * cellHeight, run.width * cellWidth, cellHeight);
          shown += 1;
        }
      }
    }
    for (const node of nodes.splice(shown)) node.remove();
  }

  function clear(): void {
    rendered?.dispose();
    rendered = null;
    layer?.remove();
    layer = null;
    nodes.length = 0;
    matches = [];
    active = -1;
  }

  return {
    paint(next, nextActive, nextColors): void {
      if (next.length === 0) {
        clear();
        return;
      }
      matches = next;
      active = nextActive;
      colors = nextColors;
      columns = terminal.cols;
      if (!layer) {
        layer = document.createElement('div');
        layer.dataset.terminalFindLayer = '';
        layer.style.cssText = 'position:absolute;inset:0;overflow:hidden;pointer-events:none';
        mount.appendChild(layer);
        rendered = terminal.onRender(draw);
      }
      draw();
    },
    clear,
  };
}
