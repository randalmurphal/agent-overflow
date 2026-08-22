export interface VirtualPlaneGeometry {
  /** Global content-space offset of the mounted plane. */
  origin: number;
  /** Local extent below the origin. Rows prepended while an anchor is held
   * may temporarily paint into the plane's visible negative overflow. */
  size: number;
}

export interface GlobalPlaneRow<Key> {
  key: Key;
  offset: number;
  size: number;
}

export interface VirtualPlaneState<Key> {
  anchorKey: Key | null;
  localOffsets: ReadonlyMap<Key, number>;
}

export interface VirtualPlaneProjection<Key> {
  geometry: VirtualPlaneGeometry;
  anchorKey: Key | null;
  localOffsets: ReadonlyMap<Key, number>;
}

/**
 * Projects global engine rows into one mounted paint plane. A structural
 * keyed mutation captures the last surviving mounted row as the plane anchor.
 * While that row remains mounted, its local coordinate stays fixed and the
 * plane origin absorbs global head-splice and measurement compensation.
 * Once it leaves the overscan window, the projection rebases around the new
 * window. The retired anchor is already offscreen at that point.
 */
export function projectVirtualPlane<Key>(
  rows: readonly GlobalPlaneRow<Key>[],
  previous: VirtualPlaneState<Key>,
  captureSurvivor: boolean,
): VirtualPlaneProjection<Key> {
  if (rows.length === 0) {
    return {
      geometry: { origin: 0, size: 0 },
      anchorKey: null,
      localOffsets: new Map(),
    };
  }

  let anchorKey = previous.anchorKey;
  let anchor = anchorKey === null ? undefined : rows.find((row) => row.key === anchorKey);
  let anchorLocal = anchorKey === null ? undefined : previous.localOffsets.get(anchorKey);

  if (captureSurvivor) {
    for (let index = rows.length - 1; index >= 0; index--) {
      const candidate = rows[index];
      const local = previous.localOffsets.get(candidate.key);
      if (local === undefined) continue;
      anchorKey = candidate.key;
      anchor = candidate;
      anchorLocal = local;
      break;
    }
  }

  if (!anchor || anchorLocal === undefined) {
    anchorKey = null;
    anchor = undefined;
    anchorLocal = undefined;
  }

  const origin = anchor ? anchor.offset - anchorLocal! : rows[0].offset;
  const localOffsets = new Map<Key, number>();
  let size = 0;
  for (const row of rows) {
    const local = row.offset - origin;
    localOffsets.set(row.key, local);
    size = Math.max(size, local + row.size);
  }
  return {
    geometry: { origin, size: Math.max(0, size) },
    anchorKey,
    localOffsets,
  };
}
