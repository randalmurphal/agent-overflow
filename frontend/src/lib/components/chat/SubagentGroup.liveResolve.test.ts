// The subagent card resolves its own live state.
//
// `SubagentGroupNode` is a STRUCTURAL snapshot: the timeline rebuilds it
// when the item window changes shape, not when an item's content moves.
// Everything on this card that moves inside a turn — anchor status, the
// entry count, the latest-action preview — is therefore re-read from the
// pane by the card itself, the same row-boundary resolution `TimelineLeaf`
// and `ReadGroupRow` use. Before that, the projection patched fresh refs
// into the node centrally, which made every streaming tick of every group
// descendant rebuild the whole virtualizer data array.
//
// These tests feed the card a deliberately stale node and assert it
// renders the store's values, over transitions rather than single states.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import {
  groupItemsBySubagent,
  type SubagentGroupNode,
  type TimelineNode,
} from '../../utils/subagentGrouping';
import SubagentGroupTestHarness from './SubagentGroupTestHarness.svelte';
import {
  applySubagentProgress,
  resetForTest as resetSubagentProgressForTest,
} from '../../stores/subagentProgress.svelte';

function findGroup(nodes: readonly TimelineNode[]): SubagentGroupNode {
  for (const node of nodes) {
    if (node.kind === 'group') return node;
  }
  throw new Error('fixture produced no subagent group');
}

function agentLaunch(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'agent:1',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Agent',
    role: 'assistant',
    status: 'running',
    summary: 'Agent: exploring',
    payloadMeta: JSON.stringify({
      toolName: 'Agent',
      input: { description: 'Find the bell icon', subagent_type: 'Explore' },
    }),
    ...overrides,
  });
}

/**
 * Builds the pane and captures the group node ONCE. The node keeps the
 * item objects it was built from, so every later store write leaves it
 * stale — which is precisely the production condition being tested.
 */
async function setup(items: Item[]): Promise<{ pane: ThreadPane; group: SubagentGroupNode }> {
  const pane = await buildPane(undefined, items);
  return { pane, group: findGroup(groupItemsBySubagent([...pane.items])) };
}

function indicatorState(container: HTMLElement): string | null {
  const status = container.querySelector('[data-testid="subagent-group-status"]');
  return status?.querySelector('[data-testid="indicator"]')?.getAttribute('data-state') ?? null;
}

describe('<SubagentGroup> live resolution against the pane', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('tracks the anchor status through running → errored on a stale node', async () => {
    const { pane, group } = await setup([
      agentLaunch(),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'running', summary: 'reading' }),
    ]);
    const { container } = render(SubagentGroupTestHarness, { props: { group, pane } });
    expect(indicatorState(container)).toBe('running');

    pane.applyItemPatch({
      threadId: 'thread-1', itemId: 'agent:1', kind: 'tool_call',
      patch: { status: 'errored', updatedAt: 5 },
    });
    await tick();

    expect(group.parent.status, 'node must stay stale for this to prove anything').toBe('running');
    expect(indicatorState(container)).toBe('error');
  });

  it('tracks the latest-action preview as a child streams', async () => {
    const { pane, group } = await setup([
      agentLaunch(),
      makeItem({
        id: 'child:1', itemIndex: 1, parentId: 'agent:1',
        status: 'streaming', summary: 'reading alpha.ts',
      }),
    ]);
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });
    expect(getByTestId('subagent-group-preview').textContent).toContain('alpha.ts');

    pane.upsertItem(makeItem({
      id: 'child:1', itemIndex: 1, parentId: 'agent:1',
      status: 'streaming', summary: 'reading beta.ts', updatedAt: 6,
    }));
    await tick();

    expect(getByTestId('subagent-group-preview').textContent).toContain('beta.ts');
  });

  it('falls back to the node snapshot when a settled child is evicted', async () => {
    // The other half of the resolver contract. A collapsed card's settled
    // descendants are evicted from the window, so `getItemById` starts
    // answering undefined for a row the node still lists. The preview must
    // land on the snapshot, not blank out.
    const { pane, group } = await setup([
      agentLaunch(),
      makeItem({
        id: 'child:1', itemIndex: 1, parentId: 'agent:1',
        status: 'running', summary: 'reading alpha.ts',
      }),
    ]);
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });

    pane.upsertItem(makeItem({
      id: 'child:1', itemIndex: 1, parentId: 'agent:1',
      status: 'completed', summary: 'read alpha.ts', updatedAt: 7,
    }));
    await tick();

    expect(pane.getItemById('child:1'), 'settled child must be evicted here').toBeUndefined();
    expect(getByTestId('subagent-group-preview').textContent).toContain('reading alpha.ts');
  });

  it('picks up an entry-count decoration that lands without a structural rebuild', async () => {
    const { pane, group } = await setup([
      agentLaunch(),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'running', summary: 'one' }),
    ]);
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });
    expect(group.descendantCount).toBe(1);
    expect(getByTestId('subagent-group-count').textContent).toContain('1 entry');

    pane.applyItemMeta({
      threadId: 'thread-1', itemId: 'agent:1', kind: 'tool_call',
      meta: JSON.stringify({ subagentDescendantCount: 7 }), updatedAt: 8,
    });
    await tick();

    expect(getByTestId('subagent-group-count').textContent).toContain('7 entries');
  });

  it('holds the entry count when a later write drops the decoration', async () => {
    // Transition coverage for the card's `Math.max`: decoration present →
    // absent must land on the node's own count, never on zero. Here the
    // node was itself built while the decoration existed, so it already
    // carries 7 — the assertion is that the card does not regress to the
    // one loaded child, and does not blank the label.
    const { pane, group } = await setup([
      agentLaunch({ meta: JSON.stringify({ subagentDescendantCount: 7 }) }),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'running', summary: 'one' }),
    ]);
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });
    expect(group.descendantCount).toBe(7);
    expect(getByTestId('subagent-group-count').textContent).toContain('7 entries');

    pane.applyItemMeta({
      threadId: 'thread-1', itemId: 'agent:1', kind: 'tool_call', meta: '', updatedAt: 9,
    });
    await tick();

    expect(getByTestId('subagent-group-count').textContent).toContain('7 entries');
  });
});

