// The group a status's mark draws (StatusMark), on every surface that
// draws one: the group the project's status groups put it in, else the
// tracker's. A status the project leaves out of counts has no group of its
// own to draw, so it keeps the tracker's. The counts' rule beside it is
// `trackerBoardModel.ts#groupOf`. This module stays small and pure: the
// thread rows and the thread header draw a mark at startup, before any
// Issues store loads.

/** Each status the project puts in another group than the tracker's, by
 * status ID, with that group's name: a board's `regrouped`, or a site's
 * entry of the project's setup (`trackerSetup.svelte.ts#trackerRegroupsOf`). */
export type Regroups = ReadonlyMap<string, string>;

/** The group `status`'s mark draws under `regroups`; none for a status
 * the tracker names no group of. */
export function markGroup(regroups: Regroups | undefined, status: { readonly id: string; readonly group?: string }): string | undefined {
  const own = regroups?.get(status.id);
  return own && own !== 'left_out' ? own : status.group || undefined;
}
