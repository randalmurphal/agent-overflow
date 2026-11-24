import { beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ToolCallCard from './ToolCallCard.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { codexSubagentDisplayLabel } from '../../utils/subagentLaunch';

beforeEach(() => {
  setBindingMock('GetPayloadPreview', async () => ({
    data: '', totalSize: 0, nextOffset: 0, isComplete: true,
  }));
  setBindingMock('GetPayloadData', async () => ({ data: '' }));
});

describe('codexSubagentDisplayLabel', () => {
  it('renders name and role once when they are the same word', () => {
    expect(codexSubagentDisplayLabel('reviewer', 'reviewer', 'Agent')).toBe('reviewer');
    expect(codexSubagentDisplayLabel('Reviewer', 'reviewer', 'Agent')).toBe('Reviewer');
  });

  it('keeps the bracketed role when it adds information', () => {
    expect(codexSubagentDisplayLabel('scout', 'reviewer', 'Agent')).toBe('scout [reviewer]');
    expect(codexSubagentDisplayLabel('', 'reviewer', 'Agent')).toBe('Agent [reviewer]');
    expect(codexSubagentDisplayLabel('scout', '', 'Agent')).toBe('scout');
  });
});

describe('<CollabToolRow> immutable spawn and standalone activity', () => {
  it('ignores legacy appended operations and status metadata on a spawn row', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const item = makeItem({
      id: 'launch-1',
      kind: 'tool_call',
      status: 'completed',
      toolName: 'collab_agent',
      isBackground: true,
      meta: JSON.stringify({
        input: {
          tool: 'spawn_agent',
          taskName: '/root/reviewer',
          receiverThreadIds: ['child-1'],
        },
        codex_child_terminal_statuses: { 'child-1': 'completed' },
        codex_collab_delivered_at: 1700,
        codex_collab_interactions: [
          { id: 'old', kind: 'interacted', tool: 'send_message', at: 1 },
        ],
      }),
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });
    const text = getByTestId('collab-tool-row').textContent ?? '';
    expect(text).toContain('Spawned reviewer');
    // The legacy meta is carried, not shown: neither its raw values nor the
    // labels the removed spawn-card renderer made of them ("message sent" for
    // the interaction, "delivered" for the card state).
    for (const legacy of ['completed', 'delivered', '1700', 'interacted', 'send_message', 'message sent']) {
      expect(text, legacy).not.toContain(legacy);
    }
  });

  it('renders a spawn row the same, with no indicator, whatever its stored liveness bit says', async () => {
    // A Claude background launch's dots turn off when the store settles it;
    // a Codex spawn row is `completed` and never had them.
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const spawn = (active: boolean) => makeItem({
      id: 'launch-1',
      kind: 'tool_call',
      status: 'completed',
      toolName: 'collab_agent',
      isBackground: true,
      meta: JSON.stringify({
        live_background_active: active,
        input: { tool: 'spawn_agent', taskName: '/root/reviewer', receiverThreadIds: ['child-1'] },
      }),
    });
    const { container, rerender, queryByTestId } = render(ToolCallCard, { props: { pane, item: spawn(true) } });
    const before = container.innerHTML;
    expect(queryByTestId('indicator')).toBeNull();

    await rerender({ pane, item: spawn(false) });

    expect(queryByTestId('indicator')).toBeNull();
    expect(container.innerHTML).toBe(before);
  });

  it('renders outbound and inbound communication as independent activity rows', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const sent = makeItem({
      id: 'send-1',
      kind: 'tool_call',
      status: 'completed',
      toolName: 'send_input',
      meta: JSON.stringify({
        input: {
          tool: 'send_input',
          activityKind: 'interacted',
          activityTool: 'followup_task',
          agentPath: '/root/reviewer',
          receiverThreadIds: ['child-1'],
        },
      }),
    });
    const progress = makeItem({
      id: 'progress-1',
      kind: 'tool_call',
      status: 'completed',
      toolName: 'send_input',
      meta: JSON.stringify({
        input: {
          tool: 'send_input',
          activityKind: 'progress',
          message: 'Running focused tests',
          target: '/root/reviewer',
        },
      }),
    });

    const sentView = render(ToolCallCard, { props: { pane, item: sent } });
    expect(sentView.getByTestId('collab-tool-row').textContent).toContain('Sent follow-up to reviewer');
    sentView.unmount();

    const progressView = render(ToolCallCard, { props: { pane, item: progress } });
    expect(progressView.getByTestId('collab-tool-row').textContent).toContain('Progress from reviewer');
    expect(progressView.getByText(/Running focused tests/)).toBeInTheDocument();
  });
});
