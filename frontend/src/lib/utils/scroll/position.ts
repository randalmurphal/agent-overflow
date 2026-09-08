import type { ScrollGrid } from './grid';

/** Engine readbacks may round a grid position again through float32. */
export function scrollReadbackTolerance(grid: ScrollGrid, position: number): number {
  return grid.readbackError + Math.abs(position) * 2 ** -23 + grid.quantum * 1e-6;
}

/** Sample a continuous position without changing its velocity or losing phase.
 * The caller retains modeled minus readback; writeOffset is applied only to
 * the request, after choosing the accepted position. */
export function sampleScrollPosition(modeled: number, current: number, target: number, grid: ScrollGrid): number {
  if (!Number.isFinite(modeled) || !Number.isFinite(current) || !Number.isFinite(target)
    || !Number.isFinite(grid.quantum) || grid.quantum <= 0
    || !Number.isFinite(grid.writeOffset) || !Number.isFinite(grid.readbackError) || grid.readbackError < 0) {
    throw new RangeError('Scroll position sampling requires finite positions and a positive grid');
  }
  // An exact endpoint may lie between accepted positions. Request it only
  // when the continuous path is near it, never merely because a readback is.
  if (Math.abs(modeled - target) <= grid.quantum / 2) return target;
  const nearest = Math.max(0, Math.round(modeled / grid.quantum) * grid.quantum);
  // A zoom change can leave the existing readback between new grid points.
  // Wait for the path to reach the next point instead of moving backwards.
  const sampled = modeled >= current ? Math.max(current, nearest) : Math.min(current, nearest);
  const bounded = current <= target ? Math.min(sampled, target) : Math.max(sampled, target);
  return Math.abs(bounded - current) <= scrollReadbackTolerance(grid, current) ? current : bounded;
}
