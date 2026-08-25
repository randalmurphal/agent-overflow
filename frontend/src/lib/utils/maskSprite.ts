/**
 * Same-document mask sprite: one hidden <svg> of <mask> elements that icon
 * spans reference via `--mask-icon: url(#id)` through the shared
 * `.lucide-icon`/`.mask-icon` rule in app.css.
 *
 * Why not `mask-image: url(data:image/svg+xml,...)` (what shipped first):
 * Blink builds an ISOLATED SVG document — internal page, LocalDOMWindow and
 * its full singleton roster (GestureManager, FontFallbackMap, spell-check
 * requester, PaintTimingDetector, ...) — per DISTINCT image URI, and sprite
 * fragments don't dedupe (measured 2026-08-25: data-URI and blob fragments
 * both spawn documents per reference). The app's 47 icon URIs held ~57 such
 * documents alive, and their tiny long-lived singletons were the survivors
 * pinning hundreds of near-empty 128KB Oilpan pages (committed 153→240MB
 * over 80min at ~20MB live — the renderer floor ratchet). A same-document
 * `mask: url(#id)` reference needs no document at all and paints
 * pixel-identically (spritecheck3/4 probes, soak rig).
 *
 * Mechanics: masks are `mask-type: alpha` (same semantics the data URIs had
 * under mask-mode: match-source), with maskUnits + maskContentUnits both
 * objectBoundingBox and the content wrapped in `scale(1/viewBox)` so the
 * shape stretches to the span's box — the spans are square, so this matches
 * the old `mask-size: contain` rendering.
 */

interface SpriteEntry {
  ref: string; // 'url(#ao-mi-N)'
  markup: string; // the <mask> outerHTML-equivalent, kept for root rebuilds
}

const SVG_NS = 'http://www.w3.org/2000/svg';
const entries = new Map<string, SpriteEntry>();
let root: SVGSVGElement | undefined;
let seq = 0;

function ensureRoot(): SVGSVGElement {
  if (root === undefined || !root.isConnected) {
    root = document.createElementNS(SVG_NS, 'svg');
    root.setAttribute('width', '0');
    root.setAttribute('height', '0');
    root.setAttribute('aria-hidden', 'true');
    root.setAttribute('data-mask-sprite', '');
    root.style.position = 'absolute';
    // A replaced root would orphan every registered mask, so rebuild them all.
    let markup = '';
    for (const e of entries.values()) markup += e.markup;
    if (markup !== '') root.innerHTML = markup;
    document.body.appendChild(root);
  }
  return root;
}

/**
 * Register (idempotently, keyed) a mask shape and return the `url(#id)`
 * reference for `--mask-icon`.
 *
 * @param key      stable identity for the shape (e.g. `tool:terminal`)
 * @param viewBoxW natural width of the shape's coordinate space
 * @param viewBoxH natural height of the shape's coordinate space
 * @param attrs    presentation attributes for the content group
 *                 (e.g. `fill="none" stroke="black" stroke-width="2"`)
 * @param body     inner SVG markup in viewBox coordinates
 */
export function maskSpriteRef(
  key: string,
  viewBoxW: number,
  viewBoxH: number,
  attrs: string,
  body: string,
): string {
  const hit = entries.get(key);
  if (hit !== undefined) return hit.ref;
  const id = `ao-mi-${seq++}`;
  const markup =
    `<mask id="${id}" maskUnits="objectBoundingBox" maskContentUnits="objectBoundingBox" style="mask-type:alpha">` +
    `<g transform="scale(${1 / viewBoxW} ${1 / viewBoxH})" ${attrs}>${body}</g></mask>`;
  const entry: SpriteEntry = { ref: `url(#${id})`, markup };
  entries.set(key, entry);
  ensureRoot().insertAdjacentHTML('beforeend', markup);
  return entry.ref;
}
