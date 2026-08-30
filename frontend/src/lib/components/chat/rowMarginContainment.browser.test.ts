import { describe, it, expect, afterEach } from 'vitest';
// Import the REAL production stylesheet so these assertions are coupled to the
// actual fix -- the `[data-row-geometry-content] { display: flow-root }` rule
// in app.css. Delete that rule and the containment test below fails: that is
// the fails-without / passes-with guard for the settle-moment scroll flicker.
// (Full mechanism: docs/architecture/settle-flicker-analysis.md.)
import '../../../app.css';

// happy-dom reports zero geometry, so this invariant -- a timeline row's
// trailing bottom margin must not escape the row's content box -- can only be
// verified in a real layout engine. This file runs in the `browser` vitest
// project (real Chromium via Playwright); see frontend/vitest.config.ts.

const mounted: HTMLElement[] = [];

// A faithful slice of the timeline row chain:
//   item   -- the virtualizer's row wrapper stand-in. `contain: layout` makes it its
//             own BFC; that is what TRAPS a margin which escapes the row below,
//             producing the wrapper-vs-content-box divergence that drives the snap.
//   row    -- [data-row-index]: the element the per-row ResizeObserver measures
//             (content-box). The fix's job is to keep this in agreement with
//             what the row wrapper measures.
//   geo    -- [data-row-geometry-content]: the wrapper app.css turns into a BFC.
//   child  -- the row content carrying the trailing margin.
function mountRow(child: HTMLElement) {
  const item = document.createElement('div');
  item.style.cssText = 'contain: layout style; width: 600px;';
  const row = document.createElement('div');
  row.setAttribute('data-row-index', '0');
  const geo = document.createElement('div');
  geo.setAttribute('data-row-geometry-content', '');
  geo.appendChild(child);
  row.appendChild(geo);
  item.appendChild(row);
  document.body.appendChild(item);
  mounted.push(item);
  return { item, row, geo };
}

afterEach(() => {
  for (const el of mounted.splice(0)) el.remove();
});

describe('timeline row margin containment (settle-flicker fix)', () => {
  it('keeps a trailing bottom margin inside the row content box', () => {
    const child = document.createElement('div');
    child.style.cssText = 'height: 100px; margin-bottom: 20px;';
    const { row, geo } = mountRow(child);

    // With `display: flow-root`, geo is a BFC: the 20px bottom margin is part of
    // geo's box and propagates to [data-row-index]'s content box. Without the
    // rule the margin collapses OUT (geo/row measure only the 100px child) and
    // is trapped by the row wrapper instead -- the divergence that oscillates.
    expect(child.offsetHeight).toBe(100);
    expect(geo.offsetHeight).toBe(child.offsetHeight + 20);
    expect(row.clientHeight).toBe(geo.offsetHeight);
  });

  it('keeps a markdown row flush at the top (BFC x edge-reset coupling)', () => {
    // .markdown-body > .md-committed > h1.md-blk: h1 has an intrinsic 0.75rem
    // top margin (app.css `.markdown-body h1`), zeroed by the
    // explicit `.sd-trim-first-block` marker. Streamdown moves that marker
    // only when the outer block changes, so nested syntax-span insertions do
    // not invalidate every prior md-blk. The BFC is flush
    // -safe ONLY because of that reset -- a BFC traps a child's top margin as
    // stray top space. If the reset regresses, this fails (the coupling guard).
    const body = document.createElement('div');
    body.className = 'markdown-body';
    const committed = document.createElement('div');
    committed.className = 'md-committed';
    const h1 = document.createElement('h1');
    h1.className = 'md-blk sd-trim-first-block';
    h1.textContent = 'Heading';
    committed.appendChild(h1);
    body.appendChild(committed);
    const { geo } = mountRow(body);

    // No stray top space: the first block's top aligns with the row wrapper top.
    const gap = h1.getBoundingClientRect().top - geo.getBoundingClientRect().top;
    expect(gap).toBe(0);
  });
});
