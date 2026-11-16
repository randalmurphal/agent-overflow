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
//
// Two kinds of surface register here. An overlay (the default) is a surface
// the reader dismisses; `airspaceSurfaces()` lists overlays only, because
// command gates and Back read its length as "an overlay is open". A layer
// (`overlay: false`, the promoted pane) occludes views beneath it but is
// not something to dismiss. Either may supply its own painted rect when its
// box is not what it paints (a clip-path reveal), and neither occludes a
// view inside itself.

export interface AirspaceRect {
  left: number;
  top: number;
  right: number;
  bottom: number;
}

export interface AirspaceSurfaceOptions {
  /** The rect the surface paints now, in viewport px; null paints nothing.
   * Defaults to its bounding box. */
  rect?: () => AirspaceRect | null;
  /** False for a layer that is not a dismissable overlay. Default true. */
  overlay?: boolean;
}

interface Entry {
  el: HTMLElement;
  rect: (() => AirspaceRect | null) | null;
}

let surfaces = $state.raw<readonly HTMLElement[]>([]);
let entries: readonly Entry[] = [];
let layers: readonly Entry[] = [];

/** Registers one surface's painted root; returns its release. */
export function registerAirspaceSurface(el: HTMLElement, options?: AirspaceSurfaceOptions): () => void {
  const entry: Entry = { el, rect: options?.rect ?? null };
  if (options?.overlay === false) {
    layers = [...layers, entry];
    return () => {
      layers = layers.filter((candidate) => candidate !== entry);
    };
  }
  entries = [...entries, entry];
  surfaces = [...surfaces, el];
  return () => {
    if (!entries.includes(entry)) return;
    entries = entries.filter((candidate) => candidate !== entry);
    // Drop one occurrence: the same element may be registered twice.
    const index = surfaces.indexOf(el);
    if (index >= 0) surfaces = [...surfaces.slice(0, index), ...surfaces.slice(index + 1)];
  };
}

/**
 * Svelte action form: `use:airspaceSurface` on an overlay primitive's painted
 * root. The overlay primitives and the pane strip's drag shield own the call
 * sites; feature components never register themselves.
 */
export function airspaceSurface(el: HTMLElement): { destroy: () => void } {
  const release = registerAirspaceSurface(el);
  return { destroy: release };
}

/** Reactive read: the currently mounted overlay surfaces (layers excluded). */
export function airspaceSurfaces(): readonly HTMLElement[] {
  return surfaces;
}

function paintedRect(entry: Entry): AirspaceRect | null {
  if (entry.rect) return entry.rect();
  return entry.el.getBoundingClientRect();
}

function occludes(entry: Entry, rect: AirspaceRect, subject: Node | null): boolean {
  if (subject && entry.el.contains(subject)) return false;
  const r = paintedRect(entry);
  if (!r || r.right - r.left <= 0 || r.bottom - r.top <= 0) return false;
  return r.left < rect.right && r.right > rect.left && r.top < rect.bottom && r.bottom > rect.top;
}

/**
 * Whether any registered surface intersects the given viewport rect. Reads
 * live geometry, so callers re-run it on the events that can move a surface
 * (registry change, scroll, resize, `promotedLayerFrame`) rather than
 * caching the answer. `subject` is the element the rect belongs to: a
 * surface that contains it does not occlude it.
 */
export function airspaceIntersects(rect: AirspaceRect, subject?: Node | null): boolean {
  const own = subject ?? null;
  for (const entry of entries) if (occludes(entry, rect, own)) return true;
  for (const entry of layers) if (occludes(entry, rect, own)) return true;
  return false;
}

export function resetAirspaceForTest(): void {
  surfaces = [];
  entries = [];
  layers = [];
}
