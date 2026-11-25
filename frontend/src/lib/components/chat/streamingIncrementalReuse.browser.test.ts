// End-to-end proof of the incremental-lex fast paths at
// the DOM layer: while a long list or table streams through the REAL
// timeline (real pane, real smoother, real ChatMarkdown split), the
// elements of already-sealed items/rows must keep their DOM identity and
// text across later appends. That identity is the entire point of the
// token-reference reuse — if a regression re-mints sealed tokens, Svelte
// re-creates the elements and this fails even though the rendered text
// still looks right (and the per-tick cost silently returns to O(block)).
//
// The token-level equivalence corpus lives in
// src/lib/markdown/incrementalLex.test.ts; this file only guards the layer
// unit tests cannot see: tokens → Svelte each-diffing → real DOM.
import { describe, expect, it, onTestFinished } from 'vitest';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { raf, wait } from '../../../test/helpers/browserFrames';
import { revealWithoutWaitingForFrames } from '../../../test/helpers/revealPace';
import { captureResizeObserverLoopErrors } from '../../../test/helpers/resizeObserverLoopErrors';
import {
  SEED_COUNT,
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
  type QuietBottomOptions,
  type SeedProse,
} from '../../../test/helpers/timelineBrowserHarness';

const QUIET: QuietBottomOptions = { epsilonPx: 2, stableFrames: 24, frameBudget: 600 };

const SEED_PROSE: SeedProse = {
  question: (i) => `Seed question ${i} long enough to wrap across a couple of visual lines at timeline width?`,
  replyLead: (i) => `Seed reply ${i} that wraps across several visual lines so the virtualizer has realistic heights.`,
  replyList: `- seed bullet with **bold**\n- seed bullet with \`code\``,
};

setupTimelineHarness();

const UNITS = 90;

const listText = (): string => {
  const parts: string[] = ['The full breakdown:', ''];
  for (let i = 0; i < UNITS; i++) {
    parts.push(
      `- Item ${i}: the \`resolver\` keeps a **steady** cadence while pass ${i} holds the viewport.`,
    );
  }
  return parts.join('\n');
};

const tableText = (): string => {
  const parts: string[] = ['| Item | Detail | Pass |', '| --- | :---: | ---: |'];
  for (let i = 0; i < UNITS; i++) {
    parts.push(`| Item ${i} | the \`resolver\` keeps a **steady** cadence | pass ${i} |`);
  }
  return parts.join('\n');
};

interface ReuseCase {
  name: string;
  itemId: string;
  finalText: string;
  /** row-scoped selector for the repeated sealed element */
  unitSelector: string;
  /** index of the sealed unit to observe and text that must sit inside it */
  observeIndex: number;
  observeText: string;
  /** unit count expected after settle (tables add a header row) */
  settledCount: number;
}

const CASES: ReuseCase[] = [
  {
    name: 'sealed list items keep DOM identity and text while the tail streams',
    itemId: 'incremental-list',
    finalText: listText(),
    unitSelector: '[data-streamdown-li]',
    observeIndex: 5,
    observeText: 'Item 5',
    settledCount: UNITS,
  },
  {
    name: 'sealed table rows keep DOM identity and text while the tail streams',
    itemId: 'incremental-table',
    finalText: tableText(),
    unitSelector: '[data-streamdown-tr]',
    // Index 0 is the header <tr>; observe a sealed body row behind the tail.
    observeIndex: 6,
    observeText: 'Item 5',
    settledCount: UNITS + 1,
  },
];

