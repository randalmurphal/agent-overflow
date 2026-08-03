// App-wide motion registry for the content-lease demote deferral.
//
// A lease demotion (chokepoint.ts) re-rasters the pane's text. On a
// quiet compositor that completes inside one vsync and is invisible;
// while ANOTHER pane is streaming, the raster threads are contended
// and the same re-raster smears across frames as a visible shimmer —
// the 2026-08-03 incident: an idle pane's demote (armed 5s earlier by
// a review-pane close) fired mid-stream of its neighbor and flickered
// everything below the reader's pointer. A controller cannot see its
// neighbors through its own deps, so controllers register a motion
// probe here and the lease consults the union before demoting.
//
// Deliberately not reactive state: probes are read inside a timer
// callback at a 250ms cadence, and the inputs (spring liveness, the
// live-content hold) are plain closures already. A Svelte store here
// would buy nothing and put this package's purity at risk.

const probes = new Set<() => boolean>();

/**
 * Register a controller's motion probe (true while its spring is
 * active/armed or its surface holds live streaming content). Returns
 * the release; callers pair it with their detach path — a probe left
 * behind by a detached controller reads false forever but still costs
 * a call per demote check, and enough of them is a leak.
 */
export function registerAppMotionProbe(probe: () => boolean): () => void {
  probes.add(probe);
  return () => {
    probes.delete(probe);
  };
}

/** True if any registered surface is currently in motion. */
export function appMotionActive(): boolean {
  for (const probe of probes) {
    if (probe()) return true;
  }
  return false;
}
