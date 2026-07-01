import { describe, it, expect, afterEach } from 'vitest';
import { mount, unmount, tick } from 'svelte';
// Real production cascade so `max-h-[3lh]`, `text-[0.75rem]`, `leading-relaxed`
// and `whitespace-pre-wrap` resolve to the same geometry the app ships. The
// browser project compiles tailwind against app.css (see vitest.config.ts).
import '../../../app.css';
import TailClampedText from './TailClampedText.svelte';

// happy-dom reports zero geometry, so the tail-visibility invariant below can
// only be checked in a real layout engine. This file runs in the `browser`
// vitest project.
//
// What it pins down (docs/architecture/settle-flicker-analysis.md): while
// collapsed, TailClampedText shows only the last 3 lines of the streaming
// reasoning tail, bottom-anchored. The contract is that the NEWEST line stays
// visible at the bottom of that 3-line window through every reflow — including
// a width re-wrap that carries NO text delta.
//
// That last clause is the regression guard. The original implementation pinned
// the tail imperatively (`$effect: scrollTop = scrollHeight`) with `text` /
// `expanded` as its only dependencies, so it never re-ran on a width change.
// With `whitespace-pre-wrap`, a narrower width re-wraps the body, `scrollHeight`
// grows, and the stale `scrollTop` leaves the window scrolled UP — the tail
// falls out of view until the next text delta re-pins it. In the live app the
// row width oscillates while the scroll spring settles at stream start (the
// a5a5d032 width-reflow strand), so the thinking tail re-wraps mid-stream with
// no delta to fix it. The third test reproduces exactly that: width change,
// no text change, tail must remain visible.

const mounted: Array<{ app: ReturnType<typeof mount>; host: HTMLElement }> = [];

const raf = () => new Promise<void>((r) => requestAnimationFrame(() => r()));

// Rect of the final character of the body's text — the newest streamed glyph.
// (Twin of `tailOverflowPx` in messageTimelineTrace.ts; intentionally separate
// — that one is a single bounded read on a dev hot path, this descends to the
// deepest leaf and keeps sub-pixel precision for the assertions below.)
function lastCharRect(body: HTMLElement): DOMRect {
  let node: Node = body;
  while (node.lastChild) node = node.lastChild; // descend to the trailing text node
  const len = node.textContent?.length ?? 0;
  const range = document.createRange();
  if (node.nodeType === Node.TEXT_NODE && len > 0) {
    range.setStart(node, len - 1);
    range.setEnd(node, len);
  } else {
    range.selectNodeContents(node);
  }
  return range.getBoundingClientRect();
}

// Rect of the first character — the oldest line currently in the tail window.
function firstCharRect(body: HTMLElement): DOMRect {
  let node: Node = body;
  while (node.firstChild) node = node.firstChild; // descend to the leading text node
  const len = node.textContent?.length ?? 0;
  const range = document.createRange();
  if (node.nodeType === Node.TEXT_NODE && len > 0) {
    range.setStart(node, 0);
    range.setEnd(node, 1);
  } else {
    range.selectNodeContents(node);
  }
  return range.getBoundingClientRect();
}

// How far the newest glyph sits BELOW the bottom edge of the clamped window.
// <= 0 means the tail is visible; a positive value is the jump the user sees.
function tailOverflowPx(body: HTMLElement): number {
  return lastCharRect(body).bottom - body.getBoundingClientRect().bottom;
}

// How far the oldest glyph is clipped ABOVE the window top. > 0 means content
// overflows the 3-line window (the clamp is active) — true whether the tail is
// held by scroll position or by a flex bottom-anchor, so the assertion does not
// couple to either mechanism.
function topClipPx(body: HTMLElement): number {
  return body.getBoundingClientRect().top - firstCharRect(body).top;
}

// Total rendered height of the text, first glyph top to last glyph bottom —
// independent of clamping or scroll position. Grows when a narrower width wraps
// the same text across more visual lines, for ANY pin mechanism, so it is a
// neutral check that a re-wrap actually changed the layout.
function contentSpanPx(body: HTMLElement): number {
  return lastCharRect(body).bottom - firstCharRect(body).top;
}

