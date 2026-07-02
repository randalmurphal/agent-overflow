// The MessageTimeline ↔ patched-virtua wiring seam: the stick controller's
// onBeforeScrollTopWrite option must reach the bound VirtualizerHandle's
// markProgrammaticScroll() (patches/virtua@0.49.1.patch) so virtua never
// classifies a pin write as a user scroll-down and drops its above-viewport
// buffer — the streaming settle-flicker churn
// (docs/architecture/settle-flicker-analysis.md). The controller side (hook
// precedes every programmatic write) is proven in
// utils/scroll/index.svelte.test.ts, and the virtua side (marking prevents the
// buffer drop) in src/test/integration/virtua-patch-buffer-retention
// .browser.test.ts. This test closes the seam between them: delete
// MessageTimeline's onBeforeScrollTopWrite wiring and only this fails.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('virtua/svelte', async () => ({
  Virtualizer: (await import('../../../test/mocks/StubVirtualizer.svelte')).default,
}));

import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { clearThreadScrollSnapshotsForTest } from '../../utils/threadScrollSnapshots';
import { clearThreadVirtuaSizeCacheForTest } from '../../utils/threadVirtuaSizeCache';
import {
  resetVirtuaMarkCalls,
  virtuaMarkCalls,
} from '../../../test/mocks/virtuaMarkRecorder';
import MessageTimeline from './MessageTimeline.svelte';

beforeEach(async () => {
  resetBindingMocks();
  clearThreadScrollSnapshotsForTest();
  clearThreadVirtuaSizeCacheForTest();
  resetVirtuaMarkCalls();
  setBindingMock('GetSettings', async () => null);
  await loadSettings();
});

describe('MessageTimeline virtua manual-scroll marking', () => {
  it('marks virtua as programmatically scrolled before a controller pin write lands', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'a', summary: 'a' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'b' }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    await tick();
    await tick();
    await waitFor(() => {
      expect(container.querySelector('[data-testid="message-timeline-node"]')).not.toBeNull();
      expect(pane.scrollController).not.toBeNull();
    });

    const scrollEl = container.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    expect(scrollEl).not.toBeNull();
    // 10px shy of the 400px bottom target so the pin performs a real write
    // (happy-dom's all-zero geometry would make target === scrollTop === 0,
    // and an equal-value pin skips the write and with it the hook).
    let scrollTop = 390;
    Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => 1000 });
    Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => 600 });
    Object.defineProperty(scrollEl, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (value: number) => {
        scrollTop = value;
      },
    });

    const marksBefore = virtuaMarkCalls();
    // Out-of-content geometry change (the 'content' observation kind):
    // fresh mount is sticky and unescaped, so the controller sync-pins to
    // the bottom target — and must mark virtua first.
    pane.scrollController?.observe('content');

    expect(scrollTop).toBe(400);
    expect(virtuaMarkCalls()).toBeGreaterThan(marksBefore);
  });
});
