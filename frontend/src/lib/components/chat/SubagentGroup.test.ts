import { describe, expect, it, beforeAll } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import SubagentGroupTestHarness from './SubagentGroupTestHarness.svelte';
import type { Item } from '../../types/models';
import type { SubagentGroupNode, TimelineLeaf, TimelineNode } from '../../utils/subagentGrouping';

// happy-dom lacks Element.animate; Svelte's transition:slide hits it
// when the region mounts/unmounts. Stub it with a fake Animation that
// fires onfinish on the next microtask so Svelte processes the
// transition end and removes the element from the DOM promptly.
beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        let onfinish: (() => void) | null = null;
        const animation = {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {},
          finish() {
            onfinish?.();
          },
          play() {},
          pause() {},
          reverse() {},
          addEventListener(type: string, cb: EventListener) {
            if (type === 'finish') onfinish = cb as unknown as () => void;
          },
          removeEventListener() {},
          get onfinish() {
            return onfinish;
          },
          set onfinish(cb: (() => void) | null) {
            onfinish = cb;
            if (cb) queueMicrotask(cb);
          },
        };
        return animation;
      };
  }
});

function mkItem(overrides: Partial<Item> & { id: string }): Item {
  const createdAt = overrides.createdAt ?? 0;
  return {
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: '',
    createdAt,
    updatedAt: overrides.updatedAt ?? createdAt,
    ...overrides,
  };
}

function mkLeaf(id: string, summary = ''): TimelineLeaf {
  return { kind: 'leaf', item: mkItem({ id, summary }) };
}

interface AgentParentInput {
  description?: string;
  prompt?: string;
  subagent_type?: string;
  model?: string;
  tool?: string;
}

interface AgentParentOverrides {
  toolName?: string;
  status?: Item['status'];
  isBackground?: boolean;
  /** Becomes parent.meta JSON. */
  metaFields?: Record<string, unknown>;
  /** Becomes payloadMeta.input. */
  input?: AgentParentInput;
}

function mkAgentParent(id: string, overrides: AgentParentOverrides = {}): Item {
  const meta = overrides.metaFields ? JSON.stringify(overrides.metaFields) : '';
  const payloadMeta = overrides.input
    ? JSON.stringify({ toolName: overrides.toolName ?? 'Agent', input: overrides.input })
    : '';
  return mkItem({
    id,
    kind: 'tool_call',
    toolName: overrides.toolName ?? 'Agent',
    status: overrides.status ?? 'running',
    isBackground: overrides.isBackground,
    summary: 'Agent: launching',
    meta,
    payloadMeta,
  });
}

function mkGroup(
  overrides: Partial<SubagentGroupNode> & { parentId: string; parentItem?: Item },
): SubagentGroupNode {
  const { parentId, parentItem, ...rest } = overrides;
  return {
    kind: 'group',
    parent: parentItem ?? mkAgentParent(parentId),
    children: [],
    descendantCount: 0,
    latestChildSummary: '',
    ...rest,
  };
}

