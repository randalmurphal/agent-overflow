/**
 * Shared "lazy client-side hydration of complete-source blocks" painter.
 *
 * Mermaid and KaTeX both need the FULL source of a block before they can
 * parse it — partial input throws a parse error. The Go markdown pipeline
 * gates emission of `<pre class="mermaid">` and `<div class="math-display">`
 * on fence/delimiter closure, so by the time the frontend sees one of these
 * elements the source is complete. This module watches the document for
 * those elements, lazy-imports the heavy renderer on first sighting, and
 * paints the rendered HTML into each element.
 *
 * Every painter shares one MutationObserver, one debounce, and one LRU
 * source-hash cache keyed by painter. Caching the rendered HTML lets the
 * streaming re-paint path (`AssistantMessage.svelte`'s `{@html}` wipes
 * innerHTML on every delta) reinstate the rendered SVG / math in
 * microseconds without re-invoking the library.
 *
 * Two paint paths:
 *   1. Synchronous cache-hit paint inside the `MutationObserver` callback.
 *      When a freshly-added element's source is already in the cache, we
 *      paint immediately without waiting for the debounced scan. This is
 *      load-bearing during streaming: Go re-renders the whole markdown
 *      every ~50 ms and Svelte's `{@html}` replaces the parent's children
 *      wholesale, which otherwise would cancel each pending debounce
 *      timer in turn and leave the mermaid/math block visibly unrendered
 *      for the entire stream.
 *   2. Debounced async scan for cache misses. First-paint of a new source
 *      waits for the debounce, loads the heavy library on demand, runs
 *      `render`, captures `el.innerHTML` into the cache, then the sync
 *      path picks up future paints for free.
 *
 * Idempotency is tracked via `data-rendered-${key}="<hash>"` written with
 * `setAttribute` — NOT `dataset`, which translates camelCase property
 * names to kebab-case HTML attribute names and caused a silent
 * selector-mismatch bug in the prior mermaid/math renderers.
 *
 * Failed render (valid selector but invalid source) paints the source AND
 * the error underneath, then caches the composite. The cache ensures
 * that subsequent paints of the same bad source produce the same
 * fallback without re-invoking the failing renderer — closing off the
 * "error becomes the next scan's input source" loop that plagued the
 * prior implementation.
 *
 * The primitive is coupled to the `.markdown-body` container class: the
 * observer ignores mutations that don't touch a `.markdown-body` subtree.
 * Painters targeting elements outside that class wouldn't be scanned.
 * If a new surface needs client-side hydration, extend the filter rather
 * than adding another observer.
 */

export type PainterSpec<M> = {
  /** CSS selector for the elements this painter hydrates. */
  selector: string;
  /**
   * Stable identifier used to namespace the idempotency attribute
   * (`data-rendered-${key}`) and the per-painter cache. Must not
   * overlap across painters.
   */
  key: string;
  /** Read the raw source out of the element. Called once per element. */
  readSource(el: HTMLElement): string;
  /**
   * Render `src` into `el` in place. The primitive captures
   * `el.innerHTML` after this resolves and stores it in the source-hash
   * cache, so future paints of the same source are free.
   */
  render(el: HTMLElement, src: string, mod: M): Promise<void> | void;
  /** Dynamic-import the heavy renderer. Called at most once per key. */
  load(): Promise<M>;
  /**
   * Optional transform applied to cached HTML before it's painted into
   * a new element. Used by mermaid to rewrite SVG element ids so the
   * same cached diagram painted twice doesn't cause id collisions.
   */
  rewriteCached?(html: string): string;
};

const DEBOUNCE_MS = 80;
const CACHE_MAX = 128;

type RegistryEntry<M> = {
  spec: PainterSpec<M>;
  loader: Promise<M> | null;
  // Rendered-HTML cache keyed by source hash. Painted back into a
  // fresh element when Svelte's `{@html}` wipe destroys the rendered
  // DOM.
  cache: LRU<string, string>;
  // Source-text cache keyed by source hash. Lets callers (e.g. the
  // mermaid context-menu "Copy source" action) retrieve the original
  // source after the renderer has replaced the element's textContent
  // with its rendered output. Same LRU cap and eviction as `cache`.
  sourceCache: LRU<string, string>;
};

// Registry is intentionally global: one observer + one debounce + one
// cache per painter key across the whole app. Repeat registrations
// with the same key are ignored so `registerPainter` is safe to call
// from `onMount` in a component that may re-mount.
const registry: Array<RegistryEntry<unknown>> = [];
let observer: MutationObserver | null = null;
let debounceHandle: ReturnType<typeof setTimeout> | null = null;
// isScanning + rescanRequested together prevent two `scan()` calls from
// overlapping. The first scan awaits its loader / renders; if another
// scheduleScan fires while it's in-flight, we note "rescan" and re-run
// scan exactly once after the first completes. Without this, two scans
// can both querySelectorAll the same pending elements before either
// marks them, and render() is invoked twice for the same element.
let isScanning = false;
let rescanRequested = false;

