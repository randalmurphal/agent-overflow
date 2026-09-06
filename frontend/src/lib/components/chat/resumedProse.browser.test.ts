import { expect, it } from 'vitest';
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import {
  mountTimeline, seedTimelineItems, setupTimelineHarness, waitForQuietBottom,
} from '../../../test/helpers/timelineBrowserHarness';

setupTimelineHarness();

const quiet = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };

it.each(['codex', 'claude'] as const)('%s keeps a released compact command mounted when earlier prose resumes', { timeout: 30000 }, async (provider) => {
  const threadId = 'resumed-prose';
  const { pane, scrollEl } = await mountTimeline(threadId, seedTimelineItems(threadId, {
    question: (i) => `Question ${i}`,
    replyLead: (i) => `Answer ${i}. A paragraph with enough text to exercise the real timeline.`,
    replyList: '- First point\n- Second point',
  }), quiet, { provider });
  const prose = makeItem({ id: 'prose', threadId, turnIndex: 100, itemIndex: 0,
    kind: 'assistant_text', role: 'assistant', status: 'streaming', summary: '' });
  pane.applyProviderItemUpserts([prose]);
  const append = (delta: string) => pane.applyItemDelta({
    threadId, itemId: prose.id, kind: prose.kind, delta, updatedAt: Date.now(),
  });
  append('I am checking the command results. ');
  await waitForQuietBottom(scrollEl, 'initial prose', quiet);
  pane.applyProviderItemUpserts([makeItem({
    id: 'command', threadId, turnIndex: 100, itemIndex: 1,
    kind: 'tool_call', toolName: provider === 'codex' ? 'commandExecution' : 'Bash',
    status: 'running', summary: 'git status',
  })]);
  await waitFor(() => scrollEl.querySelector('[data-item-id="command"]') !== null,
    'compact command appears');
  await waitForQuietBottom(scrollEl, 'command visible', quiet);
  const command = scrollEl.querySelector('[data-item-id="command"]');
  // No disclosure click: the command output stays collapsed throughout.
  for (let burst = 0; burst < 3; burst++) {
    append('The earlier message continues while the command is visible. '.repeat(4));
    if (burst === 1) pane.applyItemPatch({ threadId, itemId: 'command', kind: 'tool_call',
      patch: { status: 'completed', updatedAt: Date.now() } });
    await tick();
    let previousTop = scrollEl.scrollTop;
    for (let frame = 0; frame < 90; frame++) {
      await raf();
      expect(scrollEl.querySelector('[data-item-id="command"]'),
        'resuming an earlier reveal must not retract an already visible command').toBe(command);
      expect(scrollEl.scrollTop, 'append-only prose must not pull bottom-follow upward')
        .toBeGreaterThanOrEqual(previousTop - 1);
      previousTop = scrollEl.scrollTop;
    }
    await waitForQuietBottom(scrollEl, `burst ${burst} settles`, quiet);
  }
});
