// Per-parent expansion state for the sidebar's nested thread view.
// Module-level so the sidebar's thread list can read it from any place
// the tree is computed. State is intentionally simple: a set of parent
// ids currently expanded. The tree-builder in utils/threadTree.ts keeps
// its own sorting / flattening logic pure; this store is just the toggle
// surface the row components drive.

let expanded: Set<string> = $state(new Set());

export function getExpandedParents(): Set<string> {
  return expanded;
}

export function isParentExpanded(id: string): boolean {
  return expanded.has(id);
}

export function expandParent(id: string): void {
  if (expanded.has(id)) return;
  expanded = new Set(expanded).add(id);
}

export function collapseParent(id: string): void {
  if (!expanded.has(id)) return;
  const next = new Set(expanded);
  next.delete(id);
  expanded = next;
}

export function toggleParent(id: string): void {
  if (expanded.has(id)) collapseParent(id);
  else expandParent(id);
}

/**
 * Merge-in a set of parent ids that should be considered expanded (e.g.
 * the parent of the currently-active thread). Won't collapse anything
 * already expanded; additive only.
 */
export function ensureExpanded(ids: Iterable<string>): void {
  const next = new Set(expanded);
  let changed = false;
  for (const id of ids) {
    if (!next.has(id)) {
      next.add(id);
      changed = true;
    }
  }
  if (changed) expanded = next;
}

/** Test helper — clears state between tests. */
export function resetExpandedParentsForTest(): void {
  expanded = new Set();
}
