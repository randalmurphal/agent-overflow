import { describe, expect, it, beforeAll } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import SubagentGroupTestHarness from './SubagentGroupTestHarness.svelte';
import type { Item } from '../../types/models';
import type { SubagentGroupNode, TimelineLeaf, TimelineNode } from '../../utils/subagentGrouping';

// happy-dom lacks Element.animate. Keep this stub for any child
// components that use Svelte transitions while the group test harness
// mounts/unmounts nested content.
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

function mkLeaf(id: string, summary = '', kind = 'assistant_text'): TimelineLeaf {
  return { kind: 'leaf', item: mkItem({ id, summary, kind }) };
}

/** A tool_call leaf — the row kind the expanded body's digest always keeps. */
function mkToolLeaf(id: string, summary = ''): TimelineLeaf {
  return { kind: 'leaf', item: mkItem({ id, summary, kind: 'tool_call', toolName: 'Bash' }) };
}

interface AgentParentInput {
  description?: string;
  prompt?: string;
  subagent_type?: string;
  model?: string;
  reasoningEffort?: string;
  tool?: string;
  receiverThreadIds?: string[];
  newAgentNickname?: string;
  newAgentRole?: string;
  run_in_background?: boolean;
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
  const parent = parentItem ?? mkAgentParent(parentId);
  return {
    kind: 'group',
    parent,
    anchor: parent,
    groupKey: parentId,
    children: [],
    descendantCount: 0,
    loadedDescendantCount: 0,
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

    const toggle = getByRole('button', { name: /Find the bell icon/ });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');

    expect(getByTestId('subagent-group').getAttribute('data-tool-kind')).toBe('robot');
    expect(getByTestId('subagent-group-label').textContent).toContain('Explore');
    expect(getByTestId('subagent-group-count').textContent).toContain('2 entries');
    expect(getByTestId('subagent-group-count')).toHaveAttribute(
      'aria-label',
      '2 timeline entries inside this subagent group',
    );
    expect(queryByTestId('leaf')).toBeNull();
  });

  it('renders the title as `<agent_type> (<Model>)`, description, and initializing latest-action row', () => {
    // run_in_background:false is launch-time foreground proof — without
    // it a childless running card withholds the placeholder (see the
    // foreground-proof test below).
    const group = mkGroup({
      parentId: 'p1',
      parentItem: mkAgentParent('p1', {
        metaFields: { subagent_model: 'claude-opus-4-7' },
        input: { description: 'Find foo', subagent_type: 'Explore', run_in_background: false },
      }),
      children: [],
      descendantCount: 0,
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });

    const label = getByTestId('subagent-group-label').textContent ?? '';
    expect(label).toContain('Explore');
    expect(label).toContain('Opus 4.7');

    expect(getByTestId('subagent-group-description').textContent).toContain('Find foo');
    expect(getByTestId('subagent-group-preview').textContent).toContain('Initializing...');
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

  it('shows the running indicator while parent.status is streaming', () => {
    const group = mkGroup({
      parentId: 'p-stream',
      parentItem: mkAgentParent('p-stream', {
        status: 'streaming',
        input: { description: 'Streaming subagent', subagent_type: 'Explore' },
      }),
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });
    expect(getByTestId('subagent-group-status').querySelector('[data-testid="indicator"]')?.getAttribute('data-state')).toBe('running');
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

  it('shows a running indicator while parent.status is running, and no success indicator once it flips', () => {
    const running = mkGroup({
      parentId: 'p-run',
      parentItem: mkAgentParent('p-run', {
        status: 'running',
        input: { description: 'Long-running explore', subagent_type: 'Explore' },
      }),
    });
    const { getByTestId, unmount } = render(SubagentGroupTestHarness, { props: { group: running } });
    expect(getByTestId('subagent-group-status').querySelector('[data-testid="indicator"]')?.getAttribute('data-state')).toBe('running');
    unmount();

    const completed = mkGroup({
      parentId: 'p-done',
      parentItem: mkAgentParent('p-done', {
        status: 'completed',
        input: { description: 'Finished', subagent_type: 'Explore' },
      }),
    });
    const { container } = render(SubagentGroupTestHarness, { props: { group: completed } });
    expect(container.querySelector('[data-testid="subagent-group-status"]')).toBeNull();
  });

  it('shows RowError for terminal failed parent statuses', () => {
    const cases = [
      { status: 'errored' as const, expected: 'Agent failed' },
      { status: 'killed' as const, expected: 'Tool call stopped' },
      { status: 'declined' as const, expected: 'Tool call declined' },
    ];

    for (const testCase of cases) {
      const group = mkGroup({
        parentId: `p-${testCase.status}`,
        parentItem: mkAgentParent(`p-${testCase.status}`, {
          status: testCase.status,
          input: { description: 'Failed', subagent_type: 'Explore' },
        }),
      });
      const { getByTestId, unmount } = render(SubagentGroupTestHarness, { props: { group } });
      expect(getByTestId('subagent-group-status').querySelector('[data-testid="indicator"]')?.getAttribute('data-state')).toBe(
        testCase.status === 'declined' ? 'declined' : 'error',
      );
      expect(getByTestId('subagent-group-error').textContent).toContain(testCase.expected);
      unmount();
    }
  });

  it('keeps the status slot wrapper present in both running and completed states', () => {
    // Stability guard: running and terminal states share the same slot
    // wrapper so the transition does not shift adjacent chrome
    // (entry-count, elapsed). The wrapper carries
    // `subagent-group-status-slot` and a min-width class; the inner
    // content swaps under it.
    const running = mkGroup({
      parentId: 'p-run-slot',
      parentItem: mkAgentParent('p-run-slot', {
        status: 'running',
        input: { description: 'Stable slot', subagent_type: 'Explore' },
      }),
    });
    const { getByTestId: runningGet, unmount } = render(SubagentGroupTestHarness, { props: { group: running } });
    const runningSlot = runningGet('subagent-group-status-slot');
    expect(runningSlot).not.toBeNull();
    expect(runningSlot.className).toContain('min-w-');
    unmount();

    const completed = mkGroup({
      parentId: 'p-done-slot',
      parentItem: mkAgentParent('p-done-slot', {
        status: 'completed',
        input: { description: 'Stable slot done', subagent_type: 'Explore' },
      }),
    });
    const { getByTestId: doneGet } = render(SubagentGroupTestHarness, { props: { group: completed } });
    const doneSlot = doneGet('subagent-group-status-slot');
    expect(doneSlot).not.toBeNull();
    expect(doneSlot.className).toContain('min-w-');
  });

  it('always renders the elapsed slot so it never appears mid-run as a layout shift', () => {
    // The elapsed-time chip is rendered with a reserved slot even
    // before the first second tick produces a non-empty label, so
    // its first-paint mount does not push adjacent chrome rightward.
    const fresh = mkGroup({
      parentId: 'p-fresh',
      parentItem: {
        ...mkAgentParent('p-fresh', {
          status: 'running',
          input: { description: 'Just started', subagent_type: 'Explore' },
        }),
        createdAt: 0,
        updatedAt: 0,
      },
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group: fresh } });
    const slot = getByTestId('subagent-group-duration');
    expect(slot).not.toBeNull();
    expect(slot.className).toContain('min-w-');
  });

  it('shows final elapsed time for terminal subagents when timestamps are valid', () => {
    const parent = {
      ...mkAgentParent('p-duration', {
        status: 'completed',
        input: { description: 'Timed run', subagent_type: 'Explore' },
      }),
      createdAt: 1_000,
      updatedAt: 91_000,
    };
    const group = mkGroup({
      parentId: 'p-duration',
      parentItem: parent,
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group } });

    expect(getByTestId('subagent-group-duration').textContent?.trim()).toBe('1m 30s');
  });

  it('shows the backgrounded indicator when the parent is a backgrounded launch', () => {
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
    const indicator = status.querySelector('[data-testid="indicator"]');
    expect(indicator?.getAttribute('data-state')).toBe('backgrounded');
    expect(indicator?.getAttribute('aria-label')).toBe('Backgrounded');
  });

  it('clicking the header toggles expansion and renders children in an uncapped body', async () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [mkToolLeaf('c1', 'Bash: first'), mkToolLeaf('c2', 'Bash: second')],
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

    // The capped-body + fade scroller is deleted (spec Q6): the digest body
    // renders in place with no inner max-height scroll region.
    const body = getByTestId('subagent-group-body');
    expect(body.className).not.toContain('max-h-[20rem]');
    expect(body.className).not.toContain('overflow-y-auto');

    await fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await waitFor(() => expect(queryAllByTestId('leaf')).toHaveLength(0));
  });

  it('expanded body is a digest: tool calls + final text; thinking and intermediate text stay out', async () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [
        mkLeaf('think-1', 'pondering', 'thinking'),
        mkToolLeaf('tool-1', 'Bash: build'),
        mkLeaf('text-1', 'intermediate note'),
        mkToolLeaf('tool-2', 'Read: file'),
        mkLeaf('text-2', 'final report'),
      ],
      descendantCount: 5,
    });
    const { getByRole, getAllByTestId } = render(SubagentGroupTestHarness, {
      props: { group },
    });

    await fireEvent.click(getByRole('button'));

    const ids = getAllByTestId('leaf').map((el) => el.getAttribute('data-id'));
    expect(ids).toEqual(['tool-1', 'tool-2', 'text-2']);
  });

  it('drops the final-text slot when the agent was killed — mid-flight prose is not an answer', async () => {
    // User report 2026-08-22: a stopped agent's last text rendered in the
    // expanded body as if it were the final report. A killed/errored
    // agent has no final answer, so its digest is tool calls only.
    const group = mkGroup({
      parentId: 'p1',
      parentItem: mkAgentParent('p1', { status: 'killed' }),
      children: [
        mkToolLeaf('tool-1', 'Read: file'),
        mkLeaf('text-1', 'mid-flight prose'),
      ],
      descendantCount: 2,
    });
    const { getByRole, getAllByTestId } = render(SubagentGroupTestHarness, {
      props: { group },
    });

    await fireEvent.click(getByRole('button'));

    const ids = getAllByTestId('leaf').map((el) => el.getAttribute('data-id'));
    expect(ids).toEqual(['tool-1']);
  });

  it('uses native button activation for keyboard-accessible toggling', async () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [mkLeaf('c1', 'reachable')],
      descendantCount: 1,
    });
    const { getByRole, getAllByTestId } = render(SubagentGroupTestHarness, { props: { group } });
    const toggle = getByRole('button');

    // Testing-library's click event is the reliable stand-in for the
    // native activation event browsers synthesize for Enter/Space on a
    // focused button. The component intentionally does not add its own
    // keydown handler because that can double-toggle on Space.
    await fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(getAllByTestId('leaf')).toHaveLength(1);

    await fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });

  it('uses latestChildSummary for the stable latest-action row', () => {
    const withLatest = mkGroup({
      parentId: 'p',
      parentItem: mkAgentParent('p', {
        input: { description: 'Original ask', subagent_type: 'Explore' },
      }),
      children: [],
      descendantCount: 1,
      latestChildSummary: 'Bash: pwd',
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group: withLatest } });
    expect(getByTestId('subagent-group-preview').textContent?.trim()).toBe('└ Bash: pwd');
  });

  it('withholds the latest-action row until the launch is proven foreground', () => {
    // A flag-less Agent launch can still be flipped to a backgrounded
    // leaf by the CLI's async ack (is_background arrives only on the
    // tool_result, claude-wire.md §E5), and that flip must stay
    // height-neutral — so the "Initializing..." placeholder renders
    // only once foreground is proven, never in the unclassified window.
    const unknown = mkGroup({
      parentId: 'p-unknown',
      parentItem: mkAgentParent('p-unknown', {
        input: { description: 'Just launched', subagent_type: 'Explore' },
      }),
      children: [],
      descendantCount: 0,
      latestChildSummary: '',
    });
    const first = render(SubagentGroupTestHarness, { props: { group: unknown } });
    expect(first.getByTestId('subagent-group-description').textContent?.trim()).toBe('Just launched');
    expect(first.queryByTestId('subagent-group-preview')).toBeNull();
    first.unmount();

    // Descendants prove foreground even before any child produces text
    // (e.g. a history anchor carrying only the decorated count).
    const withDescendants = mkGroup({
      parentId: 'p-desc',
      parentItem: mkAgentParent('p-desc', {
        input: { description: 'Working', subagent_type: 'Explore' },
      }),
      children: [],
      descendantCount: 2,
      latestChildSummary: '',
    });
    const second = render(SubagentGroupTestHarness, { props: { group: withDescendants } });
    expect(second.getByTestId('subagent-group-preview').textContent?.trim()).toBe('└ Initializing...');
    second.unmount();

    // An explicit run_in_background:false in the tool input is
    // launch-time proof — the placeholder shows immediately.
    const explicitForeground = mkGroup({
      parentId: 'p-fg',
      parentItem: mkAgentParent('p-fg', {
        input: { description: 'Sync agent', subagent_type: 'Explore', run_in_background: false },
      }),
      children: [],
      descendantCount: 0,
      latestChildSummary: '',
    });
    const third = render(SubagentGroupTestHarness, { props: { group: explicitForeground } });
    expect(third.getByTestId('subagent-group-preview').textContent?.trim()).toBe('└ Initializing...');
    third.unmount();

    // A settled card with no child text has nothing to say — the
    // placeholder must not stick to finished agents forever.
    const settled = mkGroup({
      parentId: 'p-settled',
      parentItem: mkAgentParent('p-settled', {
        status: 'completed',
        input: { description: 'Done quietly', subagent_type: 'Explore' },
      }),
      children: [],
      descendantCount: 2,
      latestChildSummary: '',
    });
    const fourth = render(SubagentGroupTestHarness, { props: { group: settled } });
    expect(fourth.queryByTestId('subagent-group-preview')).toBeNull();
  });

  it('renders nested subagent groups recursively when expanded', async () => {
    const inner: SubagentGroupNode = mkGroup({
      parentId: 'inner',
      children: [mkToolLeaf('c1', 'inner-one'), mkToolLeaf('c2', 'inner-two')],
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

  it('shows a loading placeholder when expanded with unloaded descendants', async () => {
    // History loads deliver the anchor + decorated count without child
    // rows; the expanded body must say "loading", not lie with the
    // defensive no-entries copy, while hydration is in flight.
    const group = mkGroup({
      parentId: 'lazy',
      children: [],
      descendantCount: 4,
      loadedDescendantCount: 0,
    });
    const { getByRole, getByTestId, queryByText } = render(SubagentGroupTestHarness, {
      props: { group },
    });
    await fireEvent.click(getByRole('button'));
    expect(getByTestId('subagent-group-loading').textContent).toContain('Loading 4 entries');
    expect(queryByText(/No child entries captured/i)).not.toBeInTheDocument();
  });

  // Records hydration requests without a real pane. `getItemById` /
  // `subagentLiveAggregate` answer "not loaded" so the card falls back to
  // the node it was handed — these tests are about the hydration trigger,
  // and the live-resolution path has its own suite
  // (SubagentGroup.liveResolve.test.ts) driven by a real pane.
  function paneStub(): { pane: import('../../stores/thread.svelte').ThreadPane; ensured: string[] } {
    const ensured: string[] = [];
    const pane = {
      isSubagentGroupExpanded: () => true,
      toggleSubagentGroupExpanded: () => {},
      ensureSubagentChildren: (rootItemId: string) => {
        ensured.push(rootItemId);
        return Promise.resolve(true);
      },
      getItemById: () => undefined,
      subagentLiveAggregate: () => undefined,
      scrollController: null,
    } as unknown as import('../../stores/thread.svelte').ThreadPane;
    return { pane, ensured };
  }

  it('requests child hydration when expanded children trail the descendant count', async () => {
    const { pane, ensured } = paneStub();
    const group = mkGroup({
      parentId: 'lazy',
      children: [],
      descendantCount: 3,
      loadedDescendantCount: 0,
    });
    render(SubagentGroupTestHarness, { props: { group, pane } });
    await waitFor(() => expect(ensured).toEqual(['lazy']));
  });

  it('does not request hydration when every descendant is loaded', async () => {
    const { pane, ensured } = paneStub();
    const group = mkGroup({
      parentId: 'all-loaded',
      children: [mkLeaf('c1'), mkLeaf('c2')],
      descendantCount: 2,
      loadedDescendantCount: 2,
    });
    const { getByTestId } = render(SubagentGroupTestHarness, { props: { group, pane } });
    // The pane stub reports the card expanded; wait for the body to
    // mount so the hydration effect has definitely had its chance.
    await waitFor(() => expect(getByTestId('subagent-group-body')).toBeInTheDocument());
    expect(ensured).toEqual([]);
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

});