export function registerPainter<M>(spec: PainterSpec<M>): void {
  if (typeof document === 'undefined') return;
  if (registry.some((e) => e.spec.key === spec.key)) return;
  registry.push({
    spec: spec as PainterSpec<unknown>,
    loader: null,
    cache: new LRU(CACHE_MAX),
    sourceCache: new LRU(CACHE_MAX),
  });
  ensureObserver();
  scheduleScan();
}

function ensureObserver(): void {
  if (observer !== null) return;
  observer = new MutationObserver((mutations) => {
    if (!mutationsTouchMarkdownBody(mutations)) return;
    // Fast path: paint cache hits synchronously from the observer
    // callback. During streaming, Svelte's `{@html}` replaces the
    // whole markdown-body's children every ~50 ms and the 80 ms
    // debounce would keep getting cancelled by the next delta,
    // leaving rendered mermaid/math diagrams visibly blank for the
    // entire stream. Painting cache hits here — before yielding back
    // to the event loop — means the user sees the rendered output
    // continuously, and the debounced scan only runs for actual
    // cache misses (first-paints).
    paintCachedMatches(mutations);
    scheduleScan();
  });
  observer.observe(document.body, { childList: true, subtree: true });
}

// paintCachedMatches is the synchronous cache-hit branch invoked from
// the observer callback. It walks the added nodes from each mutation,
// looks for elements matching any painter's selector, and paints them
// from the cache if a hash match exists. Cache misses are deferred to
// the async scan path; nothing here awaits or loads modules.
function paintCachedMatches(mutations: MutationRecord[]): void {
  for (const entry of registry) {
    for (const m of mutations) {
      for (const n of Array.from(m.addedNodes)) {
        if (!(n instanceof Element)) continue;
        if (n.matches?.(entry.spec.selector)) {
          paintFromCache(n as HTMLElement, entry);
        }
        const descendants = n.querySelectorAll?.<HTMLElement>(entry.spec.selector);
        if (descendants) {
          for (const d of descendants) paintFromCache(d, entry);
        }
      }
    }
  }
}

function paintFromCache<M>(el: HTMLElement, entry: RegistryEntry<M>): void {
  const attr = `data-rendered-${entry.spec.key}`;
  if (el.hasAttribute(attr)) return;
  let source: string;
  try {
    source = entry.spec.readSource(el);
  } catch {
    return; // the async scan path will log and handle
  }
  if (!source.trim()) return;
  const hash = fnv1a(source);
  const cached = entry.cache.get(hash);
  if (cached === undefined) return;
  el.setAttribute(attr, hash);
  let painted = cached;
  if (entry.spec.rewriteCached) {
    try {
      painted = entry.spec.rewriteCached(cached);
    } catch (err) {
      console.error(`[lazyCompleteSourceRenderer] rewriteCached '${entry.spec.key}' failed`, err);
    }
  }
  el.innerHTML = painted;
}

// A mutation matters when it touches (or introduces) a `.markdown-body`
// subtree. Chat messages are the only surface that emits rendered
// markdown today, and they all live under that class. Filtering here
// avoids scheduling scans for every tooltip, composer focus tick, and
// sidebar refresh happening elsewhere in the app.
function mutationsTouchMarkdownBody(mutations: MutationRecord[]): boolean {
  for (const m of mutations) {
    const t = m.target;
    if (t instanceof Element && t.closest('.markdown-body')) return true;
    for (const n of Array.from(m.addedNodes)) {
      if (!(n instanceof Element)) continue;
      if (n.matches?.('.markdown-body') || n.querySelector?.('.markdown-body')) return true;
    }
  }
  return false;
}

function scheduleScan(): void {
  if (isScanning) {
    rescanRequested = true;
    return;
  }
  if (debounceHandle !== null) clearTimeout(debounceHandle);
  debounceHandle = setTimeout(() => {
    debounceHandle = null;
    void scan();
  }, DEBOUNCE_MS);
}

async function scan(): Promise<void> {
  isScanning = true;
  try {
    for (const entry of registry) {
      const pending = Array.from(
        document.querySelectorAll<HTMLElement>(
          `${entry.spec.selector}:not([data-rendered-${entry.spec.key}])`,
        ),
      );
      if (pending.length === 0) continue;
      const mod = await loadModule(entry);
      if (mod === undefined) continue; // loader failed; skip this painter this round
      for (const el of pending) {
        await process(el, entry, mod);
      }
    }
  } finally {
    isScanning = false;
    if (rescanRequested) {
      rescanRequested = false;
      scheduleScan();
    }
  }
}

// loadModule awaits the painter's dynamic import, caching the resolved
// promise so subsequent scans reuse the loaded module. On rejection it
// resets `entry.loader` to null so a later scan can retry — the prior
// design left a rejected promise in place permanently, which meant a
// single transient import failure killed the painter for the session.
async function loadModule<M>(entry: RegistryEntry<M>): Promise<M | undefined> {
  if (entry.loader === null) entry.loader = entry.spec.load();
  try {
    return await entry.loader;
  } catch (err) {
    entry.loader = null;
    console.error(`[lazyCompleteSourceRenderer] load '${entry.spec.key}' failed`, err);
    return undefined;
  }
}

