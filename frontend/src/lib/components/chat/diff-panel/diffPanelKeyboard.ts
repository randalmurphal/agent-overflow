// Diff panel keyboard dispatch. Left/Right arrow stepping through the
// checkpoint list — extracted so DiffPanelDrawer.svelte stays under the
// 300-line guideline.
//
// The real handler lives in the component because it needs to wire a
// window-level listener; this module provides the pure "given the
// keystroke, what should happen?" logic. Returns the target turn index
// to select, or null for no-op.

import type { Checkpoint } from '../../../types/checkpoint';

export interface DiffPanelKeyArgs {
  key: string;
  metaKey: boolean;
  ctrlKey: boolean;
  altKey: boolean;
  /** The DOM target; used to ignore keystrokes typed into form controls. */
  targetTag: string | undefined;
  /** Only 'turn' source handles arrow-key stepping. */
  source: 'turn' | 'worktree' | 'cumulative';
  panelOpen: boolean;
  checkpoints: Checkpoint[];
  selectedTurnIndex: number | null;
}

/**
 * Return the `turnIndex` to select on the current keystroke, or `null`
 * when the key doesn't apply (closed panel, different source tab,
 * typing into an input, wrong key, no-op move, etc).
 */
export function selectTurnForKey(args: DiffPanelKeyArgs): number | null {
  if (!args.panelOpen) return null;
  if (args.source !== 'turn') return null;
  if (args.targetTag === 'INPUT' || args.targetTag === 'TEXTAREA' || args.targetTag === 'SELECT') {
    return null;
  }
  if (args.metaKey || args.ctrlKey || args.altKey) return null;
  if (args.key !== 'ArrowLeft' && args.key !== 'ArrowRight') return null;
  if (args.checkpoints.length === 0) return null;

  const idx = args.checkpoints.findIndex((c) => c.turnIndex === args.selectedTurnIndex);
  const nextIdx =
    args.key === 'ArrowLeft'
      ? idx <= 0
        ? 0
        : idx - 1
      : idx === -1
        ? 0
        : Math.min(args.checkpoints.length - 1, idx + 1);
  const target = args.checkpoints[nextIdx];
  if (!target) return null;
  if (target.turnIndex === args.selectedTurnIndex) return null;
  return target.turnIndex;
}