describe('streaming incremental token reuse (real timeline × Chromium)', () => {
  for (const testCase of CASES) {
    it(testCase.name, async () => {
      const resizeObserverErrors = captureResizeObserverLoopErrors();
      onTestFinished(resizeObserverErrors.stop);
      const { itemId, finalText } = testCase;
      const threadId = `thread-${itemId}`;
      const reveal = revealWithoutWaitingForFrames();
      const { pane, scrollEl } = await mountTimeline(
        threadId,
        seedTimelineItems(threadId, SEED_PROSE),
        QUIET,
      );
      const turnIndex = SEED_COUNT;

      pane.setActiveTurn({ turnId: 'turn-inc', turnIndex, startedAt: Date.now() });
      pane.upsertItem(makeItem({
        id: itemId, threadId, turnIndex, itemIndex: 0,
        kind: 'assistant_text', role: 'assistant', status: 'streaming',
        summary: '', createdAt: turnIndex, updatedAt: turnIndex,
      }));
      // Track identity and text through the drain from the moment the
      // observation threshold is reached. Registering before the burst avoids
      // racing the unthrottled MessageChannel reveal clock against the
      // wall-clock wait(25) between wire chunks. The unit is sealed (well
      // behind the live tail), so its element must be the SAME node with the
      // SAME text after every reveal tick until the stream settles.
      const row = (): HTMLElement | null =>
        scrollEl.querySelector(`[data-item-id="${itemId}"]`);
      const units = (): NodeListOf<Element> =>
        row()?.querySelectorAll(testCase.unitSelector) ??
        (document.createDocumentFragment().querySelectorAll('*') as NodeListOf<Element>);
      const unitCount = (): number => units().length;
      let observed: HTMLElement | null = null;
      let observedText: string | null = null;
      let samples = 0;
      const changes: string[] = [];
      reveal.onTick(() => {
        if ((pane.getItemById(itemId)?.summary.length ?? 0) >= finalText.length) return;
        if (observed === null) {
          if (unitCount() < 20) return;
          observed = units()[testCase.observeIndex] as HTMLElement;
          observedText = observed.textContent;
          return;
        }
        if (units()[testCase.observeIndex] !== observed) changes.push(`sealed unit replaced at sample ${samples}`);
        else if (observed.textContent !== observedText) changes.push(`sealed unit text changed at sample ${samples}`);
        samples += 1;
      });

      // Fat burst: the wire completes quickly, the smoother drains for many ticks.
      for (let off = 0, b = 0; off < finalText.length; off += 900, b++) {
        pane.applyItemDelta({
          threadId, itemId, kind: 'assistant_text',
          delta: finalText.slice(off, off + 900), updatedAt: turnIndex + 1 + b,
        });
        await wait(25);
      }
      pane.applyItemPatch({
        threadId, itemId, kind: 'assistant_text',
        patch: { rev: 0, status: 'completed', summary: finalText, updatedAt: turnIndex + 99 },
      });
      pane.settleTurn({
        turnId: 'turn-inc', turnIndex, startedAt: Date.now() - 1_000,
        completedAt: Date.now(), stopReason: 'end_turn', assistantMessageId: null,
        tokenUsage: null, aborted: false, errorMessage: '',
      });

      const deadline = performance.now() + 30_000;
      while (performance.now() < deadline && (pane.getItemById(itemId)?.summary.length ?? 0) < finalText.length) await raf();
      expect(observed, 'reveal must reach the observation point').not.toBeNull();
      expect(observedText).toContain(testCase.observeText);
      expect(changes.slice(0, 1)).toEqual([]);
      expect(
        pane.getItemById(itemId)?.summary.length,
        'stream must fully reveal',
      ).toBe(finalText.length);
      // The identity property is only proven if the drain gave us real samples.
      expect(samples).toBeGreaterThan(50);

      // Settle: the split collapses to the committed instance (a one-time
      // remount by design). The final DOM must carry the complete content.
      await wait(300);
      expect(unitCount()).toBe(testCase.settledCount);
      const settled = units();
      expect(settled[testCase.settledCount - UNITS].textContent).toContain('Item 0');
      expect(settled[testCase.settledCount - 1].textContent).toContain(`Item ${UNITS - 1}`);
      expect(resizeObserverErrors.messages).toEqual([]);
    });
  }
});
