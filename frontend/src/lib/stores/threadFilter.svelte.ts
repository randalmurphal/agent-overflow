// Sidebar thread filter + multi-select state. Lives outside the sidebar
// component so the command palette can imperatively focus the search box or
// jump to a thread by index even when the sidebar isn't mounted.

let query = $state('');
let workspaceFilter = $state<string | null>(null);
let selected: Set<string> = $state(new Set());

export function getThreadFilterQuery(): string {
  return query;
}

export function setThreadFilterQuery(next: string): void {
  query = next;
}

export function getWorkspaceFilter(): string | null {
  return workspaceFilter;
}

export function setWorkspaceFilter(next: string | null): void {
  if (workspaceFilter === next) return;
  workspaceFilter = next;
  // A workspace filter change can hide previously-selected threads,
  // so clear the selection.
  selected = new Set();
}

export function getSelectedThreadIds(): Set<string> {
  return selected;
}

export function isThreadSelected(id: string): boolean {
  return selected.has(id);
}

export function toggleThreadSelection(id: string): void {
  const next = new Set(selected);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  selected = next;
}

export function setThreadSelection(ids: Iterable<string>): void {
  selected = new Set(ids);
}

export function clearThreadSelection(): void {
  if (selected.size === 0) return;
  selected = new Set();
}

