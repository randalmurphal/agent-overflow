// The promoted pane's layer as native browser views see it.
//
// A promoted pane is laid over the pane strip and revealed by a clip-path
// that animates between its slot and the strip's visible area
// (components/panes/panePromotion.svelte.ts). A native view cannot see a
// clip-path, so the layer publishes here what it paints:
//
// - It registers in the airspace registry as a layer (not an overlay): a
//   view beneath it is occluded by its CURRENT clip rect, and a view inside
//   it is never occluded by it (`airspaceIntersects(rect, subject)`).
// - `promotedLayerClip(el)` answers the clip rect, in viewport px, that the
//   layer applies to `el` this frame: non-null only while a promote or
//   demote animation runs and `el` is inside the promoted pane. A view
//   inside intersects its visible clip with it. Null means no extra clip.
// - `promotedLayerFrame()` is a reactive counter that changes on every
//   animation frame of a promote or demote, and when the layer starts or
//   ends. A geometry reader reads it in an effect and re-measures.
//
// PaneHost's promotion controller is the only writer (`setPromotedLayer`).

import { untrack } from 'svelte';
import { registerAirspaceSurface, type AirspaceRect } from './paneAirspace.svelte';

let layerEl: HTMLElement | null = null;
let layerClip: AirspaceRect | null = null;
let release: (() => void) | null = null;
let frame = $state(0);

/**
 * The promoted layer's clip rect for an element inside it, while a promote
 * or demote animates; null when `el` is not inside the promoted pane or no
 * animation runs.
 */
export function promotedLayerClip(el: Node): AirspaceRect | null {
  if (!layerEl || !layerClip || !layerEl.contains(el)) return null;
  return layerClip;
}

/** Reactive: changes every animation frame of a promote or demote. */
export function promotedLayerFrame(): number {
  return frame;
}

/**
 * Controller only. `el` is the promoted section (null: no promotion);
 * `clip` its current clip rect, null once fully revealed.
 */
export function setPromotedLayer(el: HTMLElement | null, clip: AirspaceRect | null): void {
  if (el !== layerEl) {
    release?.();
    release = null;
    layerEl = el;
    if (el) {
      release = registerAirspaceSurface(el, {
        overlay: false,
        rect: () => layerClip ?? el.getBoundingClientRect(),
      });
    }
  }
  layerClip = el ? clip : null;
  // Untracked: a writer running inside an effect must not subscribe to it.
  frame = untrack(() => frame) + 1;
}