describe('<SubagentGroup>', () => {
  it('renders collapsed by default with the agent label, entry count, and robot icon', () => {
    const group = mkGroup({
      parentId: 'p1',
      parentItem: mkAgentParent('p1', {
        input: { description: 'Find the bell icon', subagent_type: 'Explore' },
      }),
      children: [mkLeaf('c1', 'one'), mkLeaf('c2', 'two')],
      descendantCount: 2,
    });
    const { getByRole, getByTestId, queryByTestId } = render(SubagentGroupTestHarness, {
      props: { group },
    });

    const toggle = getByRole('button');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');

    expect(getByTestId('subagent-group').getAttribute('data-tool-kind')).toBe('robot');
    expect(getByTestId('subagent-group-label').textContent).toContain('Explore');
    expect(getByTestId('subagent-group-count').textContent).toContain('2 entries');
    expect(queryByTestId('leaf')).toBeNull();
  });

  it('renders the title as `<agent_type> (<Model>)` and the description as preview', () => {
    const group = mkGroup({
      parentId: 'p1',
      parentItem: mkAgentParent('p1', {
        metaFields: { subagent_model: 'claude-opus-4-7' },
        input: { description: 'Find foo', subagent_type: 'Explore' },
      }),
      children: [],
      descendantCount: 0,
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });

    const label = getByTestId('subagent-group-label').textContent ?? '';
    expect(label).toContain('Explore');
    expect(label).toContain('Opus 4.7');

    expect(getByTestId('subagent-group-preview').textContent).toContain('Find foo');
  });

  it('falls back to "Agent" when subagent_type is missing', () => {
    const group = mkGroup({
      parentId: 'p1',
      parentItem: mkAgentParent('p1', {
        input: { description: 'Investigate something' },
      }),
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });
    expect(getByTestId('subagent-group-label').textContent).toContain('Agent');
  });

  it('shows "running" while parent.status is streaming (matches running branch)', () => {
    const group = mkGroup({
      parentId: 'p-stream',
      parentItem: mkAgentParent('p-stream', {
        status: 'streaming',
        input: { description: 'Streaming subagent', subagent_type: 'Explore' },
      }),
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });
    expect(getByTestId('subagent-group-status').textContent?.trim()).toBe('running');
  });

  it('falls back to the launch input.model when no subagent_model is stamped yet', () => {
    // Covers the brief window between launch and the subagent's
    // first assistant envelope: the parser hasn't stamped meta yet,
    // but the user-supplied `input.model` alias is already in the
    // payloadMeta and should drive the affix.
    const group = mkGroup({
      parentId: 'p-pre-stamp',
      parentItem: mkAgentParent('p-pre-stamp', {
        // No metaFields → no subagent_model on parent.meta.
        input: { description: 'Just launched', subagent_type: 'Explore', model: 'opus' },
      }),
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });
    expect(getByTestId('subagent-group-label').textContent).toContain('Opus');
  });

  it('omits the model affix entirely for Codex collab_agent (no per-spawn override on the wire)', () => {
    const group = mkGroup({
      parentId: 'codex-no-model',
      parentItem: mkAgentParent('codex-no-model', {
        toolName: 'collab_agent',
        // Even if some hypothetical model field crept into the input,
        // the affix should stay empty — Codex children share the
        // session model and we don't surface that on the card.
        metaFields: { subagent_model: 'claude-opus-4-7' },
        input: { tool: 'spawnAgent', prompt: 'audit' },
      }),
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });
    const label = getByTestId('subagent-group-label').textContent ?? '';
    expect(label).not.toContain('Opus');
    expect(label).not.toContain('(');
  });

  it('shows "running" while parent.status is running, and CompletionBadge once it flips', () => {
    const running = mkGroup({
      parentId: 'p-run',
      parentItem: mkAgentParent('p-run', {
        status: 'running',
        input: { description: 'Long-running explore', subagent_type: 'Explore' },
      }),
    });
    const { getByTestId, unmount } = render(SubagentGroupTestHarness, { props: { group: running } });
    expect(getByTestId('subagent-group-status').textContent?.trim()).toBe('running');
    unmount();

    const completed = mkGroup({
      parentId: 'p-done',
      parentItem: mkAgentParent('p-done', {
        status: 'completed',
        input: { description: 'Finished', subagent_type: 'Explore' },
      }),
    });
    const { container } = render(SubagentGroupTestHarness, { props: { group: completed } });
    // Completion badge is rendered instead of the "running" label.
    expect(container.querySelector('[data-testid="subagent-group-status"]')).toBeNull();
    // CompletionBadge component renders a node with role status / aria-label.
    const badge = container.querySelector('[data-testid="completion-badge"]');
    expect(badge).not.toBeNull();
  });

  it('shows the "…" chip when the parent is a backgrounded launch', () => {
    const group = mkGroup({
      parentId: 'bg',
      parentItem: mkAgentParent('bg', {
        status: 'running',
        isBackground: true,
        input: { description: 'Bg subagent', subagent_type: 'Explore' },
      }),
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });
    const status = getByTestId('subagent-group-status');
    expect(status.textContent?.trim()).toBe('…');
    expect(status.getAttribute('aria-label')).toBe('Backgrounded');
  });

  it('clicking the header toggles expansion and renders children inside a scrollable body', async () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [mkLeaf('c1', 'first child text'), mkLeaf('c2', 'second child text')],
      descendantCount: 2,
    });
    const { getByRole, getByTestId, getAllByTestId, queryAllByTestId } = render(
      SubagentGroupTestHarness,
      { props: { group } },
    );

    const toggle = getByRole('button');
    await fireEvent.click(toggle);

    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    const leaves = getAllByTestId('leaf');
    expect(leaves).toHaveLength(2);

    const body = getByTestId('subagent-group-body');
    expect(body.className).toContain('max-h-[20rem]');
    expect(body.className).toContain('overflow-y-auto');

    await fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await waitFor(() => expect(queryAllByTestId('leaf')).toHaveLength(0));
  });

  it('Space and Enter activate the toggle (keyboard accessible)', async () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [mkLeaf('c1', 'reachable')],
      descendantCount: 1,
    });
    const { getByRole, getAllByTestId } = render(SubagentGroupTestHarness, { props: { group } });
    const toggle = getByRole('button');

    await fireEvent.keyDown(toggle, { key: ' ' });
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(getAllByTestId('leaf')).toHaveLength(1);

    // Enter is the native button activation — fireEvent.click matches.
    await fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });

  it('uses latestChildSummary for the preview when present, falling back to input.description', () => {
    const withLatest = mkGroup({
      parentId: 'p',
      parentItem: mkAgentParent('p', {
        input: { description: 'Original ask', subagent_type: 'Explore' },
      }),
      children: [],
      descendantCount: 1,
      latestChildSummary: 'Bash: pwd',
    });
    const { getByTestId, unmount } = render(SubagentGroupTestHarness, { props: { group: withLatest } });
    expect(getByTestId('subagent-group-preview').textContent?.trim()).toBe('Bash: pwd');
    unmount();

    const noLatest = mkGroup({
      parentId: 'p',
      parentItem: mkAgentParent('p', {
        input: { description: 'Just launched', subagent_type: 'Explore' },
      }),
      children: [],
      descendantCount: 0,
      latestChildSummary: '',
    });
    const second = render(SubagentGroupTestHarness, { props: { group: noLatest } });
    expect(second.getByTestId('subagent-group-preview').textContent?.trim()).toBe('Just launched');
  });

  it('renders nested subagent groups recursively when expanded', async () => {
    const inner: SubagentGroupNode = mkGroup({
      parentId: 'inner',
      children: [mkLeaf('c1', 'inner-one'), mkLeaf('c2', 'inner-two')],
      descendantCount: 2,
    });
    const outer = mkGroup({
      parentId: 'outer',
      children: [inner],
      descendantCount: 3,
    });

    const { getAllByRole, getAllByTestId } = render(SubagentGroupTestHarness, {
      props: { group: outer },
    });
    expect(getAllByRole('button')).toHaveLength(1);

    await fireEvent.click(getAllByRole('button')[0]);

    const buttons = getAllByRole('button');
    expect(buttons).toHaveLength(2);

    expect(() => getAllByTestId('leaf')).toThrow();

    await fireEvent.click(buttons[1]);
    expect(getAllByTestId('leaf')).toHaveLength(2);
  });

  it('shows a no-entries message when expanded with zero children (defensive)', async () => {
    const group = mkGroup({ parentId: 'empty', children: [], descendantCount: 0 });
    const { getByRole, getByText } = render(SubagentGroupTestHarness, { props: { group } });
    await fireEvent.click(getByRole('button'));
    expect(getByText(/No child entries captured/i)).toBeInTheDocument();
  });

  it('singular / plural entry count agreement', () => {
    const one = mkGroup({ parentId: 'p', children: [mkLeaf('c1')], descendantCount: 1 });
    const { getByTestId, unmount } = render(SubagentGroupTestHarness, { props: { group: one } });
    expect(getByTestId('subagent-group-count').textContent).toContain('1 entry');
    unmount();

    const many: SubagentGroupNode = mkGroup({
      parentId: 'p2',
      children: [mkLeaf('a'), mkLeaf('b'), mkLeaf('c')] as TimelineNode[],
      descendantCount: 3,
    });
    const second = render(SubagentGroupTestHarness, { props: { group: many } });
    expect(second.getByTestId('subagent-group-count').textContent).toContain('3 entries');
  });

  it('grandchild (depth >= 3) renders as marker only — no recursive card', () => {
    const group = mkGroup({
      parentId: 'grand',
      children: [mkLeaf('leaf-under-grand', 'hidden body')],
      descendantCount: 1,
    });
    const { queryByTestId, queryByRole, getByText } = render(SubagentGroupTestHarness, {
      props: { group, startDepth: 3 },
    });
    expect(queryByTestId('subagent-group')).toBeNull();
    expect(queryByRole('button')).toBeNull();
    expect(queryByTestId('subagent-group-marker')).not.toBeNull();
    expect(getByText(/Spawned subagent/i)).toBeInTheDocument();
    expect(queryByTestId('leaf')).toBeNull();
  });

  it('renders Codex collab_agent with title-cased tool label and prompt preview', () => {
    const group = mkGroup({
      parentId: 'codex-1',
      parentItem: mkAgentParent('codex-1', {
        toolName: 'collab_agent',
        input: { tool: 'spawnAgent', prompt: 'Audit the new feature flags rollout' },
      }),
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });
    expect(getByTestId('subagent-group-label').textContent).toContain('Spawn Agent');
    expect(getByTestId('subagent-group-preview').textContent).toContain('Audit the new feature flags rollout');
  });
});
