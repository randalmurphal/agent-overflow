import { describe, it, expect, afterEach } from 'vitest';
import { mount, unmount, tick } from 'svelte';
// Real production cascade: the containment contract below depends on how
// `max-w-[82%]`, the bubble's shrink-to-fit flex sizing, and the summary
// paragraph's wrapping utilities compose. The browser project compiles
// tailwind against app.css (see vitest.config.ts).
import '../../../app.css';
import UserMessage from './UserMessage.svelte';
import type { Item } from '../../types/models';

// happy-dom reports zero geometry, so horizontal-containment invariants can
// only be verified in a real layout engine. This file runs in the `browser`
// vitest project (real Chromium via Playwright).
//
// Regression guard for pasted plaintext tables (e.g. copied out of Teams):
// border rows are long unbroken runs of `-` / `|`, and cell padding can be
// NBSP, so a line's min-content width is the full table width. The bubble is
// a shrink-to-fit flex child (`items-end`), whose fit-content width floors at
// min-content — with `overflow-wrap: break-word` (which does NOT lower
// min-content) the bubble blew out past the pane edge instead of wrapping.

const mounted: Array<{ app: object; host: HTMLElement }> = [];

function makeItem(summary: string): Item {
  return {
    id: 'item-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'user_message',
    role: 'user',
    status: 'completed',
    summary,
    createdAt: 1784039160000,
    updatedAt: 1784039160000,
  };
}

async function mountMessage(summary: string, width = 700): Promise<HTMLElement> {
  const host = document.createElement('div');
  host.style.cssText = `width: ${width}px;`;
  document.body.appendChild(host);
  const app = mount(UserMessage, { target: host, props: { item: makeItem(summary) } });
  mounted.push({ app, host });
  await tick();
  return host;
}

afterEach(async () => {
  for (const { app, host } of mounted.splice(0)) {
    await unmount(app);
    host.remove();
  }
});

function bubbleOverflowPx(host: HTMLElement): number {
  const bubble = host.querySelector<HTMLElement>('[data-testid="user-message-bubble"]');
  expect(bubble).not.toBeNull();
  const hostRect = host.getBoundingClientRect();
  const bubbleRect = bubble!.getBoundingClientRect();
  // The bubble is right-aligned, so a blowout escapes past the LEFT edge.
  return Math.max(hostRect.left - bubbleRect.left, bubbleRect.right - hostRect.right);
}

describe('user message horizontal containment', () => {
  it('contains a pasted plaintext table with unbroken border runs', async () => {
    const border = `+${'-'.repeat(160)}+`;
    const row = `|${' cell '.padEnd(80, ' ')}|${' another cell '.padEnd(79, ' ')}|`;
    const table = [border, row, border, row, border].join('\n');
    const host = await mountMessage(`check this out\n\n${table}`);
    expect(bubbleOverflowPx(host)).toBeLessThanOrEqual(0);
  });

  it('contains NBSP-padded lines (no soft-wrap opportunities)', async () => {
    const nbspLine = ('col1' + '\u00a0'.repeat(3) + 'value' + '\u00a0'.repeat(3)).repeat(20);
    const host = await mountMessage(nbspLine);
    expect(bubbleOverflowPx(host)).toBeLessThanOrEqual(0);
  });

  it('contains a single long unbroken token (URL/path)', async () => {
    const host = await mountMessage(`https://example.com/${'a'.repeat(400)}`);
    expect(bubbleOverflowPx(host)).toBeLessThanOrEqual(0);
  });

  it('still shrink-wraps a short message instead of stretching to 82%', async () => {
    const host = await mountMessage('short');
    const bubble = host.querySelector<HTMLElement>('[data-testid="user-message-bubble"]')!;
    expect(bubble.getBoundingClientRect().width).toBeLessThan(200);
  });
});
