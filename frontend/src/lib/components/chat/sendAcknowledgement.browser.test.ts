import { afterEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf } from '../../../test/helpers/browserFrames';
import {
  mountTimeline, seedTimelineItems, setupTimelineHarness, waitForQuietBottom,
} from '../../../test/helpers/timelineBrowserHarness';
import {
  clearUiRenderTrace, getUiRenderTraceRecords, setUiRenderTraceEnabled,
} from '../../utils/uiRenderTrace';

setupTimelineHarness();
afterEach(() => { clearUiRenderTrace(); setUiRenderTraceEnabled(false); });

const QUIET = { epsilonPx: 1, stableFrames: 12, frameBudget: 480 };
const PROSE = {
  question: (i: number) => `Question ${i}: check rendering.`,
  replyLead: (i: number) => `Reply ${i}: enough ordinary prose to create a scrollable transcript and exercise real windowing.`,
  replyList: '- one\n- two\n- three',
};

for (const provider of ['codex', 'claude'] as const) {
  describe(`${provider} send confirmation`, () => {
    for (const timing of ['during', 'after'] as const) {
      it(`preserves the row and geometry when confirmation arrives ${timing} the send glide`, async () => {
        const threadId = `ack-${provider}-${timing}`;
        const items = seedTimelineItems(threadId, PROSE);
        const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET, { provider });
        setUiRenderTraceEnabled(true);
        const item = makeItem({
          id: 'optimistic:repro-send', threadId, kind: 'user_text', role: 'user', status: 'completed',
          turnIndex: items.at(-1)!.turnIndex + 1, itemIndex: 0,
          summary: 'First line of the sent message.\nSecond line of the sent message.\nThird line of the sent message.',
          meta: JSON.stringify({ sendId: 'repro-send' }), createdAt: 100, updatedAt: 100,
        });
        pane.trackOptimisticItem(item.id);
        pane.armStructuralSpring();
        pane.upsertItems([item]);
        await tick();
        await raf();
        await raf();
        if (timing === 'after') await waitForQuietBottom(scrollEl, 'optimistic send settled', QUIET);
        else expect(scrollEl.scrollHeight - scrollEl.clientHeight - scrollEl.scrollTop).toBeGreaterThan(20);
        const beforeTop = scrollEl.scrollTop;
        const beforeHeight = scrollEl.scrollHeight;
        const row = scrollEl.querySelector(`[data-item-id="${item.id}"]`);
        expect(row).not.toBeNull();
        const seq = getUiRenderTraceRecords().at(-1)?.seq ?? 0;
        const confirmed = { ...item, id: 'user:canonical-repro', updatedAt: 101 };
        pane.applyProviderItemUpserts([confirmed]);
        await tick();
        expect(scrollEl.querySelector(`[data-item-id="${confirmed.id}"]`)).toBe(row);
        const samples = [beforeTop];
        for (let i = 0; i < 75; i++) {
          await raf();
          samples.push(scrollEl.scrollTop);
          expect(scrollEl.scrollHeight).toBe(beforeHeight);
        }
        for (let i = 1; i < samples.length; i++) expect(samples[i]).toBeGreaterThanOrEqual(samples[i - 1] - 1);
        const records = getUiRenderTraceRecords().filter(r => r.seq > seq);
        const shrink = records.filter(r => r.label === 'scroll.contentRO' && (r.data as { delta: number }).delta < 0);
        expect(shrink).toEqual([]);
        if (timing === 'after') {
          expect(Math.max(...samples) - Math.min(...samples)).toBeLessThanOrEqual(1);
          expect(records.filter(r => r.label === 'scroll.spring.chase')).toEqual([]);
        }
        expect(pane.items.filter(i => i.kind === 'user_text' && i.turnIndex === item.turnIndex)).toEqual([confirmed]);
        const settledTop = scrollEl.scrollTop;
        pane.applyProviderItemUpserts([confirmed]);
        await tick();
        await raf();
        expect(scrollEl.querySelector(`[data-item-id="${confirmed.id}"]`)).toBe(row);
        expect(scrollEl.scrollTop).toBe(settledTop);
      });
    }

    it('moves the same row before its response when the backend corrects the predicted turn', async () => {
      const threadId = `ack-move-${provider}`;
      const items = seedTimelineItems(threadId, PROSE);
      const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET, { provider });
      const predicted = makeItem({
        id: 'optimistic:moved', threadId, kind: 'user_text', role: 'user', status: 'completed',
        turnIndex: 46, itemIndex: 0, summary: 'Message awaiting authoritative placement.',
        meta: JSON.stringify({ sendId: 'moved' }),
      });
      pane.trackOptimisticItem(predicted.id);
      pane.armStructuralSpring();
      pane.upsertItems([predicted]);
      await tick();
      await waitForQuietBottom(scrollEl, 'provisional row settled', QUIET);
      const row = scrollEl.querySelector(`[data-item-id="${predicted.id}"]`)!;
      expect(row).not.toBeNull();
      const height = row.getBoundingClientRect().height;
      const response = makeItem({ id: 'response', threadId, turnIndex: 45, itemIndex: 1,
        kind: 'assistant_text', role: 'assistant', status: 'completed', summary: 'Authoritative response.' });
      pane.applyProviderItemUpserts([response]);
      await tick();
      await waitForQuietBottom(scrollEl, 'response settled', QUIET);
      const confirmed = { ...predicted, id: 'user:moved', turnIndex: 45 };
      pane.applyProviderItemUpserts([confirmed]);
      await tick();
      await waitForQuietBottom(scrollEl, 'confirmed placement settled', QUIET);
      expect(pane.items.slice(-2)).toEqual([confirmed, response]);
      expect(scrollEl.querySelector('[data-item-id="user:moved"]')).toBe(row);
      expect(row.getBoundingClientRect().height).toBe(height);
      const responseRow = scrollEl.querySelector('[data-item-id="response"]')!;
      expect(row.getBoundingClientRect().bottom).toBeLessThanOrEqual(responseRow.getBoundingClientRect().top);
    });
  });
}
