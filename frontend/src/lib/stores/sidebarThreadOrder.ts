/**
 * Sidebar navigation order is owned by the rendered sidebar tree: it already
 * accounts for project grouping, collapsed active-thread pins, filters, and
 * truncation. Keep the DOM query isolated here so pane-focused navigation code
 * does not duplicate the sidebar's sorting rules.
 */
export function getVisibleSidebarThreadIds(
  root: ParentNode | null = typeof document === 'undefined' ? null : document,
): string[] {
  if (!root) return [];
  return Array.from(root.querySelectorAll<HTMLElement>('[data-sidebar-thread-id]'))
    .map((el) => el.dataset.sidebarThreadId)
    .filter((id): id is string => !!id);
}

export const SIDEBAR_JUMP_LIMIT = 9;

/** Numbered jumps count only front-burner pin targets, across projects. */
export function getSidebarJumpThreadIds(
  root: ParentNode | null = typeof document === 'undefined' ? null : document,
): string[] {
  if (!root) return [];
  const ids = new Set<string>();
  for (const row of root.querySelectorAll<HTMLElement>(
    '[data-sidebar-thread-id][data-sidebar-jump-target]',
  )) {
    const id = row.dataset.sidebarThreadId;
    if (!id) continue;
    ids.add(id);
    if (ids.size === SIDEBAR_JUMP_LIMIT) break;
  }
  return [...ids];
}
