import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import {
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import { waitFor } from '../../../test/helpers/browserFrames';

setupTimelineHarness();

const QUIET: QuietBottomOptions = { epsilonPx: 2, stableFrames: 8, frameBudget: 360 };

describe('forked command result presentation', () => {
  it('stays top-level Markdown when the preceding activity run collapses', async () => {
    const threadId = 'thread-agent-result';
    const items = seedTimelineItems(threadId, {
      question: (i) => `Question ${i}`,
      replyLead: (i) => `Reply ${i}`,
      replyList: '- one\n- two',
    });
    items.push(
      makeItem({
        id: 'review-tool', threadId, turnIndex: 50, itemIndex: 0,
        kind: 'tool_call', toolName: 'Skill', summary: 'Skill: code-review',
        meta: JSON.stringify({
          toolName: 'Skill', input: { skill: 'code-review' },
          skillFork: { agentId: 'agent-1', commandName: 'code-review' },
          directCommandFork: true, directCommandResult: true,
        }),
      }),
      makeItem({
        id: 'command-result:review', threadId, turnIndex: 50, itemIndex: 1,
        kind: 'command_result', role: 'system', summary: '## Findings\n\nNo issues found.',
        meta: JSON.stringify({
          kind: 'command_result', preview: '## Findings\n\nNo issues found.',
          agentResult: {
            launchId: 'review-tool', sourceKind: 'skill', sourceName: 'code-review',
          },
        }),
      }),
    );

    const { scrollEl } = await mountTimeline(threadId, items, QUIET);
    const run = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;
    const result = scrollEl.querySelector('[data-agent-result="true"]') as HTMLElement;
    const body = scrollEl.querySelector('[data-testid="agent-result-body"]') as HTMLElement;
    expect(run).not.toBeNull();
    expect(result).not.toBeNull();
    expect(run.contains(result)).toBe(false);
    expect(body.querySelector('h2')?.textContent).toBe('Findings');
    expect(getComputedStyle(body).overflowY).toBe('visible');

    const toggle = run.querySelector('[data-testid="activity-run-header"]') as HTMLButtonElement;
    toggle.click();
    await waitFor(() => run.dataset.collapsed === 'true', 'activity run to collapse');
    expect(result.isConnected).toBe(true);
    expect(result.textContent).toContain('No issues found.');
  });
});