describe('<SubagentGroup> card affordances (agent-visibility)', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
    resetSubagentProgressForTest();
  });

  it('shows the live activity line, tool count and tokens while the agent runs', async () => {
    const { pane, group } = await setup([
      agentLaunch(),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'running', summary: 'reading' }),
    ]);
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });

    applySubagentProgress({
      threadId: 'thread-1',
      itemId: 'agent:1',
      progress: { taskId: 'task-9', toolUses: 4, totalTokens: 12_400, activity: 'Scanning the parser' },
      updatedAt: 100,
    });
    await tick();

    expect(getByTestId('subagent-group-preview').textContent).toContain('Scanning the parser');
    expect(getByTestId('subagent-group-tools').textContent?.trim()).toBe('4 tools');
    expect(getByTestId('subagent-group-tokens').textContent?.trim()).toBe('12.4k tokens');
  });

  it('prefers the persisted final numbers and drops the activity once settled', async () => {
    const { pane, group } = await setup([
      agentLaunch({
        status: 'completed',
        meta: JSON.stringify({
          subagentProgress: { toolUses: 7, totalTokens: 250_000, durationMs: 90_000 },
        }),
      }),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'completed', summary: 'done' }),
    ]);
    // A stale live tick that outlived the terminal must not win.
    applySubagentProgress({
      threadId: 'thread-1',
      itemId: 'agent:1',
      progress: { toolUses: 3, totalTokens: 1_000, activity: 'stale tick' },
      updatedAt: 50,
    });
    const { getByTestId, queryByTestId } = render(SubagentGroupTestHarness, {
      props: { group, pane },
    });

    expect(getByTestId('subagent-group-tools').textContent?.trim()).toBe('7 tools');
    expect(getByTestId('subagent-group-tokens').textContent?.trim()).toBe('250.0k tokens');
    const preview = queryByTestId('subagent-group-preview');
    if (preview) expect(preview.textContent).not.toContain('stale tick');
  });

  it('marks an async launch as background (data attribute, no pill) with the agent kind chip', async () => {
    // The visible "background" pill was removed by user ruling (2026-08-22):
    // it was noise on every async card. Classification stays pinned via the
    // card root's data-background attribute, which the e2e specs assert too.
    // A detached Claude launch has no card until its completion sibling
    // loads (the spawn row is immutable; the card sits at the completion —
    // utils/subagentGrouping.ts `SubagentGroupNode.anchor`), so the fixture
    // settles the agent to get one.
    const { pane, group } = await setup([
      agentLaunch({ isBackground: true }),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'completed', summary: 'w' }),
      makeItem({
        id: 'complete:agent:1',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'agent:1',
        status: 'completed',
        summary: 'Agent: done',
      }),
    ]);
    const { getByTestId, queryByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });
    expect(getByTestId('subagent-group').getAttribute('data-background')).toBe('true');
    expect(queryByTestId('subagent-group-background-pill')).toBeNull();
    expect(getByTestId('subagent-group-kind').textContent?.trim()).toBe('agent');
  });

  it('labels a forked skill card with the skill kind chip and never offers backgrounding', async () => {
    const { pane, group } = await setup([
      makeItem({
        id: 'skill:1',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Skill',
        role: 'assistant',
        status: 'running',
        summary: 'Skill: code-review',
        payloadMeta: JSON.stringify({ toolName: 'Skill', input: { skill: 'code-review' } }),
      }),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'skill:1', status: 'running', summary: 'w' }),
    ]);
    const { getByTestId, queryByTestId } = render(SubagentGroupTestHarness, {
      props: { group, pane },
    });
    expect(getByTestId('subagent-group-kind').textContent?.trim()).toBe('skill');
    expect(getByTestId('subagent-group-label').textContent).toContain('code-review');
    expect(queryByTestId('subagent-group-background-button')).toBeNull();
  });

  it('background button backgrounds a running foreground agent through the binding', async () => {
    const calls: unknown[][] = [];
    setBindingMock('BackgroundClaudeTask', async (...args: unknown[]) => {
      calls.push(args);
      return null;
    });
    const { pane, group } = await setup([
      agentLaunch(),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'running', summary: 'w' }),
    ]);
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });

    await fireEvent.click(getByTestId('subagent-group-background-button'));
    await tick();

    expect(calls).toEqual([['thread-1', 'agent:1']]);
  });

  it('surfaces a background refusal on the card instead of swallowing it', async () => {
    setBindingMock('BackgroundClaudeTask', async () => {
      throw new Error('no matching foreground task');
    });
    const { pane, group } = await setup([
      agentLaunch(),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'running', summary: 'w' }),
    ]);
    const { getByTestId, findByTestId } = render(SubagentGroupTestHarness, {
      props: { group, pane },
    });

    await fireEvent.click(getByTestId('subagent-group-background-button'));

    const error = await findByTestId('subagent-group-background-error');
    expect(error.textContent).toContain('no matching foreground task');
  });

  it('open-in-pane routes through pane.openAgentPane with the launch id and display name', async () => {
    // One door: the pane decides where opening goes (the base pane opens
    // the companion; the agent pane's facade pushes scope). The card only
    // ever calls pane.openAgentPane.
    const opened: [string, string][] = [];
    const { pane, group } = await setup([
      agentLaunch(),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'running', summary: 'w' }),
    ]);
    const spied = new Proxy(pane, {
      get(target, prop) {
        if (prop === 'openAgentPane') {
          return (id: string, label: string) => opened.push([id, label]);
        }
        return Reflect.get(target, prop, target);
      },
    });
    const { getByTestId } = render(SubagentGroupTestHarness, {
      props: { group, pane: spied },
    });

    await fireEvent.click(getByTestId('subagent-group-open-pane'));

    expect(opened).toEqual([['agent:1', 'Explore']]);
  });

  it('surfaces a failed transcript backfill as an inline error on the card', async () => {
    // triage stamps notification_output_state/error when the
    // task_notification's output_file could not be read. A silently
    // incomplete body reads exactly like a complete one, so the card says
    // so inline.
    const { pane, group } = await setup([
      agentLaunch({
        status: 'completed',
        meta: JSON.stringify({
          notification_output_state: 'error',
          notification_output_error: 'output file vanished before read',
        }),
      }),
      makeItem({ id: 'child:1', itemIndex: 1, parentId: 'agent:1', status: 'completed', summary: 'done' }),
    ]);
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });
    expect(getByTestId('subagent-group-output-error').textContent).toContain(
      'output file vanished before read',
    );
  });

  it("summarizes a finished Codex child with its answer, and renders that answer once", async () => {
    // A Codex child DOES stream its transcript to the parent, parented to
    // the launch — its final assistant message is the answer, as a normal
    // message. The completion sibling's `preview` is a 240-char truncation
    // of that same text, so it is the collapsed one-liner and nothing
    // more: rendering it in the body too showed the answer twice,
    // unformatted and cut mid-word (user ruling 2026-08-23).
    const { pane, group } = await setup([
      agentLaunch({
        toolName: 'collab_agent',
        status: 'completed',
        summary: 'Spawn reviewer',
        payloadMeta: JSON.stringify({
          toolName: 'collab_agent',
          input: { tool: 'spawn_agent', newAgentNickname: 'reviewer' },
        }),
      }),
      makeItem({
        id: 'child-text',
        itemIndex: 1,
        threadId: 'thread-1',
        kind: 'assistant_text',
        role: 'assistant',
        status: 'completed',
        parentId: 'agent:1',
        summary: 'Final verdict: LGTM, with one caveat about the parser drift.',
      }),
      makeItem({
        id: 'complete:agent:1',
        itemIndex: 2,
        threadId: 'thread-1',
        kind: 'tool_completion',
        toolName: 'collab_agent',
        status: 'completed',
        completionOf: 'agent:1',
        payloadMeta: JSON.stringify({ preview: 'Final verdict: LGTM, with one caveat' }),
      }),
    ]);
    expect(group.anchor.id).toBe('complete:agent:1');
    const { getByTestId, getAllByText, queryByTestId } = render(SubagentGroupTestHarness, {
      props: { group, pane },
    });
    expect(getByTestId('subagent-group-preview').textContent).toContain('Final verdict: LGTM');

    await fireEvent.click(getByTestId('subagent-group-toggle'));
    expect(queryByTestId('subagent-group-final-answer')).toBeNull();
    expect(queryByTestId('subagent-group-digest-empty')).toBeNull();
    expect(
      getAllByText(/Final verdict: LGTM, with one caveat about the parser drift/),
    ).toHaveLength(1);
  });

  // Real V2 wire shape (codex 0.149.0): `{task_name, fork_turns,
  // message}` with the message encrypted and NO nickname anywhere, so
  // the label already IS the model-chosen task name. The description
  // slot must stay empty rather than repeat it.
  it('says a V2 Codex task name once, in the label, not twice', async () => {
    const { pane, group } = await setup([
      agentLaunch({
        toolName: 'collab_agent',
        status: 'completed',
        summary: 'Spawn audit_internal_tail',
        payloadMeta: JSON.stringify({
          toolName: 'collab_agent',
          input: {
            tool: 'spawn_agent',
            activityKind: 'started',
            taskName: '/root/audit_internal_tail',
          },
        }),
      }),
      makeItem({
        id: 'complete:agent:1',
        itemIndex: 1,
        threadId: 'thread-1',
        kind: 'tool_completion',
        toolName: 'collab_agent',
        status: 'completed',
        completionOf: 'agent:1',
      }),
    ]);
    const { getByTestId, queryByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });
    expect(getByTestId('subagent-group-label').textContent).toContain('audit_internal_tail');
    expect(queryByTestId('subagent-group-description')).toBeNull();
  });

  // V1 (`collabAgentToolCall`) DOES carry a plaintext prompt. The card
  // reads it off the Codex input, not through the Claude reader whose
  // `description` branch returns unclamped text.
  it('describes a V1 Codex card with its plaintext prompt, clamped', async () => {
    const prompt = 'Audit '.repeat(30);
    const { pane, group } = await setup([
      agentLaunch({
        toolName: 'collab_agent',
        status: 'completed',
        summary: 'Spawn reviewer',
        payloadMeta: JSON.stringify({
          toolName: 'collab_agent',
          input: { tool: 'spawn_agent', newAgentNickname: 'reviewer', prompt },
        }),
      }),
      makeItem({
        id: 'complete:agent:1',
        itemIndex: 1,
        threadId: 'thread-1',
        kind: 'tool_completion',
        toolName: 'collab_agent',
        status: 'completed',
        completionOf: 'agent:1',
      }),
    ]);
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });
    const shown = getByTestId('subagent-group-description').textContent?.trim() ?? '';
    expect(shown).toContain('Audit Audit');
    expect(shown.length).toBeLessThanOrEqual(81);
  });
});
