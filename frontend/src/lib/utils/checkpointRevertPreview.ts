import type { Checkpoint } from '../types/checkpoint';

/**
 * Row in the RevertDialog preview list. Mirrors the shape exposed to
 * `RevertDialog.svelte`'s `affectedFiles` prop.
 */
export interface RevertAffectedFile {
  path: string;
  kind: string;
  additions: number;
  deletions: number;
}

/**
 * Files the agent wrote across every turn AFTER the target checkpoint.
 * Drives the RevertDialog preview so the user sees the exact set the
 * path-scoped restore will roll back.
 *
 * Built by intersecting each post-target row's `files` summary with
 * that row's `toolPaths`, then deduping on path with the latest
 * additions/deletions count winning (so a file edited in turns 2 and 5
 * shows turn 5's stats — what the user is about to undo). Sort is
 * locale-stable so the preview is deterministic across reverts.
 *
 * Returns an empty array when no agent activity has been recorded
 * after the target — either legitimately (the agent did nothing
 * file-mutating since the checkpoint) or because the rows pre-date
 * tool-path tracking. The dialog hides the preview when the array is
 * empty.
 */
export function buildRevertAffectedFiles(
  checkpoints: Checkpoint[],
  targetTurnCount: number | null,
): RevertAffectedFile[] {
  if (targetTurnCount === null) return [];
  const byPath = new Map<string, RevertAffectedFile>();
  for (const cp of checkpoints) {
    if (cp.checkpointTurnCount <= targetTurnCount) continue;
    const tp = new Set(cp.toolPaths ?? []);
    if (tp.size === 0) continue;
    for (const file of cp.files ?? []) {
      if (!tp.has(file.path)) continue;
      byPath.set(file.path, {
        path: file.path,
        kind: file.kind,
        additions: file.additions,
        deletions: file.deletions,
      });
    }
  }
  return [...byPath.values()].sort((a, b) => a.path.localeCompare(b.path));
}
