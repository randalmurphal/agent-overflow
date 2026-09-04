// The engine's actual scroll lattice is not necessarily a CSS or device
// pixel: browser zoom introduces a third scale. Measure a private scroller,
// never move the reader's viewport to discover a capability.
export interface ScrollGrid {
  /** Distance between accepted positions, in CSS pixels. */
  readonly quantum: number;
  /** Offset from a position to the middle of its accepted write interval. */
  readonly writeOffset: number;
  /** Largest observed position error after fitting the lattice. */
  readonly readbackError: number;
}

const TRANSITIONS = 16;
const SEARCH_STEPS = 24;

/** Pure calibration seam; read writes an isolated scroller and returns its readback. */
export function measureScrollGrid(read: (requested: number) => number): ScrollGrid {
  if (read(0) !== 0) throw new Error('Scroll grid calibration must start at zero');
  let previous = 0;
  let boundary = 0;
  let searchWidth = 1;
  let firstPosition = 0;
  let offsetSum = 0;
  const positions: number[] = [];
  for (let event = 0; event <= TRANSITIONS; event++) {
    let low = boundary;
    let high = low + searchWidth;
    let accepted = read(high);
    while (accepted <= previous && high < 4096) {
      searchWidth *= 2;
      high = low + searchWidth;
      accepted = read(high);
    }
    if (!Number.isFinite(accepted) || accepted <= previous) {
      throw new Error('Scroll grid calibration could not advance its private scroller');
    }
    for (let step = 0; step < SEARCH_STEPS; step++) {
      const middle = (low + high) / 2;
      const value = read(middle);
      if (value > previous) high = middle;
      else low = middle;
    }
    boundary = high;
    const position = read(high);
    positions.push(position);
    if (event === 0) firstPosition = position;
    offsetSum += boundary - position;
    searchWidth = Math.max(position - previous, 1 / 64) * 2;
    previous = position;
  }
  // Averaging multiple accepted positions avoids treating a readback's
  // limited precision (Android reports 1/32px) as the rendering quantum.
  const quantum = (previous - firstPosition) / TRANSITIONS;
  if (!(quantum > 0) || !Number.isFinite(quantum)) {
    throw new Error('Scroll grid calibration returned an invalid quantum');
  }
  const offset = offsetSum / (TRANSITIONS + 1) + quantum / 2;
  const readbackError = Math.max(...positions.map((position, index) =>
    Math.abs(position - (index + 1) * quantum)));
  return { quantum, writeOffset: Math.abs(offset) < quantum * 1e-5 ? 0 : offset, readbackError };
}

interface DocumentGrid {
  dirty: boolean;
  ratio: number;
  grid: ScrollGrid | null;
}
const documents = new WeakMap<Document, DocumentGrid>();

/** One cached measurement per document/scale, with no per-frame DOM geometry reads. */
export function documentScrollGrid(doc: Document): ScrollGrid {
  const win = doc.defaultView;
  if (!win) throw new Error('Scroll grid requires a document with a window');
  let state = documents.get(doc);
  if (!state) {
    state = { dirty: true, ratio: 0, grid: null };
    documents.set(doc, state);
    const ownedState = state;
    win.addEventListener('resize', () => { ownedState.dirty = true; }, { passive: true });
  }
  const ratio = win.devicePixelRatio;
  if (state.grid && !state.dirty && state.ratio === ratio) return state.grid;
  const scroller = doc.createElement('div');
  const content = doc.createElement('div');
  scroller.style.cssText = 'all:initial;position:fixed;left:-10000px;top:0;width:1px;height:1px;overflow:scroll;visibility:hidden;pointer-events:none;contain:strict;scroll-behavior:auto;';
  content.style.cssText = 'all:initial;display:block;width:1px;height:8192px;';
  scroller.appendChild(content);
  doc.documentElement.appendChild(scroller);
  try {
    const grid = measureScrollGrid((requested) => {
      scroller.scrollTop = requested;
      return scroller.scrollTop;
    });
    state.grid = grid;
    state.ratio = ratio;
    state.dirty = false;
    return grid;
  } finally {
    scroller.remove();
  }
}
