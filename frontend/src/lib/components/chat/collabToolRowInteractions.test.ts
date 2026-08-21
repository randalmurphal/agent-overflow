import { beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ToolCallCard from './ToolCallCard.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  collabCardState,
  collabInteractionLabel,
  collabInteractions,
  type CollabInteraction,
} from './collabToolRowData';
import { codexSubagentDisplayLabel } from '../../utils/subagentLaunch';

beforeEach(() => {
  resetBindingMocks();
  setBindingMock('GetPayloadPreview', async () => ({
    data: '',
    totalSize: 0,
    nextOffset: 0,
    isComplete: true,
  }));
  setBindingMock('GetPayloadData', async () => ({ data: '' }));
});

function spawnLaunch(meta: Record<string, unknown>) {
  return makeItem({
    id: 'launch-1',
    kind: 'tool_call',
    status: 'running',
    toolName: 'collab_agent',
    isBackground: true,
    summary: 'Spawned reviewer',
    meta: JSON.stringify({
      input: {
        tool: 'spawn_agent',
        taskName: '/root/reviewer',
        newAgentNickname: 'reviewer',
        receiverThreadIds: ['child-1'],
      },
      ...meta,
    }),
  });
}

function interaction(overrides: Partial<CollabInteraction> = {}): CollabInteraction {
  return { id: 'i1', kind: 'interacted', tool: '', text: '', at: 1, ...overrides };
}

describe('collabInteractions', () => {
  it('reads well-formed entries and drops junk', () => {
    const parsed = collabInteractions({
      codex_collab_interactions: [
        { id: 'a', kind: 'interacted', tool: 'followup_task', at: 5 },
        { id: 'b', kind: 'progress', text: 'halfway; tests failing in X', at: 6 },
        { id: '', kind: 'interacted' },
        { id: 'c', kind: 'not-a-kind' },
        'nope',
        null,
      ],
    });
    expect(parsed).toEqual([
      { id: 'a', kind: 'interacted', tool: 'followup_task', text: '', at: 5 },
      { id: 'b', kind: 'progress', tool: '', text: 'halfway; tests failing in X', at: 6 },
    ]);
  });

  it('returns nothing for absent or non-array meta', () => {
    expect(collabInteractions(null)).toEqual([]);
    expect(collabInteractions({ codex_collab_interactions: {} })).toEqual([]);
  });
});

describe('collabInteractionLabel', () => {
  it('names follow-up tasks only when the raw verb proves it', () => {
    expect(collabInteractionLabel(interaction({ tool: 'followup_task' }))).toBe(
      'follow-up task sent',
    );
    expect(collabInteractionLabel(interaction({ tool: 'send_message' }))).toBe('message sent');
    // No raw verb (a resumed session): stays neutral, never upgraded to
    // "follow-up task" by observing that a child turn followed (invariant 25).
    expect(collabInteractionLabel(interaction({ tool: '' }))).toBe('message sent');
  });

  it('labels progress and resumed beats', () => {
    expect(collabInteractionLabel(interaction({ kind: 'progress' }))).toBe('progress reported');
    expect(collabInteractionLabel(interaction({ kind: 'resumed' }))).toBe('resumed');
  });

  it('carries a plaintext progress body, and stays bare without one', () => {
    expect(
      collabInteractionLabel(interaction({ kind: 'progress', text: 'halfway; tests failing in X' })),
    ).toBe('progress reported: halfway; tests failing in X');
    // An encrypted envelope carries no body at all — the beat still shows.
    expect(collabInteractionLabel(interaction({ kind: 'progress', text: '' }))).toBe(
      'progress reported',
    );
  });

  it('never repeats the child label the card header already carries', () => {
    for (const entry of [
      interaction({ tool: 'followup_task' }),
      interaction({ tool: 'send_message' }),
      interaction({ kind: 'progress' }),
      interaction({ kind: 'resumed' }),
    ]) {
      expect(collabInteractionLabel(entry)).not.toMatch(/reviewer|agent|\[/);
    }
  });
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

describe('collabCardState', () => {
  const child = ['child-1'];

  it('is live until every child reports a terminal status', () => {
    expect(collabCardState({}, child)).toBe('live');
    expect(
      collabCardState({ codex_child_terminal_statuses: { 'child-1': 'completed' } }, [
        'child-1',
        'child-2',
      ]),
    ).toBe('live');
  });

  it('is live again after a reactivated child clears its status', () => {
    expect(collabCardState({ codex_child_terminal_statuses: { 'child-1': '' } }, child)).toBe(
      'live',
    );
  });

  it('is idle when the child finished with nothing drained', () => {
    expect(
      collabCardState({ codex_child_terminal_statuses: { 'child-1': 'completed' } }, child),
    ).toBe('idle');
  });

  it('is delivered once a FINAL_ANSWER reached parent context', () => {
    expect(
      collabCardState(
        {
          codex_child_terminal_statuses: { 'child-1': 'completed' },
          codex_collab_delivered_at: 1700,
        },
        child,
      ),
    ).toBe('delivered');
  });

  it('is interrupted regardless of an earlier delivery', () => {
    expect(
      collabCardState(
        {
          codex_child_terminal_statuses: { 'child-1': 'interrupted' },
          codex_collab_delivered_at: 1700,
        },
        child,
      ),
    ).toBe('interrupted');
  });

  it('has no state without a known child roster', () => {
    expect(collabCardState({}, [])).toBeNull();
  });
});

describe('<CollabToolRow> spawn card', () => {
  it('renders interactions as sub-lines with a resumed section and the card state', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const item = spawnLaunch({
      codex_child_terminal_statuses: { 'child-1': 'completed' },
      codex_collab_delivered_at: 1700,
      codex_collab_interactions: [
        { id: 'a', kind: 'interacted', tool: 'send_message', at: 1 },
        { id: 'b', kind: 'progress', at: 2 },
        { id: 'c', kind: 'resumed', at: 3 },
        { id: 'd', kind: 'interacted', tool: 'followup_task', at: 4 },
      ],
    });
    const { getAllByTestId, getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });
    const lines = getAllByTestId('collab-tool-row-interaction');
    expect(lines.map((line) => line.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      '└ message sent',
      '└ progress reported',
      '└ resumed',
      '└ follow-up task sent',
    ]);
    expect(lines[2].getAttribute('data-kind')).toBe('resumed');
    expect(getByTestId('collab-tool-row-state').getAttribute('data-state')).toBe('delivered');
    expect(queryByTestId('collab-tool-row-interactions-earlier')).toBeNull();
  });

  it('shows only the newest interactions and counts the rest', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const item = spawnLaunch({
      codex_collab_interactions: Array.from({ length: 11 }, (_, index) => ({
        id: `i${index}`,
        kind: 'interacted',
        tool: 'send_message',
        at: index,
      })),
    });
    const { getAllByTestId, getByTestId } = render(ToolCallCard, { props: { pane, item } });
    expect(getAllByTestId('collab-tool-row-interaction')).toHaveLength(8);
    expect(getByTestId('collab-tool-row-interactions-earlier').textContent).toContain('+3 earlier');
    expect(getByTestId('collab-tool-row-state').getAttribute('data-state')).toBe('live');
  });

  it('renders no interaction block on a card with none', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const { queryByTestId } = render(ToolCallCard, { props: { pane, item: spawnLaunch({}) } });
    expect(queryByTestId('collab-tool-row-interactions')).toBeNull();
  });
});