// Mount the real component as a flex child of a fixed-width row, mirroring its
// production parent (ReasoningTailRow lays it out with `flex-1 min-w-0`).
function mountBody(text: string, widthPx: number, expanded = false) {
  const host = document.createElement('div');
  host.style.cssText = `display:flex; width:${widthPx}px; align-items:flex-start;`;
  document.body.appendChild(host);
  const app = mount(TailClampedText, { target: host, props: { text, expanded, testId: 'tail-body' } });
  mounted.push({ app, host });
  const body = host.querySelector<HTMLElement>('[data-testid="tail-body"]');
  if (!body) throw new Error('tail body did not mount');
  return { host, body };
}

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

// Six long logical lines. Each one is wide enough to wrap, so the total
// visual-line count — and thus the rendered height — depends on the container
// width. That width sensitivity is the whole point: it is what an imperative
// pin fails to track.
const LONG = Array.from(
  { length: 6 },
  (_, i) =>
    `Reasoning line ${i}: the model is weighing several considerations here and the sentence runs long enough that it wraps across more than one visual row at a narrow width.`,
).join('\n');

describe('TailClampedText tail-pin', () => {
  it('clamps to 3 lines and keeps the newest line visible on mount', async () => {
    const { body } = mountBody(LONG, 600);
    await tick();
    await raf();

    // The 3-line clamp is active: older content is clipped above the window.
    expect(topClipPx(body)).toBeGreaterThan(0);
    const threeLines = parseFloat(getComputedStyle(body).lineHeight) * 3;
    expect(body.clientHeight).toBeLessThanOrEqual(Math.ceil(threeLines) + 1);

    // Tail visible: the last glyph sits at/above the bottom of the window.
    expect(tailOverflowPx(body)).toBeLessThanOrEqual(1);
  });

  it('shows short content in full without clamping', async () => {
    const { body } = mountBody('line one\nline two', 600);
    await tick();
    await raf();
    const lh = parseFloat(getComputedStyle(body).lineHeight);
    // The box COLLAPSES to its 2 lines of content — proving there is no empty
    // gap. This is the real guard against a flex-end gap regression: if
    // `max-h-[3lh]` ever became a fixed `h-[3lh]`, clientHeight would jump to
    // 3lh and `justify-end` would bottom-align the two lines with a one-line gap
    // above. (Asserting first-glyph position instead would be fooled by the
    // ~2px line-box leading offset of a character Range rect.)
    expect(body.clientHeight).toBeLessThanOrEqual(Math.ceil(lh * 2) + 1);
    expect(body.scrollHeight).toBeLessThanOrEqual(body.clientHeight + 1); // no overflow
    expect(tailOverflowPx(body)).toBeLessThanOrEqual(1); // newest line visible
  });

  // THE GUARD (fails-without / passes-with). A width-only re-wrap must keep the
  // tail visible. The original imperative pin never fired on resize, so the
  // newest line scrolled out of the 3-line window (~234px below it in Chromium).
  it('keeps the newest line visible when only the width changes (re-wrap)', async () => {
    const { host, body } = mountBody(LONG, 600);
    await tick();
    await raf();

    const spanBefore = contentSpanPx(body);
    expect(tailOverflowPx(body)).toBeLessThanOrEqual(1); // visible at the wide width

    // Re-wrap WITHOUT touching `text`: narrower width => more visual lines =>
    // taller text. This is the spring-driven width oscillation, with no delta
    // to re-pin.
    host.style.width = '300px';
    await tick();
    await raf();
    await raf();

    // Sanity: the re-wrap actually re-flowed the text taller, otherwise the
    // test proves nothing about staleness. Mechanism-neutral so it passes on
    // both the old (stale) and new (anchored) implementations.
    expect(contentSpanPx(body)).toBeGreaterThan(spanBefore);

    // The newest line must still be visible. Without a reflow-aware anchor it
    // is not — it sits well below the clamped window.
    expect(tailOverflowPx(body)).toBeLessThanOrEqual(1);
  });
});
