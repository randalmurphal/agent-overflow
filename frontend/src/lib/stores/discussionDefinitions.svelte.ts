// The persisted discussion DEFINITIONS, as a revision counter and nothing
// else.
//
// There is no list here on purpose. Two surfaces read the definitions and each
// reads a different slice of them (the settings editor reads both scopes and
// keeps its own selection; the start flow reads one project's), so a shared
// cache would be a third projection to keep in step with two that already
// work. What was missing was only the SIGNAL: create, rename, edit and delete
// all persisted and answered their own caller, so a definition written on one
// device never appeared on another until that screen was reopened.
//
// The counter is the whole store. A surface reads it in an `$effect` and
// re-runs its own load; `discussion:definitions-changed` carries no payload
// for the same reason, since a rename moves a definition between names and a
// frame naming one row could not say what any reader's list now holds.

let revision = $state(0);

/** Reactive read. Changes once per definition write, from any client. */
export function discussionDefinitionsRevision(): number {
  return revision;
}

/** `discussion:definitions-changed` — called by events.ts. */
export function applyDiscussionDefinitionsChanged(): void {
  revision += 1;
}

/** Test seam: back to a fresh module load. */
export function resetDiscussionDefinitionsForTest(): void {
  revision = 0;
}
