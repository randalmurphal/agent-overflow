import type { BackendKey } from '../transport/backendKey';
import { backendById, onBackendDetached, withBackendTarget } from '../transport/backends';
import { composeWorkspaceKey } from '../utils/workspaceKey';
// Claude skills, cached per computer and workspace directory.
//
// Filesystem-enumerated by the backend (user tier, project tier, enabled
// plugins) because the zero-token probe runs --safe-mode and reports no
// skills: without this read a cold Claude thread's command menu can't list
// them until a session's `system/init` frame arrives. Once that frame
// exists its name set is authoritative — this list only fills the gap
// before it and enriches names with descriptions.
//
// Same shape and lifecycle as `codexSkills.svelte.ts`: keyed by workspace
// path and computer, fetched lazily when a menu opens. The `threads:operate`
// grant is checked before reading provider configuration.

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
const keys = new Map<string, BackendKey>();
const MAX_WORKSPACES = 128;
onBackendDetached(({ backendId }) => {
  for (const [key, backend] of keys) if (backend === backendId) {
    keys.delete(key); inFlight.delete(key); byWorkspace.drop(key);
  }
});

/** Tracked read for one workspace. `unknown` means nothing has been asked yet. */
export function getClaudeSkills(workspacePath: string | null | undefined, backend: BackendKey): ClaudeSkillsEntry {
  if (!workspacePath) return UNKNOWN;
  return byWorkspace.get(composeWorkspaceKey(backend, workspacePath));
}

/**
 * Load a workspace's skills if they are not already loaded. Concurrent
 * callers share one request; a rejection is captured into the entry rather
 * than propagated, because every caller is a menu opening rather than an
 * action the user can retry from.
 */
export function ensureClaudeSkills(workspacePath: string | null | undefined, backend: BackendKey): Promise<void> {
  if (!workspacePath) return Promise.resolve();
  const key = composeWorkspaceKey(backend, workspacePath);
  const target = backendById(backend);
  keys.delete(key);
  keys.set(key, backend);
  if (keys.size > MAX_WORKSPACES) {
    const oldest = keys.keys().next().value!;
    keys.delete(oldest); inFlight.delete(oldest); byWorkspace.drop(oldest);
  }
  if (!hasScope('threads:operate', backend)) {
    byWorkspace.set(key, {
      status: 'error',
      skills: [],
      error: 'This device is not allowed to read Claude skills on this computer.',
    });
    return Promise.resolve();
  }
  const existing = byWorkspace.get(key);
  if (existing.status === 'ready' || existing.status === 'loading') {
    return inFlight.get(key) ?? Promise.resolve();
  }

  byWorkspace.set(key, {
    status: 'loading',
    skills: existing.skills,
    error: '',
  });
  const request = withBackendTarget(backend, () => GetClaudeSkills(workspacePath))
    .then((answer: ClaudeSkill[] | null) => {
      if (backendById(backend) !== target || inFlight.get(key) !== request) return;
      byWorkspace.set(key, {
        status: 'ready',
        skills: answer ?? [],
        error: '',
      });
    })
    .catch((err: unknown) => {
      if (backendById(backend) !== target || inFlight.get(key) !== request) return;
      byWorkspace.set(key, {
        status: 'error',
        skills: [],
        error: errString(err),
      });
    })
    .finally(() => {
      if (inFlight.get(key) === request) inFlight.delete(key);
    });
  inFlight.set(key, request);
  return request;
}

/** Test-only fixture isolation. */
export function resetForTest(): void {
  byWorkspace.reset();
  inFlight.clear();
  keys.clear();
}
