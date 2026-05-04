import { describe, expect, it } from 'vitest';
import type { Checkpoint } from '../types/checkpoint';
import { buildRevertAffectedFiles } from './checkpointRevertPreview';

function checkpoint(
  turnCount: number,
  toolPaths: string[],
  files: Checkpoint['files'],
): Checkpoint {
  return {
    id: `c-${turnCount}`,
    threadId: 't-1',
    checkpointTurnCount: turnCount,
    status: 'ready',
    files,
    toolPaths,
    capturedAt: 0,
  };
}

describe('buildRevertAffectedFiles', () => {
  it('returns empty when no target is selected', () => {
    const checkpoints = [
      checkpoint(1, ['a.go'], [{ path: 'a.go', kind: 'modified', additions: 1, deletions: 0 }]),
    ];
    expect(buildRevertAffectedFiles(checkpoints, null)).toEqual([]);
  });

  it('intersects each row\'s files with its toolPaths', () => {
    // Turn 1's files lists a.go AND b.go (b.go is a manual user edit
    // captured by the baseline); only a.go is in toolPaths so only a.go
    // shows in the preview.
    const checkpoints = [
      checkpoint(0, [], []),
      checkpoint(1, ['a.go'], [
        { path: 'a.go', kind: 'modified', additions: 5, deletions: 1 },
        { path: 'b.go', kind: 'modified', additions: 99, deletions: 99 },
      ]),
    ];
    const got = buildRevertAffectedFiles(checkpoints, 0);
    expect(got).toEqual([
      { path: 'a.go', kind: 'modified', additions: 5, deletions: 1 },
    ]);
  });

  it('dedupes by path with the latest turn winning on stats', () => {
    const checkpoints = [
      checkpoint(0, [], []),
      checkpoint(1, ['shared.go'], [{ path: 'shared.go', kind: 'modified', additions: 2, deletions: 0 }]),
      checkpoint(2, ['shared.go'], [{ path: 'shared.go', kind: 'modified', additions: 7, deletions: 3 }]),
    ];
    const got = buildRevertAffectedFiles(checkpoints, 0);
    expect(got).toHaveLength(1);
    expect(got[0]).toEqual({ path: 'shared.go', kind: 'modified', additions: 7, deletions: 3 });
  });

  it('skips rows with empty toolPaths', () => {
    // Turn 1 has files but no toolPaths (legacy or non-agent activity).
    // Should not contribute to the preview.
    const checkpoints = [
      checkpoint(0, [], []),
      checkpoint(1, [], [{ path: 'manual.go', kind: 'modified', additions: 1, deletions: 0 }]),
      checkpoint(2, ['agent.go'], [{ path: 'agent.go', kind: 'modified', additions: 1, deletions: 0 }]),
    ];
    const got = buildRevertAffectedFiles(checkpoints, 0);
    expect(got.map((f) => f.path)).toEqual(['agent.go']);
  });

  it('only considers rows with checkpointTurnCount > target', () => {
    const checkpoints = [
      checkpoint(0, [], []),
      checkpoint(1, ['before.go'], [{ path: 'before.go', kind: 'modified', additions: 1, deletions: 0 }]),
      checkpoint(2, ['after.go'], [{ path: 'after.go', kind: 'modified', additions: 1, deletions: 0 }]),
    ];
    // Reverting to turn 1 should only show what was written in turn 2+.
    const got = buildRevertAffectedFiles(checkpoints, 1);
    expect(got.map((f) => f.path)).toEqual(['after.go']);
  });

  it('returns alphabetically sorted output', () => {
    const checkpoints = [
      checkpoint(0, [], []),
      checkpoint(1, ['z.go', 'a.go', 'm.go'], [
        { path: 'z.go', kind: 'modified', additions: 1, deletions: 0 },
        { path: 'a.go', kind: 'modified', additions: 1, deletions: 0 },
        { path: 'm.go', kind: 'modified', additions: 1, deletions: 0 },
      ]),
    ];
    const got = buildRevertAffectedFiles(checkpoints, 0);
    expect(got.map((f) => f.path)).toEqual(['a.go', 'm.go', 'z.go']);
  });
});
