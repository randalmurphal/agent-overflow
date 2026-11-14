import { afterEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf } from '../../../test/helpers/browserFrames';
import {
  distanceToBottom, mountTimeline, seedTimelineItems, setupTimelineHarness, waitForQuietBottom,
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
    it('glides the sent message in when its row arrives, and a repeated row changes nothing', async () => {
      const threadId = `ack-${provider}`;
      const items = seedTimelineItems(threadId, PROSE);
      const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET, { provider });
      setUiRenderTraceEnabled(true);
      // The composer draws nothing for the send: the timeline is unchanged
      // until the backend's row arrives on the wire.
      pane.awaitSendConfirmation('repro-send');
      await tick();
      await raf();
      expect(pane.items).toHaveLength(items.length);
      expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(1);
      const item = makeItem({
        id: 'user:canonical-repro', threadId, kind: 'user_text', role: 'user', status: 'completed',
        turnIndex: items.at(-1)!.turnIndex + 1, itemIndex: 0,
        summary: 'First line of the sent message.\nSecond line of the sent message.\nThird line of the sent message.',
        meta: JSON.stringify({ sendId: 'repro-send' }), createdAt: 100, updatedAt: 100,
      });
      const seq = getUiRenderTraceRecords().at(-1)?.seq ?? 0;
      const beforeTop = scrollEl.scrollTop;
      pane.applyProviderItemUpserts([item]);
      expect(pane.sendAwaitingConfirmation).toBe(false);
      await tick();
      await raf();
      await raf();
      const row = scrollEl.querySelector(`[data-item-id="${item.id}"]`);
      expect(row).not.toBeNull();
      // The row opened a gap that closes over frames, not in one write.
      expect(distanceToBottom(scrollEl)).toBeGreaterThan(20);
      const samples = [beforeTop];
      for (let i = 0; i < 75; i++) {
        await raf();
        samples.push(scrollEl.scrollTop);
      }
      for (let i = 1; i < samples.length; i++) expect(samples[i]).toBeGreaterThanOrEqual(samples[i - 1] - 1);
      await waitForQuietBottom(scrollEl, 'sent message settled', QUIET);
      const shrink = getUiRenderTraceRecords().filter(r => r.seq > seq
        && r.label === 'scroll.contentRO' && (r.data as { delta: number }).delta < 0);
      expect(shrink).toEqual([]);

      const settledTop = scrollEl.scrollTop;
      const settledHeight = scrollEl.scrollHeight;
      const settledSeq = getUiRenderTraceRecords().at(-1)?.seq ?? 0;
      expect(pane.applyProviderItemUpserts([{ ...item }])).toBeNull();
      const restated = { ...item, updatedAt: 101 };
      pane.applyProviderItemUpserts([restated]);
      await tick();
      for (let i = 0; i < 12; i++) {
        await raf();
        expect(scrollEl.scrollTop).toBe(settledTop);
        expect(scrollEl.scrollHeight).toBe(settledHeight);
      }
      expect(scrollEl.querySelector(`[data-item-id="${item.id}"]`)).toBe(row);
      expect(getUiRenderTraceRecords().filter(r => r.seq > settledSeq && r.label === 'scroll.spring.chase')).toEqual([]);
      expect(pane.items.filter(i => i.kind === 'user_text' && i.turnIndex === item.turnIndex)).toEqual([restated]);
    });
  });
}
