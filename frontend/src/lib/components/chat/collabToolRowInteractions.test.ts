import { beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ToolCallCard from './ToolCallCard.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { codexSubagentDisplayLabel } from '../../utils/subagentLaunch';

beforeEach(() => {
  resetBindingMocks();
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

    const { getByTestId, queryByTestId } = render(ToolCallCard, { props: { pane, item } });
    expect(getByTestId('collab-tool-row').textContent).toContain('Spawned reviewer');
    expect(queryByTestId('collab-tool-row-state')).toBeNull();
    expect(queryByTestId('collab-tool-row-interactions')).toBeNull();
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
    expect(sentView.getByTestId('collab-tool-row').textContent).toContain('Sent follow-up to Agent');
    sentView.unmount();

    const progressView = render(ToolCallCard, { props: { pane, item: progress } });
    expect(progressView.getByTestId('collab-tool-row').textContent).toContain('Progress from reviewer');
    expect(progressView.getByText(/Running focused tests/)).toBeInTheDocument();
  });
});
