// Replays ordered wire mutations through the real event batcher and timeline.
// Command completion must not bypass preceding prose's VISUAL drain, even
// after the provider has finished sending every byte of that prose.
import { expect, it } from 'vitest';
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { applyItemStreamEvent, flushItemEventQueue } from '../../stores/eventsItemStream';
import {
  mountTimeline, seedTimelineItems, setupTimelineHarness, waitForQuietBottom,
} from '../../../test/helpers/timelineBrowserHarness';

setupTimelineHarness();

const QUIET = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
const FINAL_WORDS = 'The prose has now completely finished.';
const SUMMARY = Array.from({ length: 5 }, (_, i) =>
  `Step ${i}: I am checking the command results before moving on. The next action will verify the remaining changes.`,
).join('\n\n') + `\n\n${FINAL_WORDS}`;

it.each(['patch', 'upsert', 'background-sibling', 'prose-status-last', 'mid-drain'] as const)(
  'withholds a completed command throughout preceding prose reveal (%s)',
  { timeout: 30000 },
  async (completion) => {
    const threadId = 'completed-command';
    const seed = seedTimelineItems(threadId, {
      question: (i) => `Question ${i}`,
      replyLead: (i) => `Answer ${i}. A paragraph with enough text to exercise the real timeline.`,
      replyList: '- First point\n- Second point',
    });
    if (completion === 'background-sibling') seed.push(makeItem({
      id: 'launch', threadId, turnIndex: 99, itemIndex: 0, kind: 'tool_call',
      toolName: 'command_execution', status: 'running', summary: 'git status', isBackground: true,
    }));
    const { pane, scrollEl } = await mountTimeline(threadId, seed, QUIET, { provider: 'codex' });
    const prose = makeItem({ id: 'prose', threadId, turnIndex: 100, itemIndex: 0,
      kind: 'assistant_text', role: 'assistant', status: 'streaming', summary: '' });
    applyItemStreamEvent({ action: 'upsert', threadId, item: prose });
    flushItemEventQueue();
    await tick();

    // Every prose byte arrives before the command; there are no resumed or
    // late deltas. Exercise both same-frame delivery and completion after
    // the prose has visibly started revealing across several frames.
    applyItemStreamEvent({ action: 'delta', threadId, itemId: prose.id,
      kind: prose.kind, delta: SUMMARY, updatedAt: 2 });
    if (completion === 'mid-drain') {
      flushItemEventQueue();
      await waitFor(() => (scrollEl.querySelector('[data-item-id="prose"]')?.textContent?.length ?? 0) > 30,
        'prose visibly revealing before command starts');
    }
    const finishProse = () => applyItemStreamEvent({ action: 'patch', threadId,
      itemId: prose.id, kind: prose.kind,
      patch: { status: 'completed', summary: SUMMARY, updatedAt: 4 } });
    if (completion !== 'prose-status-last') finishProse();

    const command = makeItem({ id: 'command', threadId, turnIndex: 100, itemIndex: 1,
      kind: 'tool_call', toolName: 'command_execution', status: 'running', summary: 'git status' });
    if (completion === 'background-sibling') {
      applyItemStreamEvent({ action: 'upsert', threadId, item: {
        ...command, kind: 'tool_completion', completionOf: 'launch', status: 'completed',
      } });
    } else {
      applyItemStreamEvent({ action: 'upsert', threadId, item: command });
      if (completion === 'upsert') {
        applyItemStreamEvent({ action: 'upsert', threadId,
          item: { ...command, status: 'completed', updatedAt: 3 } });
      } else {
        if (completion === 'mid-drain') {
          flushItemEventQueue();
          for (let frame = 0; frame < 5; frame++) await raf();
          expect(pane.getItemById(prose.id)?.summary).not.toBe(SUMMARY);
          expect(scrollEl.querySelector('[data-item-id="command"]')).toBeNull();
        }
        applyItemStreamEvent({ action: 'patch', threadId, itemId: command.id, kind: command.kind,
          patch: { status: 'completed', updatedAt: 3 } });
      }
    }
    if (completion === 'prose-status-last') finishProse();
    flushItemEventQueue();
    await tick();

    let drainFrames = 0;
    for (let frame = 0; frame < 900; frame++) {
      await raf();
      const proseDOM = scrollEl.querySelector('[data-item-id="prose"]');
      if (proseDOM?.textContent?.includes(FINAL_WORDS)) break;
      drainFrames++;
      expect(scrollEl.querySelector('[data-item-id="command"]'),
        'command completion must wait for the preceding prose DOM to finish revealing').toBeNull();
    }
    expect(drainFrames, 'fixture must exercise a real multi-frame visual drain').toBeGreaterThan(10);
    expect(pane.getItemById(prose.id)?.summary).toBe(SUMMARY);
    expect(scrollEl.querySelector('[data-item-id="prose"]')?.textContent).toContain(FINAL_WORDS);
    await waitFor(() => scrollEl.querySelector('[data-item-id="command"]') !== null,
      'completed command released after prose');
    // No disclosure clicks: the command output stays collapsed.
    await waitForQuietBottom(scrollEl, 'command release settles', QUIET);
  },
);
