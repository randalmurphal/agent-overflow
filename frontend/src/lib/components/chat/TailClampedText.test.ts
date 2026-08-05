import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import TailClampedText from './TailClampedText.svelte';
import { TAIL_WINDOW_CAP_CHARS, TAIL_WINDOW_MIN_KEEP_CHARS } from './tailWindow';

describe('<TailClampedText>', () => {
  afterEach(() => {
    cleanup();
  });

  it('renders the text and wires id + testId', () => {
    const { container } = render(TailClampedText, {
      props: { text: 'reasoning tail', expanded: false, id: 'row-1', testId: 'body' },
    });
    const el = container.querySelector('[data-testid="body"]');
    expect(el?.textContent).toBe('reasoning tail');
    expect(el?.getAttribute('id')).toBe('row-1');
  });

  it('clamps to 3 lines and bottom-anchors the tail when collapsed', () => {
    const { container } = render(TailClampedText, {
      props: { text: 'x', expanded: false, testId: 'body' },
    });
    const className = container.querySelector('[data-testid="body"]')?.className ?? '';
    expect(className).toMatch(/max-h-\[3lh\]/);
    expect(className).toMatch(/overflow-hidden/);
    // The flex bottom-anchor IS the fix (replaces the old imperative
    // `scrollTop = scrollHeight` pin). Lock it here so its removal fails fast in
    // happy-dom; the real-geometry behaviour is guarded in
    // tailClampedText.browser.test.ts.
    expect(className).toMatch(/justify-end/);
  });

  it('drops the clamp when expanded', () => {
    const { container } = render(TailClampedText, {
      props: { text: 'x', expanded: true, testId: 'body' },
    });
    const className = container.querySelector('[data-testid="body"]')?.className ?? '';
    expect(className).not.toMatch(/max-h-\[3lh\]/);
    expect(className).not.toMatch(/overflow-hidden/);
  });

  // The wrap-stable layout window (tailWindow.ts). Only the newline-cut
  // path is reachable under happy-dom — the measured cut needs real
  // geometry and is covered in tailClampedText.browser.test.ts.
  describe('layout window', () => {
    const LINE = 'w'.repeat(99); // + '\n' → 100 chars per logical line
    const longText = (lines: number): string => Array.from({ length: lines }, () => LINE).join('\n');
    const bodyText = (container: HTMLElement): string =>
      container.querySelector('[data-testid="body"]')?.textContent ?? '';

    it('windows an over-cap collapsed tail at a hard newline', () => {
      const text = longText(120); // ~12k chars, well over the cap
      const { container } = render(TailClampedText, {
        props: { text, expanded: false, testId: 'body' },
      });
      flushSync();

      const shown = bodyText(container);
      expect(shown.length).toBeLessThanOrEqual(TAIL_WINDOW_CAP_CHARS);
      expect(shown.length).toBeGreaterThanOrEqual(TAIL_WINDOW_MIN_KEEP_CHARS);
      // The window is a suffix of the full text, cut just after a '\n' —
      // the wrap-stable boundary.
      expect(text.endsWith(shown)).toBe(true);
      expect(text[text.length - shown.length - 1]).toBe('\n');
    });

    it('seeds the window at mount, before effects run', () => {
      const text = longText(120);
      const { container } = render(TailClampedText, {
        props: { text, expanded: false, testId: 'body' },
      });
      // No flushSync: the init-time seed must window the FIRST render,
      // so a windowing remount with a large retained tail never lays
      // out the full string even once.
      const shown = bodyText(container);
      expect(shown.length).toBeLessThanOrEqual(TAIL_WINDOW_CAP_CHARS);
      expect(text.endsWith(shown)).toBe(true);
    });

    it('keeps rendering the full text while under the cap', async () => {
      const under = longText(50); // ~5k chars
      const { container, rerender } = render(TailClampedText, {
        props: { text: under, expanded: false, testId: 'body' },
      });
      flushSync();
      expect(bodyText(container)).toBe(under);

      // Streaming append past the cap → the window engages.
      const over = under + '\n' + longText(50);
      await rerender({ text: over });
      flushSync();
      const shown = bodyText(container);
      expect(shown.length).toBeLessThan(over.length);
      expect(over.endsWith(shown)).toBe(true);
    });

    it('shows the full text when expanded, even after a collapsed cut', async () => {
      const text = longText(120);
      const { container, rerender } = render(TailClampedText, {
        props: { text, expanded: false, testId: 'body' },
      });
      flushSync();
      expect(bodyText(container).length).toBeLessThan(text.length);

      await rerender({ expanded: true });
      flushSync();
      expect(bodyText(container)).toBe(text);

      // Collapsing again re-windows.
      await rerender({ expanded: false });
      flushSync();
      expect(bodyText(container).length).toBeLessThan(text.length);
    });

    it('resets the window when the text is replaced instead of appended', async () => {
      const { container, rerender } = render(TailClampedText, {
        props: { text: longText(120), expanded: false, testId: 'body' },
      });
      flushSync();
      expect(bodyText(container).length).toBeLessThan(longText(120).length);

      // A dropped retained tail (prune/eviction/overwrite): live tail →
      // shorter rune-trimmed summary.
      await rerender({ text: 'settled summary tail' });
      flushSync();
      expect(bodyText(container)).toBe('settled summary tail');
    });
  });
});
