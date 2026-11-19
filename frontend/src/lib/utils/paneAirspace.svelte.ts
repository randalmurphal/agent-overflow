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
// (`overlay: false`) occludes views beneath it but is not something to
// dismiss: a promoted pane, the toast stack, a field's suggestion list that
// leaves the keyboard with the field. Either may supply its own
// painted rect when its box is not what it paints (a clip-path reveal), and
// neither occludes a view inside itself. `airspaceRevision()` changes whenever either kind
// registers, releases or changes size, which is when a view must be judged
// again.

import { untrack } from 'svelte';

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
let revision = $state(0);
let entries: readonly Entry[] = [];
let layers: readonly Entry[] = [];

// Registration runs inside actions and effects; the bump must not make the
// caller depend on the revision it writes.
function bump(): void {
  untrack(() => {
    revision += 1;
  });
}

// One observer for every registered surface: a surface that grows or
// shrinks in place (a toast added to the stack, a list that refilters) can
// start or stop covering a view without registering anew.
let sizes: ResizeObserver | null = null;

function observeSize(el: HTMLElement): void {
  if (typeof ResizeObserver === 'undefined') return;
  sizes ??= new ResizeObserver(bump);
  sizes.observe(el);
}

function unobserveSize(el: HTMLElement): void {
  if (entries.some((entry) => entry.el === el) || layers.some((entry) => entry.el === el)) return;
  sizes?.unobserve(el);
}

/** Registers one surface's painted root; returns its release. */
export function registerAirspaceSurface(el: HTMLElement, options?: AirspaceSurfaceOptions): () => void {
  const entry: Entry = { el, rect: options?.rect ?? null };
  observeSize(el);
  bump();
  if (options?.overlay === false) {
    layers = [...layers, entry];
    return () => {
      if (!layers.includes(entry)) return;
      layers = layers.filter((candidate) => candidate !== entry);
      unobserveSize(el);
      bump();
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
    unobserveSize(el);
    bump();
  };
}

/**
 * Svelte action form: `use:airspaceSurface` on an overlay primitive's painted
 * root, or `use:airspaceSurface={{ overlay: false }}` on a layer's. The
 * overlay primitives, the toast stack and the pane strip's drag shield own
 * the call sites; feature components never register themselves. Options that
 * change while mounted (a popover that starts or stops taking the keyboard)
 * register the surface again under them.
 */
export function airspaceSurface(
  el: HTMLElement,
  options?: AirspaceSurfaceOptions,
): { update: (next?: AirspaceSurfaceOptions) => void; destroy: () => void } {
  let current = options;
  let release = registerAirspaceSurface(el, options);
  return {
    update(next) {
      if ((next?.overlay !== false) === (current?.overlay !== false) && next?.rect === current?.rect) return;
      current = next;
      release();
      release = registerAirspaceSurface(el, next);
    },
    destroy: () => release(),
  };
}

/**
 * Reactive: changes whenever a surface of either kind registers, releases
 * or changes size. A native view reads it to judge its obscurity again.
 */
export function airspaceRevision(): number {
  return revision;
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
  sizes?.disconnect();
  sizes = null;
  surfaces = [];
  entries = [];
  layers = [];
  bump();
}
