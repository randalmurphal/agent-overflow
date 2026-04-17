// Flattens a thread list into a rendered-row sequence that preserves the
// discussion parent → child relationship. Children follow their parent in
// document order, indented by one level. When a parent is collapsed, its
// children are hidden.
//
// Stays a pure function so the sidebar can drive it from reactive state
// and so it's easy to unit-test without mounting the whole component tree.
//
// The row shape is fixed-height compatible — indent is purely visual, so
// the existing VirtualList doesn't need variable-height support.

import type { Thread } from '../types/models';

export interface ThreadDisplayRow {
  thread: Thread;
  /** 0 = top-level, 1 = direct child of a discussion parent. */
  indent: number;
  /** True when this row is a discussion parent. */
  isDiscussionParent: boolean;
  /** True when this parent has at least one visible child in the input set. */
  hasVisibleChildren: boolean;
  /** Whether this parent is currently expanded. False for non-parents. */
  expanded: boolean;
}

/**
 * Build the ordered, indented row list. `threads` is expected to already be
 * filtered / sorted however the sidebar wants them ordered at the top level;
 * this function preserves that order for roots, inserting children
 * immediately after their parent.
 *
 * A "discussion parent" is a thread with `discussionId` populated. Any
 * thread whose `parentThreadId` appears in the input set is treated as a
 * child of that parent. Orphan children (parent filtered out of the input)
 * render at indent 0 — they're legitimate threads the user should still
 * be able to reach, just without their lineage visible.
 */
export function buildDisplayRows(
  threads: Thread[],
  expandedParentIds: Set<string>,
): ThreadDisplayRow[] {
  if (threads.length === 0) return [];

  // Index for O(1) parent lookups.
  const byId = new Map<string, Thread>();
  for (const t of threads) byId.set(t.id, t);

  // Group children by their parent id. Preserves the incoming order of
  // children within a group, which lets callers pre-sort participants
  // however they like (e.g., by creation time) before calling us.
  const childrenByParent = new Map<string, Thread[]>();
  for (const t of threads) {
    if (!t.parentThreadId) continue;
    if (!byId.has(t.parentThreadId)) continue; // orphan — surface at root
    const bucket = childrenByParent.get(t.parentThreadId) ?? [];
    bucket.push(t);
    childrenByParent.set(t.parentThreadId, bucket);
  }

  const rows: ThreadDisplayRow[] = [];
  const emittedChildIds = new Set<string>();

  for (const thread of threads) {
    // Children get emitted when their parent is processed. Skip them on
    // the outer pass unless they're orphans (parent filtered out).
    if (thread.parentThreadId && byId.has(thread.parentThreadId)) {
      continue;
    }

    const isDiscussionParent = !!thread.discussionId;
    const kids = childrenByParent.get(thread.id) ?? [];
    const hasVisibleChildren = kids.length > 0;
    const expanded = hasVisibleChildren && expandedParentIds.has(thread.id);

    rows.push({
      thread,
      indent: 0,
      isDiscussionParent,
      hasVisibleChildren,
      expanded,
    });

    if (expanded) {
      for (const child of kids) {
        if (emittedChildIds.has(child.id)) continue;
        emittedChildIds.add(child.id);
        rows.push({
          thread: child,
          indent: 1,
          isDiscussionParent: false,
          hasVisibleChildren: false,
          expanded: false,
        });
      }
    }
  }

  return rows;
}

/**
 * Convenience: returns the set of parent ids that should be expanded by
 * default when the sidebar first renders. Currently: any parent whose
 * child is the active thread. The caller layers user overrides on top.
 */
export function defaultExpandedParents(
  threads: Thread[],
  activeThreadId: string | null,
): Set<string> {
  const expanded = new Set<string>();
  if (!activeThreadId) return expanded;
  for (const t of threads) {
    if (t.id === activeThreadId && t.parentThreadId) {
      expanded.add(t.parentThreadId);
    }
  }
  return expanded;
}