async function process<M>(el: HTMLElement, entry: RegistryEntry<M>, mod: M): Promise<void> {
  const attr = `data-rendered-${entry.spec.key}`;
  // Defensive: if another scan already marked this element (the
  // `isScanning` guard should prevent it, but the observer may still
  // surface the same el via a later scan run), skip without touching
  // the DOM.
  if (el.hasAttribute(attr)) return;
  let source: string;
  try {
    source = entry.spec.readSource(el);
  } catch (err) {
    console.error(`[lazyCompleteSourceRenderer] readSource '${entry.spec.key}' failed`, err);
    return;
  }
  if (!source.trim()) return;
  const hash = fnv1a(source);
  // Mark BEFORE invoking render so synchronous throws (or async paths
  // that re-enter this scan during their own DOM mutations) don't
  // re-pick up the same element and loop.
  el.setAttribute(attr, hash);
  const cached = entry.cache.get(hash);
  if (cached !== undefined) {
    let painted = cached;
    if (entry.spec.rewriteCached) {
      try {
        painted = entry.spec.rewriteCached(cached);
      } catch (err) {
        console.error(`[lazyCompleteSourceRenderer] rewriteCached '${entry.spec.key}' failed`, err);
      }
    }
    el.innerHTML = painted;
    return;
  }
  try {
    await entry.spec.render(el, source, mod);
    // Only cache if the element is still in the document after the
    // async render resolves. An outer `{@html}` wipe can detach the
    // element mid-render; capturing el.innerHTML after that would
    // persist whatever partial state the renderer left behind on a
    // dead node, and future elements with the same source would paint
    // that corruption from the cache.
    if (el.isConnected) {
      entry.cache.set(hash, el.innerHTML);
      entry.sourceCache.set(hash, source);
    }
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    const fallback = buildFailureFallback(source, msg);
    el.innerHTML = fallback;
    // Cache the fallback only if the element survived. See above.
    if (el.isConnected) {
      entry.cache.set(hash, fallback);
      entry.sourceCache.set(hash, source);
    }
  }
}

// getCachedSource returns the original textual source that produced
// the rendered output on `el`, if the primitive has it cached. The
// element must carry the painter's idempotency attribute
// (`data-rendered-<key>`) — the attribute value IS the source hash,
// which keys the source cache. Returns null when the element has no
// marker (not rendered yet), when the painter is unregistered, or
// when the hash has evicted from the LRU.
export function getCachedSource(painterKey: string, el: HTMLElement): string | null {
  const hash = el.getAttribute(`data-rendered-${painterKey}`);
  if (!hash) return null;
  const entry = registry.find((e) => e.spec.key === painterKey);
  if (!entry) return null;
  return entry.sourceCache.get(hash) ?? null;
}

// Failed-render fallback: keep the source visible so the user has
// context for the error, with the error underneath. Never show just
// the error — debugging a renderer failure without seeing what was
// given to it is painful and also invites the "error becomes the next
// scan's input" loop (the error text ends up in textContent, which
// the renderer would re-parse as mermaid/math on the next tick).
// Caching this composite under the source hash stops any re-invocation.
function buildFailureFallback(source: string, errMsg: string): string {
  return (
    '<pre class="rendered-source"><code>' +
    escapeHtml(source) +
    '</code></pre>' +
    '<div class="rendered-error">⚠ ' +
    escapeHtml(errMsg) +
    '</div>'
  );
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) => {
    switch (c) {
      case '&': return '&amp;';
      case '<': return '&lt;';
      case '>': return '&gt;';
      case '"': return '&quot;';
      case "'": return '&#39;';
      default: return c;
    }
  });
}

// FNV-1a 32-bit hash. At a 128-entry LRU the birthday-bound probability
// of collision within the cache is ~2e-5. A collision would paint the
// cached HTML for source A into an element whose textContent is source
// B — visibly wrong. For mermaid/KaTeX specifically the rendered output
// is a deterministic function of the source, so "wrong" looks like a
// diagram from one message appearing inside another with the same hash.
// Rare but real. Swap to a 64-bit hash (two rounds of FNV or xxHash)
// if this ever bites.
function fnv1a(s: string): string {
  let hash = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    hash ^= s.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return (hash >>> 0).toString(36);
}

class LRU<K, V> {
  private map = new Map<K, V>();
  constructor(private readonly max: number) {}
  get(key: K): V | undefined {
    const v = this.map.get(key);
    if (v !== undefined) {
      this.map.delete(key);
      this.map.set(key, v);
    }
    return v;
  }
  set(key: K, value: V): void {
    this.map.delete(key);
    this.map.set(key, value);
    if (this.map.size > this.max) {
      const firstKey = this.map.keys().next().value;
      if (firstKey !== undefined) this.map.delete(firstKey);
    }
  }
}

// Test-only hook: tear down module-level state. Not exported from the
// barrel but reachable via the module path in vitest specs.
export function __resetForTests(): void {
  registry.length = 0;
  if (observer !== null) {
    observer.disconnect();
    observer = null;
  }
  if (debounceHandle !== null) {
    clearTimeout(debounceHandle);
    debounceHandle = null;
  }
  isScanning = false;
  rescanRequested = false;
}
