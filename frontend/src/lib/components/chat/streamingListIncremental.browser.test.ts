// End-to-end proof of the svelte-streamdown incremental-lex patch hunk at
// the DOM layer: while a long list streams through the REAL timeline (real
// pane, real smoother, real ChatMarkdown split), the `<li>` elements of
// already-sealed items must keep their DOM identity and text across later
// appends. That identity is the entire point of the token-reference reuse —
// if a regression re-mints sealed tokens, Svelte re-creates the elements
// and this fails even though the rendered text still looks right (and the
// per-tick cost silently returns to O(list)).
//
// The token-level equivalence corpus lives in
// src/lib/markdown/incrementalLex.test.ts; this file only guards the layer
// unit tests cannot see: tokens → Svelte each-diffing → real DOM.
import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { raf, wait } from '../../../test/helpers/browserFrames';
import {
  SEED_COUNT,
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
  type QuietBottomOptions,
  type SeedProse,
} from '../../../test/helpers/timelineBrowserHarness';
import type { ThreadPane } from '../../stores/thread.svelte';

const QUIET: QuietBottomOptions = { epsilonPx: 2, stableFrames: 24, frameBudget: 600 };

const SEED_PROSE: SeedProse = {
  question: (i) => `Seed question ${i} long enough to wrap across a couple of visual lines at timeline width?`,
  replyLead: (i) => `Seed reply ${i} that wraps across several visual lines so the virtualizer has realistic heights.`,
  replyList: `- seed bullet with **bold**\n- seed bullet with \`code\``,
};

setupTimelineHarness();

const BULLETS = 90;

function buildListText(): string {
  const parts: string[] = ['The full breakdown:', ''];
  for (let i = 0; i < BULLETS; i++) {
    parts.push(
      `- Item ${i}: the \`resolver\` keeps a **steady** cadence while pass ${i} holds the viewport.`,
    );
  }
  return parts.join('\n');
}

describe('streaming list incremental rendering (real timeline × Chromium)', () => {
  it('sealed list items keep DOM identity and text while the tail streams', async () => {
    const threadId = 'thread-incremental-list';
    const { pane, scrollEl } = await mountTimeline(
      threadId,
      seedTimelineItems(threadId, SEED_PROSE),
      QUIET,
    );
    const finalText = buildListText();
    const itemId = 'incremental-list';
    const turnIndex = SEED_COUNT;

    pane.setActiveTurn({ turnId: 'turn-inc', turnIndex, startedAt: Date.now() });
    pane.upsertItem(makeItem({
      id: itemId, threadId, turnIndex, itemIndex: 0,
      kind: 'assistant_text', role: 'assistant', status: 'streaming',
      summary: '', createdAt: turnIndex, updatedAt: turnIndex,
    }));
    // Fat burst: the wire completes quickly, the smoother drains for ~10s.
    for (let off = 0, b = 0; off < finalText.length; off += 900, b++) {
      pane.applyItemDelta({
        threadId, itemId, kind: 'assistant_text',
        delta: finalText.slice(off, off + 900), updatedAt: turnIndex + 1 + b,
      });
      await wait(25);
    }
    pane.applyItemPatch({
      threadId, itemId, kind: 'assistant_text',
      patch: { status: 'completed', summary: finalText, updatedAt: turnIndex + 99 },
    });
    pane.settleTurn({
      turnId: 'turn-inc', turnIndex, startedAt: Date.now() - 1_000,
      completedAt: Date.now(), stopReason: 'end_turn', assistantMessageId: null,
      tokenUsage: null, aborted: false, errorMessage: '',
    });

    // Wait until enough of the list has revealed to pick a sealed item.
    // Scoped to the streaming row — the seed transcript has list rows of
    // its own, and a timeline-wide sweep would land on one of those.
    const row = (): HTMLElement | null =>
      scrollEl.querySelector(`[data-item-id="${itemId}"]`);
    const rowLis = (): NodeListOf<Element> =>
      row()?.querySelectorAll('[data-streamdown-li]') ??
      (document.createDocumentFragment().querySelectorAll('*') as NodeListOf<Element>);
    const liCount = (): number => rowLis().length;
    const deadline = performance.now() + 30_000;
    while (liCount() < 20 && performance.now() < deadline) await raf();
    expect(liCount(), 'reveal must reach the observation point').toBeGreaterThanOrEqual(20);

    const observed = rowLis()[5] as HTMLElement;
    const observedText = observed.textContent;
    expect(observedText).toContain('Item 5');

    // Track identity and text through the rest of the drain. The item is
    // sealed (well behind the live tail), so its element must be the SAME
    // node with the SAME text on every sample until the stream settles.
    let samples = 0;
    while (performance.now() < deadline) {
      const revealed = pane.getItemById(itemId)?.summary.length ?? 0;
      if (revealed >= finalText.length) break;
      const current = rowLis()[5];
      expect(current, `sealed li replaced at sample ${samples}`).toBe(observed);
      expect(observed.textContent, `sealed li text changed at sample ${samples}`).toBe(observedText);
      samples += 1;
      await raf();
      await raf();
    }
    expect(pane.getItemById(itemId)?.summary.length, 'stream must fully reveal').toBe(finalText.length);
    // The identity property is only proven if the drain gave us real samples.
    expect(samples).toBeGreaterThan(50);

    // Settle: the split collapses to the committed instance (a one-time
    // remount by design). The final DOM must carry the complete list.
    await wait(300);
    expect(liCount()).toBe(BULLETS);
    const lis = rowLis();
    expect(lis[0].textContent).toContain('Item 0');
    expect(lis[BULLETS - 1].textContent).toContain(`Item ${BULLETS - 1}`);
  }, 60_000);
});
