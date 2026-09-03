// Claude skills, cached per workspace directory.
//
// Filesystem-enumerated by the backend (user tier, project tier, enabled
// plugins) because the zero-token probe runs --safe-mode and reports no
// skills: without this read a cold Claude thread's command menu can't list
// them until a session's `system/init` frame arrives. Once that frame
// exists its name set is authoritative — this list only fills the gap
// before it and enriches names with descriptions.
//
// Same shape and lifecycle as `codexSkills.svelte.ts`: keyed by workspace
// path, fetched lazily when a menu opens, LOCAL-ONLY on the wire so a
// view-only client answers from here without a doomed round trip.

import { GetClaudeSkills } from './bindings';
import type { ClaudeSkill } from './bindings';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { hasScope } from '../transport/scopes';
import { errString } from '../utils/errors';

export type ClaudeSkillsStatus = 'unknown' | 'loading' | 'ready' | 'error';

export interface ClaudeSkillsEntry {
  status: ClaudeSkillsStatus;
  skills: readonly ClaudeSkill[];
  error: string;
}

const UNKNOWN: ClaudeSkillsEntry = { status: 'unknown', skills: [], error: '' };

const byWorkspace = createKeyedSignalRegistry<ClaudeSkillsEntry>(UNKNOWN);
const inFlight = new Map<string, Promise<void>>();

/** Tracked read for one workspace. `unknown` means nothing has been asked yet. */
export function getClaudeSkills(workspacePath: string | null | undefined): ClaudeSkillsEntry {
  if (!workspacePath) return UNKNOWN;
  return byWorkspace.get(workspacePath);
}

/**
 * Load a workspace's skills if they are not already loaded. Concurrent
 * callers share one request; a rejection is captured into the entry rather
 * than propagated, because every caller is a menu opening rather than an
 * action the user can retry from.
 */
export function ensureClaudeSkills(workspacePath: string | null | undefined): Promise<void> {
  if (!workspacePath) return Promise.resolve();
  if (!hasScope('threads:operate')) {
    byWorkspace.set(workspacePath, {
      status: 'error',
      skills: [],
      error: 'Claude skills are only available on the local app.',
    });
    return Promise.resolve();
  }
  const existing = byWorkspace.get(workspacePath);
  if (existing.status === 'ready' || existing.status === 'loading') {
    return inFlight.get(workspacePath) ?? Promise.resolve();
  }

  byWorkspace.set(workspacePath, {
    status: 'loading',
    skills: existing.skills,
    error: '',
  });
  const request = GetClaudeSkills(workspacePath)
    .then((answer: ClaudeSkill[] | null) => {
      byWorkspace.set(workspacePath, {
        status: 'ready',
        skills: answer ?? [],
        error: '',
      });
    })
    .catch((err: unknown) => {
      byWorkspace.set(workspacePath, {
        status: 'error',
        skills: [],
        error: errString(err),
      });
    })
    .finally(() => {
      if (inFlight.get(workspacePath) === request) inFlight.delete(workspacePath);
    });
  inFlight.set(workspacePath, request);
  return request;
}

/** Test-only fixture isolation. */
export function resetForTest(): void {
  byWorkspace.reset();
  inFlight.clear();
}
