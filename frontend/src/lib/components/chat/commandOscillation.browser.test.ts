// An independently completed command behind an open message must not flicker
// as the message's reveal buffer repeatedly catches up between input bursts.
import { expect, it } from 'vitest';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { applyItemStreamEvent, flushItemEventQueue } from '../../stores/eventsItemStream';
import {
  mountTimeline, seedTimelineItems, setupTimelineHarness, waitForQuietBottom,
} from '../../../test/helpers/timelineBrowserHarness';

setupTimelineHarness();
const quiet = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };

it('does not reveal and retract a completion during gaps in an unfinished message', { timeout: 30000 }, async () => {
  const threadId = 'burst-completion';
  const { pane, scrollEl } = await mountTimeline(threadId, seedTimelineItems(threadId, {
    question: i => `Question ${i}`, replyLead: i => `Answer ${i}. Preceding context.`, replyList: '- One\n- Two',
  }), quiet, { provider: 'codex' });
  const prose = makeItem({ id: 'prose', threadId, turnIndex: 100, itemIndex: 0,
    status: 'streaming', summary: '' });
  applyItemStreamEvent({ action: 'upsert', threadId, item: prose });
  applyItemStreamEvent({ action: 'delta', threadId, itemId: prose.id, kind: prose.kind,
    delta: 'I am checking the result. ', updatedAt: 2 });
  flushItemEventQueue();
  await waitForQuietBottom(scrollEl, 'initial burst drained', quiet);
  applyItemStreamEvent({ action: 'upsert', threadId, item: makeItem({
    id: 'command', threadId, turnIndex: 100, itemIndex: 1, kind: 'tool_completion',
    toolName: 'command_execution', status: 'completed', summary: 'git status',
  }) });
  flushItemEventQueue();
  const samples: { frame: number; shown: boolean; top: number; height: number }[] = [];
  for (let frame = 0; frame < 240; frame++) {
    if (frame === 30 || frame === 90 || frame === 150) {
      applyItemStreamEvent({ action: 'delta', threadId, itemId: prose.id, kind: prose.kind,
        delta: 'The next burst of the same message is now being displayed. ', updatedAt: 3 + frame });
      flushItemEventQueue();
    }
    await raf();
    samples.push({ frame, shown: scrollEl.querySelector('[data-item-id="command"]') !== null,
      top: scrollEl.scrollTop, height: scrollEl.scrollHeight });
  }
  const edges = samples.filter((s, i) => i === 0 || s.shown !== samples[i - 1].shown);
  expect(edges, JSON.stringify(edges)).toEqual([expect.objectContaining({ shown: false })]);
  expect(pane.getItemById(prose.id)?.status).toBe('streaming');
  applyItemStreamEvent({ action: 'patch', threadId, itemId: prose.id, kind: prose.kind,
    patch: { status: 'completed', updatedAt: 300 } });
  flushItemEventQueue();
  await waitFor(() => scrollEl.querySelector('[data-item-id="command"]') !== null, 'completion released once message ends');
  await waitForQuietBottom(scrollEl, 'completion settles', quiet);
  const completedRow = scrollEl.querySelector('[data-item-id="command"]');
  for (let frame = 0; frame < 60; frame++) {
    await raf();
    expect(scrollEl.querySelector('[data-item-id="command"]')).toBe(completedRow);
  }
});

it.each([360, 800, 1400])('does not retract a completed command below tall prose at width %i', { timeout: 30000 }, async (width) => {
  const threadId = 'oscillation';
  const seed = seedTimelineItems(threadId, {
    question: i => `Question ${i}`,
    replyLead: i => `Answer ${i}. Some preceding context for a realistically windowed timeline.`,
    replyList: '- One\n- Two',
  });
  seed.push(makeItem({ id: 'long-prose', threadId, turnIndex: 100, itemIndex: 0,
    summary: Array.from({ length: 45 }, (_, i) => `Paragraph ${i}. The result of the investigation is a useful finding about the implementation and the changes that need to be made.`).join('\n\n') }));
  const { pane, host, scrollEl } = await mountTimeline(threadId, seed, quiet, { provider: 'codex' });
  host.style.width = `${width}px`;
  host.style.setProperty('--composer-height', '230px');
  await waitForQuietBottom(scrollEl, 'viewport adjusted', quiet);
  const command = makeItem({ id: 'command', threadId, turnIndex: 100, itemIndex: 1,
    kind: 'tool_call', toolName: 'command_execution', status: 'running', summary: 'git status' });
  pane.applyProviderItemUpserts([command]);
  const samples: { frame: number; top: number; height: number; present: boolean; visible: boolean }[] = [];
  for (let frame = 0; frame < 180; frame++) {
    if (frame === 5) pane.applyProviderItemUpserts([{ ...command, status: 'completed' }]);
    await raf();
    const row = scrollEl.querySelector('[data-item-id="command"]');
    samples.push({ frame, top: scrollEl.scrollTop, height: scrollEl.scrollHeight,
      present: row !== null, visible: row !== null && getComputedStyle(row).visibility !== 'hidden' });
  }
  const regressions = samples.filter((sample, i) => i > 0 && sample.top < samples[i - 1].top - 2);
  const retractions = samples.filter((sample, i) => i > 0 && samples[i - 1].visible && !sample.visible);
  expect({ regressions, retractions }, JSON.stringify(samples)).toEqual({ regressions: [], retractions: [] });
});
