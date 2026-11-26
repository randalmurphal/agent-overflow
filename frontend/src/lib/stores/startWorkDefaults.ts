// A project's Start work defaults as its setup file holds them
// (trackerapp.StartWorkDefaults): where an entry sits, and the one rule
// that writes one, which Start work's "Save as defaults" and the Starting
// work page both change it through, so each writes an entry the other
// reads the same way.

import type { Setup, StartWorkDefaults } from '../../../bindings/agent-overflow/internal/trackerapp/models';

/** The Start work defaults' place in a project's setup file: the tracker
 * site and the tracker project, named by the key's prefix. */
export interface DefaultsPlace {
  readonly tracker: string;
  readonly site: string;
  readonly project: string;
}

/** The entry of the setup for `place`, if any. */
export function defaultsAt(setup: Setup, place: DefaultsPlace): StartWorkDefaults | null {
  return (setup.start_work ?? []).find((d) => d.tracker === place.tracker && d.site === place.site && d.project === place.project) ?? null;
}

/** A change of one entry; a field left out keeps what the entry holds. */
export interface DefaultsChange {
  /** The status to move to; null for no move. */
  readonly status?: string | null;
  readonly assign?: boolean;
  /** The branch template; null keeps the stored one. */
  readonly branch?: string | null;
  /** Whether Start work offers the branch named after the ticket. */
  readonly nameBranch?: boolean;
  /** The app project the thread runs in. */
  readonly repository?: string;
}

/** `setup` with the entry at `place` changed by `change`, in the setup of
 * app project `project`: a repository that is `project` itself, and
 * naming the branch after the ticket, are stored as absent (the built-in
 * defaults), and the other entries are kept. */
export function withStartWorkDefaults(setup: Setup, place: DefaultsPlace, project: string, change: DefaultsChange): Setup {
  const kept = defaultsAt(setup, place);
  const entry: StartWorkDefaults = { ...(kept ?? place) };
  if (change.status !== undefined) entry.status = change.status ?? undefined;
  if (change.assign !== undefined) entry.assign = change.assign;
  if (change.branch != null) entry.branch = change.branch;
  if (change.nameBranch !== undefined) entry.name_branch = change.nameBranch ? undefined : false;
  if (change.repository !== undefined) entry.repository = change.repository === project ? undefined : change.repository;
  const others = (setup.start_work ?? []).filter((d) => d !== kept);
  return { ...setup, start_work: [...others, entry] };
}
