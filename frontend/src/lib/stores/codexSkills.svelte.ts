// Codex skills, cached per workspace directory.
//
// Skills are DIRECTORY-scoped — two workspaces have different answers — so the
// cache key is the workspace path, matching the backend's own
// `(binary, cwd)` key. Fetched lazily when a composer command menu opens, never
// on a keystroke: `forceReload` is the user-initiated refresh and would re-walk
// the filesystem on every render.
//
// `GetCodexSkills` is LOCAL-ONLY on the wire. A view-only (remote) client's
// call is refused by the transport, so the failure is expected rather than
// exceptional: the entry lands in `error` state, the menu hides its skills
// section, and nothing rejects unhandled.

import { GetCodexSkills } from './bindings';
import type { CodexCwdSkills, CodexSkill } from './bindings';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { isViewOnlySession } from '../transport/runMode';
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

/** Tracked read for one workspace. `unknown` means nothing has been asked yet. */
export function getCodexSkills(workspacePath: string | null | undefined): CodexSkillsEntry {
  if (!workspacePath) return UNKNOWN;
  return byWorkspace.get(workspacePath);
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
  forceReload = false,
): Promise<void> {
  if (!workspacePath) return Promise.resolve();
  // A remote client cannot reach a local-only method at all. Answer from here
  // so the menu can hide the section without a round trip that would reject.
  if (isViewOnlySession()) {
    byWorkspace.set(workspacePath, {
      status: 'error',
      skills: [],
      error: 'Codex skills are only available on the local app.',
    });
    return Promise.resolve();
  }
  const existing = byWorkspace.get(workspacePath);
  if (!forceReload && (existing.status === 'ready' || existing.status === 'loading')) {
    return inFlight.get(workspacePath) ?? Promise.resolve();
  }
  const pending = inFlight.get(workspacePath);
  if (pending && !forceReload) return pending;

  byWorkspace.set(workspacePath, {
    status: 'loading',
    skills: existing.skills,
    error: '',
  });
  const request = GetCodexSkills(workspacePath, forceReload)
    .then((answer: CodexCwdSkills) => {
      // Per-directory load errors ride ALONGSIDE a successful answer: a
      // permissions failure that silently produced a shorter menu would be
      // indistinguishable from having no skills there.
      const loadErrors = (answer?.errors ?? [])
        .map((e) => `${e.path}: ${e.message}`)
        .join('; ');
      byWorkspace.set(workspacePath, {
        status: 'ready',
        skills: answer?.skills ?? [],
        error: loadErrors,
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
