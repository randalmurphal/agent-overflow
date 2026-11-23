import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import TailClampedText from './TailClampedText.svelte';
import { TAIL_WINDOW_CAP_CHARS } from './tailWindow';

const hosts: HTMLElement[] = [];
afterEach(() => {
  cleanup();
  for (const host of hosts.splice(0)) host.remove();
});

function mountTail(text: string, expanded: boolean) {
  const host = document.createElement('div');
  host.style.cssText = 'display:flex; width:600px; align-items:flex-start';
  document.body.appendChild(host);
  hosts.push(host);
  const view = render(TailClampedText, { target: host, props: { text, expanded } });
  const body = host.firstElementChild as HTMLElement;
  const inner = body.firstElementChild as HTMLElement;
  return { ...view, body, inner };
}

async function settle(inner: HTMLElement) {
  await tick();
  await raf();
  await waitFor(() => inner.style.transform === '', 'thinking slide to settle');
}

function lastGlyphBottom(inner: HTMLElement): number {
  const node = inner.firstChild!;
  const end = node.textContent!.trimEnd().length;
  const range = document.createRange();
  range.setStart(node, end - 1);
  range.setEnd(node, end);
  return range.getBoundingClientRect().bottom;
}

describe('thinking trailing whitespace', () => {
  for (const expanded of [false, true]) {
    it(`holds paragraph separators until text follows (expanded=${expanded})`, async () => {
      const first = 'First line\nSecond line\nThird line';
      const { body, inner, rerender } = mountTail(first, expanded);
      await settle(inner);
      const height = body.getBoundingClientRect().height;
      const glyphBottom = lastGlyphBottom(inner);

      for (const suffix of ['\n', '\n\n', '\n\n ', '\n\n \t', '\n\n \t']) {
        await rerender({ text: first + suffix, expanded });
        await settle(inner);
        expect(body.textContent).toBe(first);
        expect(body.getBoundingClientRect().height).toBe(height);
        expect(lastGlyphBottom(inner)).toBe(glyphBottom);
      }

      const continued = first + '\n\n \tNext paragraph.';
      await rerender({ text: continued + '\r\n\r\n', expanded });
      await settle(inner);
      expect(body.textContent).toBe(continued);
      if (expanded) expect(body.getBoundingClientRect().height).toBeGreaterThan(height);
      const bottomGap = body.getBoundingClientRect().bottom - lastGlyphBottom(inner);
      expect(bottomGap).toBeGreaterThanOrEqual(0);
      expect(bottomGap).toBeLessThan(Number.parseFloat(getComputedStyle(body).lineHeight));
    });

    it(`preserves indentation through empty, replacement and remount states (expanded=${expanded})`, async () => {
      const view = mountTail('\r\n \t', expanded);
      await settle(view.inner);
      expect(view.body.textContent).toBe('');
      expect(view.body.getBoundingClientRect().height).toBe(0);

      const text = '  Indented paragraph.\n\n  Another paragraph.';
      await view.rerender({ text: text + '\n\n', expanded });
      await settle(view.inner);
      expect(view.body.textContent).toBe(text);

      for (const nextExpanded of [!expanded, expanded]) {
        await view.rerender({ text: text + '\n\n', expanded: nextExpanded });
        await settle(view.inner);
        expect(view.body.textContent).toBe(text);
      }

      await view.rerender({ text: '\n\n', expanded });
      await settle(view.inner);
      expect(view.body.textContent).toBe('');
      await view.unmount();
      const remounted = mountTail(text + '\n\n', expanded);
      await settle(remounted.inner);
      expect(remounted.body.textContent).toBe(text);
    });
  }

  it('excludes trailing whitespace from window cuts on mount and append', async () => {
    const text = Array.from({ length: 180 }, (_, i) => `Line ${i}: ${'reasoning '.repeat(8)}`).join('\n') + 'done';
    const suffix = '\n'.repeat(TAIL_WINDOW_CAP_CHARS + 1);
    const { body, inner, rerender } = mountTail(text, false);
    await settle(inner);
    const visible = body.textContent;
    expect(visible!.length).toBeLessThan(TAIL_WINDOW_CAP_CHARS);
    const glyphBottom = lastGlyphBottom(inner);

    await rerender({ text: text + suffix, expanded: false });
    await settle(inner);
    expect(body.textContent).toBe(visible);
    expect(lastGlyphBottom(inner)).toBe(glyphBottom);

    const remounted = mountTail(text + suffix, false);
    await settle(remounted.inner);
    expect(remounted.body.textContent).toBe(visible);

    await rerender({ text: 'Replacement summary.\n\n', expanded: false });
    await settle(inner);
    expect(body.textContent).toBe('Replacement summary.');
  });
});
