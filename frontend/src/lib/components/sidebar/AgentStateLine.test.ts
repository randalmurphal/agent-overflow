// The state line asks the wakes store for its row once on mount. Two idle
// agents whose reads answer at different speeds must not keep each other
// reading: the store once reassigned its map on every answer, and a mount
// effect that read that map re-ran on each, so the two rows asked forever.
import { beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import AgentStateLine from './AgentStateLine.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetAgentWakesForTest } from '../../stores/agentWakes.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import type { Thread } from '../../types/models';
import type { ThreadWakeRow } from '../../stores/bindings';

function thread(id: string): Thread {
  return {
    id, title: id, provider: 'claude', model: 'test', projectId: 'project', projectPath: '/tmp/ws',
    workspacePath: '/tmp/ws', createdAt: 0, updatedAt: 0, archived: false, agentPromotedAt: 1,
  };
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

describe('<AgentStateLine>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetAgentWakesForTest();
    replaceAllThreads([thread('agent-0'), thread('agent-1')]);
  });

  it('two idle agents answering at different speeds each read once', async () => {
    const calls: string[] = [];
    setBindingMock('ListThreadWakes', async (id: string) => {
      calls.push(id);
      await sleep(id === 'agent-0' ? 3 : 9);
      return { rows: [], moreReminders: false };
    });
    render(AgentStateLine, { props: { threadId: 'agent-0', indentPx: 0 } });
    render(AgentStateLine, { props: { threadId: 'agent-1', indentPx: 0 } });
    // Long enough for a loop of reads to show (the unfixed store made one
    // every few milliseconds), short enough not to slow the suite.
    await sleep(120);
    expect(calls.sort()).toEqual(['agent-0', 'agent-1']);
  });

  it('renders the soonest loop of a row the read carries', async () => {
    const row: ThreadWakeRow = {
      id: 'l1', kind: 'loop', label: 'board check', state: 'armed', nextAt: Date.now() + 12 * 60_000,
    } as ThreadWakeRow;
    setBindingMock('ListThreadWakes', async () => ({ rows: [row], moreReminders: false }));
    const { findByTestId } = render(AgentStateLine, { props: { threadId: 'agent-0', indentPx: 0 } });
    const line = await findByTestId('thread-row-agent-state');
    expect(line.textContent).toContain('board check');
  });
});
