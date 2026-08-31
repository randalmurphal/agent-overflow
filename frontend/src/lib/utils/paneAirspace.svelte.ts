// The airspace registry for native browser views.
//
// A pane's embedded browser view is a real OS view that always paints ABOVE
// the SPA, so any AO overlay that would intersect its host rect (popover,
// modal, drawer, palette) must win by the view yielding: the pane hides the
// native view for the overlay's lifetime (spec docs/specs/embedded-browser.md
// §7). DOM ancestry cannot answer "is an overlay open over me" — overlays
// portal or fix-position — so each overlay primitive registers its painted
// root here while it is mounted, and the browser pane intersects against the
// registered elements whenever the registry or its own rect changes.
//
// This is deliberately a registry of ELEMENTS, not booleans: a scrim that
// covers the viewport intersects everything, while a small menu on the other
// side of the window obscures nothing and must not blank an unrelated pane.

let surfaces = $state.raw<readonly HTMLElement[]>([]);

/** Registers one overlay's painted root; returns its release. */
export function registerAirspaceSurface(el: HTMLElement): () => void {
  surfaces = [...surfaces, el];
  return () => {
    surfaces = surfaces.filter((candidate) => candidate !== el);
  };
}

/**
 * Svelte action form: `use:airspaceSurface` on an overlay primitive's painted
 * root. The overlay primitives own the call sites; feature components never
 * register themselves.
 */
export function airspaceSurface(el: HTMLElement): { destroy: () => void } {
  const release = registerAirspaceSurface(el);
  return { destroy: release };
}

/** Reactive read: the currently mounted overlay surfaces. */
export function airspaceSurfaces(): readonly HTMLElement[] {
  return surfaces;
}

/**
 * Whether any registered overlay intersects the given viewport rect. Reads
 * live geometry, so callers re-run it on the events that can move an overlay
 * (registry change, scroll, resize) rather than caching the answer.
 */
export function airspaceIntersects(rect: {
  left: number;
  top: number;
  right: number;
  bottom: number;
}): boolean {
  for (const el of surfaces) {
    const r = el.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) continue;
    if (r.left < rect.right && r.right > rect.left && r.top < rect.bottom && r.bottom > rect.top) {
      return true;
    }
  }
  return false;
}

export function resetAirspaceForTest(): void {
  surfaces = [];
}
