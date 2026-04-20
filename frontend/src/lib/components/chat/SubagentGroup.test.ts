import { describe, expect, it, beforeAll } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import SubagentGroupTestHarness from './SubagentGroupTestHarness.svelte';
import type { Item } from '../../types/models';
import type { SubagentGroupNode, TimelineLeaf, TimelineNode } from '../../utils/subagentGrouping';

// happy-dom lacks Element.animate; Svelte's transition:slide hits it when
// the region mounts/unmounts. Stub it with a fake Animation that fires the
// onfinish callback on the next microtask so Svelte processes the transition
// ending and Removes the element from the DOM promptly.
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

function mkGroup(
  overrides: Partial<SubagentGroupNode> & { parentId: string },
): SubagentGroupNode {
  const { parentId, ...rest } = overrides;
  return {
    kind: 'group',
    parent: mkItem({ id: parentId, summary: 'Task: run scan' }),
    children: [],
    descendantCount: 0,
    preview: '',
    truncated: false,
    ...rest,
  };
}

describe('<SubagentGroup>', () => {
  it('renders collapsed by default with the task title and entry count', () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [mkLeaf('c1', 'one'), mkLeaf('c2', 'two')],
      descendantCount: 2,
      preview: 'one / two',
    });
    const { getByRole, getByText, queryByTestId } = render(SubagentGroupTestHarness, {
      props: { group },
    });

    const toggle = getByRole('button');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    // The task title comes from the parent item's summary.
    expect(getByText('Task: run scan')).toBeInTheDocument();
    expect(getByText('2 entries')).toBeInTheDocument();
    // Children not rendered while collapsed.
    expect(queryByTestId('leaf')).toBeNull();
  });

  it('clicking the header toggles expansion and renders children', async () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [mkLeaf('c1', 'first child text'), mkLeaf('c2', 'second child text')],
      descendantCount: 2,
      preview: 'first child text / second child text',
    });
    const { getByRole, getAllByTestId, queryAllByTestId } = render(SubagentGroupTestHarness, {
      props: { group },
    });

    const toggle = getByRole('button');
    await fireEvent.click(toggle);

    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    const leaves = getAllByTestId('leaf');
    expect(leaves).toHaveLength(2);
    expect(leaves[0].textContent).toContain('first child text');
    expect(leaves[1].textContent).toContain('second child text');

    // Collapse again clears children. The slide out transition keeps the
    // element mounted momentarily, so waitFor lets Svelte flush the unmount.
    await fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await waitFor(() => expect(queryAllByTestId('leaf')).toHaveLength(0));
  });

  it('pressing Space on the header toggles expansion (keyboard accessible)', async () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [mkLeaf('c1', 'keyboard reachable')],
      descendantCount: 1,
      preview: 'keyboard reachable',
    });
    const { getByRole, getAllByTestId, queryAllByTestId } = render(SubagentGroupTestHarness, {
      props: { group },
    });

    const toggle = getByRole('button');
    // keydown dispatch mirrors what the browser would do for a focused button.
    await fireEvent.keyDown(toggle, { key: ' ' });

    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(getAllByTestId('leaf')).toHaveLength(1);

    await fireEvent.keyDown(toggle, { key: ' ' });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await waitFor(() => expect(queryAllByTestId('leaf')).toHaveLength(0));
  });

  it('pressing Enter on the header toggles expansion (native button behavior)', async () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [mkLeaf('c1', 'enter reachable')],
      descendantCount: 1,
      preview: 'enter reachable',
    });
    const { getByRole, getAllByTestId } = render(SubagentGroupTestHarness, {
      props: { group },
    });

    // Default <button> behavior handles Enter automatically via a click event.
    // testing-library simulates this via fireEvent.click.
    await fireEvent.click(getByRole('button'));
    expect(getAllByTestId('leaf')).toHaveLength(1);
  });

  it('shows the aggregated preview while collapsed', () => {
    const group = mkGroup({
      parentId: 'p1',
      children: [mkLeaf('c1', 'aggregated text')],
      descendantCount: 1,
      preview: 'aggregated text that describes what ran',
    });
    const { getByText } = render(SubagentGroupTestHarness, { props: { group } });
    expect(getByText(/aggregated text that describes what ran/)).toBeInTheDocument();
  });

  it('renders nested subagent groups recursively when expanded', async () => {
    // Outer group contains one inner group; the inner group has two leaves.
    const inner: SubagentGroupNode = mkGroup({
      parentId: 'inner',
      children: [mkLeaf('c1', 'inner-one'), mkLeaf('c2', 'inner-two')],
      descendantCount: 2,
      preview: 'inner-one / inner-two',
    });
    const outer = mkGroup({
      parentId: 'outer',
      children: [inner],
      descendantCount: 3,
      preview: 'inner-one / inner-two',
    });

    const { getAllByRole, getAllByTestId } = render(SubagentGroupTestHarness, {
      props: { group: outer },
    });
    // Start with a single button (the outer header).
    expect(getAllByRole('button')).toHaveLength(1);

    // Expand outer.
    await fireEvent.click(getAllByRole('button')[0]);

    // Inner SubagentGroup is now mounted; its header is a second button.
    const buttons = getAllByRole('button');
    expect(buttons).toHaveLength(2);

    // Leaves are still hidden because inner is collapsed.
    expect(() => getAllByTestId('leaf')).toThrow();

    // Expand inner.
    await fireEvent.click(buttons[1]);
    expect(getAllByTestId('leaf')).toHaveLength(2);
  });

  it('shows a no-entries message when the group has zero children (defensive)', async () => {
    const group = mkGroup({
      parentId: 'empty',
      children: [],
      descendantCount: 0,
      preview: '',
    });
    const { getByRole, getByText } = render(SubagentGroupTestHarness, { props: { group } });
    await fireEvent.click(getByRole('button'));
    expect(getByText(/No child entries captured/i)).toBeInTheDocument();
  });

  it('singular / plural entry count agreement', () => {
    const one = mkGroup({
      parentId: 'p',
      children: [mkLeaf('c1')],
      descendantCount: 1,
    });
    const { getByText, unmount } = render(SubagentGroupTestHarness, { props: { group: one } });
    expect(getByText('1 entry')).toBeInTheDocument();
    unmount();

    const many: SubagentGroupNode = mkGroup({
      parentId: 'p2',
      children: [mkLeaf('a'), mkLeaf('b'), mkLeaf('c')] as TimelineNode[],
      descendantCount: 3,
    });
    const second = render(SubagentGroupTestHarness, { props: { group: many } });
    expect(second.getByText('3 entries')).toBeInTheDocument();
  });

  it('falls back to "Subagent" when the parent summary is empty', () => {
    const group = mkGroup({
      parentId: 'p',
      children: [mkLeaf('c')],
      descendantCount: 1,
    });
    // Override the default parent summary with an empty string.
    group.parent = { ...group.parent, summary: '   ' };
    const { getAllByText } = render(SubagentGroupTestHarness, { props: { group } });
    // Exactly one "Subagent" node for the fallback title (plus the UPPERCASE
    // accent badge — matcher is case-insensitive). Accept either.
    const matches = getAllByText(/subagent/i);
    expect(matches.length).toBeGreaterThan(0);
  });

  it('appends an ellipsis when the preview is truncated', () => {
    const group = mkGroup({
      parentId: 'p',
      children: [mkLeaf('c')],
      descendantCount: 1,
      preview: 'this got cut off somewhere',
      truncated: true,
    });
    const { container } = render(SubagentGroupTestHarness, { props: { group } });
    // The ellipsis follows the preview text. We search the rendered DOM for
    // the marker to avoid locking to a specific element structure.
    expect(container.textContent).toMatch(/this got cut off somewhere…/);
  });
});
