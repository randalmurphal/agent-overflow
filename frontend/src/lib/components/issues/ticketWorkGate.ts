// Where a ticket's Ask and Start work make their thread, and why they
// cannot run now: the one rule for the ticket's Work section and the Board
// card's menu, which load nothing of Work's own code to say it.

import { getProjects } from '../../stores/projects.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { projectBackend } from '../../transport/entityIndex';
import { grantBlockedTitle } from '../../transport/scopeRefusal';
import { scopeGrant } from '../../transport/scopes';
import type { Project } from '../../types/models';

/** The projects of the ticket's computer `backend` a thread of the ticket
 * can be made in; the ticket's own (`ticketProject`) shows even when
 * archived. Reactive. */
export function workProjects(backend: string, ticketProject: string | null): Project[] {
  return getProjects().map((p) => p.project)
    .filter((p) => (!p.archived || p.id === ticketProject) && (projectBackend(p.id) ?? HOME_BACKEND) === backend);
}

/** The project Ask and Start work make their thread in: the ticket's when
 * `projects` lists it, else none (''). */
export function workProject(projects: readonly Project[], ticketProject: string | null): string {
  return ticketProject && projects.some((p) => p.id === ticketProject) ? ticketProject : '';
}

/** Why Ask and Start work cannot run, '' for each that can, and what this
 * device's grants leave out ('' for nothing). */
export interface WorkGate {
  readonly ask: string;
  readonly start: string;
  readonly note: string;
}

/** Ask and Start work on the ticket's computer `backend` with `project`
 * ('' for none): a thread is made under threads:operate, and Start work's
 * worktree under git:operate. Reactive. */
export function workGate(backend: string, project: string): WorkGate {
  const unplaced = project ? '' : 'Pick the project its thread goes in.';
  const threads = scopeGrant('threads:operate', backend);
  const git = scopeGrant('git:operate', backend);
  const start = threads === 'granted' ? git : threads;
  return {
    ask: grantBlockedTitle(threads) ?? unplaced,
    start: grantBlockedTitle(start) ?? unplaced,
    note: threads === 'refused'
      ? 'This device may not start threads on this computer.'
      : git === 'refused' ? 'This device may not use git on this computer. Ask still works.' : '',
  };
}
