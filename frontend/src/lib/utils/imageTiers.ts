// Display-density tiers for the images markdown paints, and the one
// measurement that picks them.
//
// The backend serves a local or forge image at a ladder width
// (GetLocalImage, FetchForgeAttachment): the smallest tier at least the
// display width in device pixels, or the original above the ladder. A
// handful of widths then serve every pane and device pixel ratio, and each
// is derived and cached once. A host asks for the tier its box needs and
// moves up when the box grows; it never moves down, because the bytes it
// holds already cover a narrower box.
//
// Measurement is one module-level ResizeObserver shared by every image
// host, plus one `matchMedia` listener that re-dispatches the last widths
// when the device pixel ratio changes (browser zoom, a move to another
// display). No host reads layout itself: a synchronous width read in a
// mount effect forces layout in the frame the virtualizer is mounting rows
// (docs/architecture/frontend-scroll.md).

/**
 * The ladder, in device pixels. `internal/attachment/derive.go`
 * (`DeriveWidths`) is the other copy; the two must match, or a request
 * between two tiers would be served at a width this side never asks for.
 */
export const IMAGE_TIER_WIDTHS: readonly number[] = [320, 480, 720, 1080, 1440, 2160, 2880, 3840, 5120];

/**
 * The tier that serves a box `cssWidth` CSS pixels wide at
 * `devicePixelRatio`: the smallest ladder width at least
 * `ceil(cssWidth * devicePixelRatio)`. 0, the original, for a box wider
 * than the ladder or a width that is not positive.
 */
export function imageTierFor(cssWidth: number, devicePixelRatio: number): number {
  if (!(cssWidth > 0)) return 0;
  const ratio = devicePixelRatio > 0 ? devicePixelRatio : 1;
  const device = Math.ceil(cssWidth * ratio);
  for (const width of IMAGE_TIER_WIDTHS) {
    if (width >= device) return width;
  }
  return 0;
}

/** Whether `candidate` serves more pixels than `current`; 0, the original, is the top. */
export function isHigherImageTier(candidate: number, current: number): boolean {
  if (current === 0) return false;
  return candidate === 0 || candidate > current;
}

type BoxCallback = (cssWidth: number) => void;

interface ObservedBox {
  callbacks: Set<BoxCallback>;
  /** The last content-box width reported for the element; null before the first. */
  width: number | null;
}

const boxes = new Map<Element, ObservedBox>();
let observer: ResizeObserver | null = null;
let ratioQuery: MediaQueryList | null = null;

/**
 * Calls `callback` with `element`'s content-box width in CSS pixels each
 * time the shared observer reports it, and again for every observed element
 * when the device pixel ratio changes. Several hosts may observe one
 * element (two images in one paragraph); one added to an element that has
 * already been measured is called with that width before this returns,
 * since the observer reports an element only once until it resizes.
 *
 * Returns the function that stops this callback.
 */
export function observeImageBox(element: Element, callback: BoxCallback): () => void {
  let box = boxes.get(element);
  if (!box) {
    box = { callbacks: new Set(), width: null };
    boxes.set(element, box);
    observer ??= new ResizeObserver(dispatchResize);
    observer.observe(element);
    if (!ratioQuery) watchDevicePixelRatio();
  }
  box.callbacks.add(callback);
  if (box.width !== null) callback(box.width);
  const held = box;
  return () => {
    if (!held.callbacks.delete(callback) || held.callbacks.size > 0) return;
    boxes.delete(element);
    observer?.unobserve(element);
    if (boxes.size === 0) unwatchDevicePixelRatio();
  };
}

function dispatchResize(entries: ResizeObserverEntry[]): void {
  for (const entry of entries) {
    const box = boxes.get(entry.target);
    if (!box) continue;
    box.width = entry.contentRect.width;
    for (const callback of box.callbacks) callback(box.width);
  }
}

// A resolution query matches one ratio, so it fires once when the ratio
// leaves it; the listener re-arms at the new ratio.
function watchDevicePixelRatio(): void {
  if (typeof matchMedia !== 'function') return;
  ratioQuery = matchMedia(`(resolution: ${globalThis.devicePixelRatio || 1}dppx)`);
  ratioQuery.addEventListener('change', handleRatioChange);
}

function unwatchDevicePixelRatio(): void {
  ratioQuery?.removeEventListener('change', handleRatioChange);
  ratioQuery = null;
}

function handleRatioChange(): void {
  unwatchDevicePixelRatio();
  if (boxes.size === 0) return;
  watchDevicePixelRatio();
  for (const box of boxes.values()) {
    if (box.width === null) continue;
    for (const callback of box.callbacks) callback(box.width);
  }
}

/**
 * Most recent tier per image identity, so a remount (a scroll away and
 * back, the side chat forked from a thread) can read the cache in the frame
 * it mounts, before anything is measured. Bounded like the media cache it
 * indexes; least recently remembered first.
 */
const lastTiers = new Map<string, number>();
export const IMAGE_TIER_MEMO_MAX_ENTRIES = 512;

export function rememberImageTier(key: string, tier: number): void {
  lastTiers.delete(key);
  lastTiers.set(key, tier);
  if (lastTiers.size <= IMAGE_TIER_MEMO_MAX_ENTRIES) return;
  const oldest = lastTiers.keys().next();
  if (!oldest.done) lastTiers.delete(oldest.value);
}

export function lastImageTier(key: string): number | undefined {
  return lastTiers.get(key);
}

/**
 * Report `cssWidth` as if the observer had measured it, for every observed
 * element or only `element`. happy-dom lays nothing out, so a component
 * test drives measurement through this.
 */
export function __reportImageBoxForTest(cssWidth: number, element?: Element): void {
  for (const [target, box] of boxes) {
    if (element && target !== element) continue;
    box.width = cssWidth;
    for (const callback of box.callbacks) callback(cssWidth);
  }
}

/** Drop every observation, the shared observer and the memo. */
export function __resetImageTiersForTest(): void {
  observer?.disconnect();
  observer = null;
  boxes.clear();
  unwatchDevicePixelRatio();
  lastTiers.clear();
}
