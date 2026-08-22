import { describe, expect, it } from 'vitest';
import { projectVirtualPlane, type VirtualPlaneState } from './plane';

describe('projectVirtualPlane', () => {
  const empty: VirtualPlaneState<string> = { anchorKey: null, localOffsets: new Map() };

  it('bounds the plane to the mounted range', () => {
    const projected = projectVirtualPlane([
      { key: 'b', offset: 40, size: 60 },
      { key: 'c', offset: 100, size: 80 },
    ], empty, false);
    expect(projected.geometry).toEqual({ origin: 40, size: 140 });
    expect([...projected.localOffsets]).toEqual([['b', 0], ['c', 60]]);
  });

  it('returns an empty origin for an empty range', () => {
    expect(projectVirtualPlane([], empty, false).geometry).toEqual({ origin: 0, size: 0 });
  });

  it('keeps the last surviving row local across a structural head shift', () => {
    const before = projectVirtualPlane([
      { key: 'a', offset: 4700, size: 100 },
      { key: 'tail', offset: 5900, size: 100 },
    ], empty, false);
    const after = projectVirtualPlane([
      { key: 'b', offset: 2600, size: 100 },
      { key: 'tail', offset: 3900, size: 100 },
      { key: 'new', offset: 4000, size: 100 },
    ], before, true);

    expect(before.localOffsets.get('tail')).toBe(1200);
    expect(after.localOffsets.get('tail')).toBe(1200);
    expect(after.geometry.origin).toBe(2700);
    expect(after.anchorKey).toBe('tail');
  });

  it('rebases after the retained anchor leaves the mounted window', () => {
    const retained: VirtualPlaneState<string> = {
      anchorKey: 'old',
      localOffsets: new Map([['old', 1200]]),
    };
    const after = projectVirtualPlane([
      { key: 'new-a', offset: 200, size: 100 },
      { key: 'new-b', offset: 300, size: 100 },
    ], retained, false);

    expect(after.anchorKey).toBeNull();
    expect(after.geometry).toEqual({ origin: 200, size: 200 });
  });
});
