import type { BackendKey } from '../transport/backendKey';
import { backendById, onBackendDetached, withBackendTarget } from '../transport/backends';
import { composeWorkspaceKey } from '../utils/workspaceKey';
// Codex skills, cached per computer and workspace directory.
//
// Skills are DIRECTORY-scoped — two workspaces have different answers — so the
// cache key is the workspace path, matching the backend's own
// `(binary, cwd)` key. Fetched lazily when a composer command menu opens, never
// on a keystroke: `forceReload` is the user-initiated refresh and would re-walk
// the filesystem on every render.
//
// `GetCodexSkills` requires `threads:operate` on the named computer.
// Check its grant before issuing the RPC, including on paired frontends.

import { GetCodexSkills } from './bindings';
import type { CodexCwdSkills, CodexSkill } from './bindings';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { hasScope } from '../transport/scopes';
import { errString } from '../utils/errors';

export type CodexSkillsStatus = 'unknown' | 'loading' | 'ready' | 'error';

export interface CodexSkillsEntry {
  status: CodexSkillsStatus;
  skills: readonly CodexSkill[];
  /** Populated for `error`; also carries per-directory load errors on `ready`. */
  error: string;
}

const UNKNOWN: CodexSkillsEntry = { status: 'unknown', skills: [], error: '' };

const byWorkspace = createKeyedSignalRegistry<CodexSkillsEntry>(UNKNOWN);
const inFlight = new Map<string, Promise<void>>();
const keys = new Map<string, BackendKey>();
const MAX_WORKSPACES = 128;
onBackendDetached(({ backendId }) => {
  for (const [key, backend] of keys) if (backend === backendId) {
    keys.delete(key); inFlight.delete(key); byWorkspace.drop(key);
  }
});

/** Tracked read for one workspace. `unknown` means nothing has been asked yet. */
export function getCodexSkills(workspacePath: string | null | undefined, backend: BackendKey): CodexSkillsEntry {
  if (!workspacePath) return UNKNOWN;
  return byWorkspace.get(composeWorkspaceKey(backend, workspacePath));
}

/**
 * Load a workspace's skills if they are not already loaded.
 *
 * `forceReload` bypasses BOTH this cache and Codex's own on-disk scan, so it
 * belongs to an explicit user refresh only. Concurrent callers share one
 * request; a rejection is captured into the entry rather than propagated,
 * because every caller is a menu opening rather than an action the user can
 * retry from.
 */
export function ensureCodexSkills(
  workspacePath: string | null | undefined,
  forceReload: boolean,
  backend: BackendKey,
): Promise<void> {
  if (!workspacePath) return Promise.resolve();
  const key = composeWorkspaceKey(backend, workspacePath);
  const target = backendById(backend);
  keys.delete(key);
  keys.set(key, backend);
  if (keys.size > MAX_WORKSPACES) {
    const oldest = keys.keys().next().value!;
    keys.delete(oldest); inFlight.delete(oldest); byWorkspace.drop(oldest);
  }
  // A session without the grant cannot reach the method at all. Answer from
  // here so the menu can hide the section without a round trip that would
  // only be refused.
  if (!hasScope('threads:operate', backend)) {
    byWorkspace.set(key, {
      status: 'error',
      skills: [],
      error: 'This device is not allowed to read Codex skills on this computer.',
    });
    return Promise.resolve();
  }
  const existing = byWorkspace.get(key);
  if (!forceReload && (existing.status === 'ready' || existing.status === 'loading')) {
    return inFlight.get(key) ?? Promise.resolve();
  }
  const pending = inFlight.get(key);
  if (pending && !forceReload) return pending;

  byWorkspace.set(key, {
    status: 'loading',
    skills: existing.skills,
    error: '',
  });
  const request = withBackendTarget(backend, () => GetCodexSkills(workspacePath, forceReload))
    .then((answer: CodexCwdSkills) => {
      if (backendById(backend) !== target || inFlight.get(key) !== request) return;
      // Per-directory load errors ride ALONGSIDE a successful answer: a
      // permissions failure that silently produced a shorter menu would be
      // indistinguishable from having no skills there.
      const loadErrors = (answer?.errors ?? [])
        .map((e) => `${e.path}: ${e.message}`)
        .join('; ');
      byWorkspace.set(key, {
        status: 'ready',
        skills: answer?.skills ?? [],
        error: loadErrors,
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
