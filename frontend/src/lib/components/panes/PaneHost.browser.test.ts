// The pane strip's reveal glide in real Chromium, where scrollLeft reads
// back rounded: a glide in flight yields to a scroll it did not make, such
// as a scroll-into-view of a control in a pane that just opened.

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import PaneHost from './PaneHost.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { focusPane, registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { raf, waitFor } from '../../../test/helpers/browserFrames';

let host: HTMLDivElement;
let app: ReturnType<typeof mount> | null = null;

beforeEach(() => {
  host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:0;width:500px;height:400px;display:flex';
  document.body.append(host);
});

afterEach(async () => {
  if (app) await unmount(app);
  app = null;
  host.remove();
  resetPaneLayoutForTest();
  resetPanesForTest();
});

it('a reveal glide yields to a scroll it did not make', async () => {
  for (const id of ['a', 'b', 'c']) registerPaneForTest(id, createThreadPane({ paneId: id }));
  setPaneLayoutItemsForTest(['a', 'b', 'c'].map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  focusPane('a');
  app = mount(PaneHost, { target: host });
  const strip = host.querySelector<HTMLElement>('[data-testid="pane-host"]');
  const c = host.querySelector<HTMLElement>('[data-pane-id="c"]');
  if (!strip || !c) throw new Error('the pane strip did not mount');
  await waitFor(() => strip.scrollWidth - strip.clientWidth > 600, 'a strip wider than its viewport');
  // The mount places the focused pane; wait for that to settle.
  let settled = -1;
  await waitFor(() => {
    const now = strip.scrollLeft;
    const still = now === settled;
    settled = now;
    return still;
  }, 'the strip at rest');
  const before = strip.scrollLeft;

  // Focusing the last pane starts a glide toward it.
  c.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, isPrimary: true }));
  await waitFor(() => strip.scrollLeft > before + 20, 'the glide under way');
  const target = c.offsetLeft + c.offsetWidth - strip.clientWidth;
  expect(strip.scrollLeft).toBeLessThan(target - 20);

  // A scroll-into-view moves the strip mid-glide.
  strip.scrollLeft = before;
  const placed = strip.scrollLeft;
  for (let i = 0; i < 20; i += 1) await raf();
  expect(strip.scrollLeft).toBe(placed);
});
