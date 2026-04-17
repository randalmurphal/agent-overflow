import { describe, expect, it } from 'vitest';
import { buildDisplayRows, defaultExpandedParents } from './threadTree';
import type { Thread } from '../types/models';

function t(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'id',
    title: 'thread',
    provider: 'claude',
    workspacePath: '/ws',
    projectPath: '/ws',
    interactionMode: 'default',
    model: 'm',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('buildDisplayRows', () => {
  it('returns [] for an empty thread list', () => {
    expect(buildDisplayRows([], new Set())).toEqual([]);
  });

  it('renders top-level threads at indent 0', () => {
    const rows = buildDisplayRows(
      [t({ id: 'a' }), t({ id: 'b' })],
      new Set(),
    );
    expect(rows.map((r) => r.thread.id)).toEqual(['a', 'b']);
    expect(rows.every((r) => r.indent === 0)).toBe(true);
  });

  it('marks discussion parents and reports child visibility correctly', () => {
    const rows = buildDisplayRows(
      [
        t({ id: 'parent', discussionId: 'def-1' }),
        t({ id: 'child-1', parentThreadId: 'parent' }),
        t({ id: 'loner' }),
      ],
      new Set(),
    );
    const parent = rows.find((r) => r.thread.id === 'parent')!;
    expect(parent.isDiscussionParent).toBe(true);
    expect(parent.hasVisibleChildren).toBe(true);
    expect(parent.expanded).toBe(false);
    // Children are hidden when the parent is collapsed.
    expect(rows.some((r) => r.thread.id === 'child-1')).toBe(false);
  });

  it('emits children under an expanded parent at indent 1', () => {
    const rows = buildDisplayRows(
      [
        t({ id: 'parent', discussionId: 'def-1' }),
        t({ id: 'child-a', parentThreadId: 'parent' }),
        t({ id: 'child-b', parentThreadId: 'parent' }),
        t({ id: 'next-top' }),
      ],
      new Set(['parent']),
    );
    expect(rows.map((r) => [r.thread.id, r.indent])).toEqual([
      ['parent', 0],
      ['child-a', 1],
      ['child-b', 1],
      ['next-top', 0],
    ]);
  });

  it('preserves the input order for top-level threads and children', () => {
    // Explicit input order: root2, child of root1, root1 — children
    // should still be emitted under root1 in their input order.
    const rows = buildDisplayRows(
      [
        t({ id: 'root2' }),
        t({ id: 'child-a', parentThreadId: 'root1' }),
        t({ id: 'root1', discussionId: 'def' }),
        t({ id: 'child-b', parentThreadId: 'root1' }),
      ],
      new Set(['root1']),
    );
    expect(rows.map((r) => r.thread.id)).toEqual(['root2', 'root1', 'child-a', 'child-b']);
  });

  it('treats a child with a filtered-out parent as top-level (orphan)', () => {
    // The parent isn't in the input set — maybe it was filtered by search.
    // The child still shows up so the user can reach it.
    const rows = buildDisplayRows(
      [t({ id: 'orphan', parentThreadId: 'missing-parent' })],
      new Set(),
    );
    expect(rows.length).toBe(1);
    expect(rows[0].thread.id).toBe('orphan');
    expect(rows[0].indent).toBe(0);
  });

  it('non-discussion parents that happen to have children still show them', () => {
    // Discussion parents have `discussionId`, but an arbitrary fork could
    // also have a child via parentThreadId. The grouping is by lineage,
    // not by whether the parent is specifically a discussion.
    const rows = buildDisplayRows(
      [
        t({ id: 'p', /* no discussionId */ }),
        t({ id: 'c', parentThreadId: 'p' }),
      ],
      new Set(['p']),
    );
    expect(rows.map((r) => r.thread.id)).toEqual(['p', 'c']);
    const parent = rows[0];
    expect(parent.isDiscussionParent).toBe(false);
    expect(parent.hasVisibleChildren).toBe(true);
    expect(parent.expanded).toBe(true);
  });

  it('handles a parent with no children (hasVisibleChildren=false)', () => {
    const rows = buildDisplayRows(
      [t({ id: 'lonely', discussionId: 'def' })],
      new Set(['lonely']),
    );
    expect(rows.length).toBe(1);
    expect(rows[0].isDiscussionParent).toBe(true);
    expect(rows[0].hasVisibleChildren).toBe(false);
    // Expanded state only matters when there are children — we report
    // false for parents with none so the UI doesn't draw a down-chevron.
    expect(rows[0].expanded).toBe(false);
  });

  it('is resilient to duplicate children in the input', () => {
    const rows = buildDisplayRows(
      [
        t({ id: 'p', discussionId: 'def' }),
        t({ id: 'c1', parentThreadId: 'p' }),
        t({ id: 'c1', parentThreadId: 'p' }), // duplicate id
      ],
      new Set(['p']),
    );
    // Child is emitted once. A duplicate id would break Svelte's keyed
    // each block — we drop the dupe on the way through.
    const childRows = rows.filter((r) => r.thread.id === 'c1');
    expect(childRows.length).toBe(1);
  });

  it('does not emit a child twice when it would be both a root and a child', () => {
    // Edge case: an input where a thread's parent id points to itself
    // (shouldn't happen, but shouldn't crash either).
    const rows = buildDisplayRows(
      [t({ id: 'self-parent', parentThreadId: 'self-parent' })],
      new Set(),
    );
    // The thread is its own parent → byId.has(parentId) is true → the
    // outer loop skips it → it never renders. Document the behavior
    // rather than leak a row at the wrong level.
    expect(rows.length).toBe(0);
  });
});

describe('defaultExpandedParents', () => {
  it('returns an empty set when there is no active thread', () => {
    expect(defaultExpandedParents([t()], null)).toEqual(new Set());
  });

  it('expands the parent of the currently active child', () => {
    const expanded = defaultExpandedParents(
      [
        t({ id: 'parent', discussionId: 'def' }),
        t({ id: 'child', parentThreadId: 'parent' }),
      ],
      'child',
    );
    expect(expanded).toEqual(new Set(['parent']));
  });

  it('returns an empty set when the active thread is a top-level root', () => {
    const expanded = defaultExpandedParents(
      [t({ id: 'root' })],
      'root',
    );
    expect(expanded).toEqual(new Set());
  });

  it('tolerates an unknown active-thread id', () => {
    const expanded = defaultExpandedParents(
      [t({ id: 'a' })],
      'missing',
    );
    expect(expanded).toEqual(new Set());
  });
});
