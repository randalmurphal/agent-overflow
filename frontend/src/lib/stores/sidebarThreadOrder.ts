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
