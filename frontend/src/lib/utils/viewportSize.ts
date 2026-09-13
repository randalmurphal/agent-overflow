// The viewport a floating surface may be clamped into.
//
// `window.innerHeight` is the LAYOUT viewport. With the soft keyboard up,
// mobile Safari keeps the layout viewport at its full height and shrinks
// only the visual viewport, so a menu clamped against `innerHeight` lands
// under the keyboard. `visualViewport` reports the part of the page the
// person can actually see; where the API is absent (older embeddings,
// happy-dom) the layout viewport is the only answer there is.

export interface ViewportSize {
  width: number;
  height: number;
}

export function viewportSize(): ViewportSize {
  const visual = window.visualViewport;
  return {
    width: visual?.width ?? window.innerWidth,
    height: visual?.height ?? window.innerHeight,
  };
}
