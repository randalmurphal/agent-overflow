/**
 * Lazy Mermaid-diagram renderer. The Go markdown pipeline rewrites
 * every ```mermaid fenced block into `<pre class="mermaid">` carrying
 * the raw source (see internal/highlight/mermaid.go). This module
 * watches the document for those blocks, lazy-imports mermaid.js on
 * the first sighting, and converts each block's textContent into an
 * inline SVG diagram.
 *
 * Why lazy: mermaid.js is ~500 KB minified. Threads that contain no
 * diagrams don't pay the bundle cost or parse time.
 *
 * Why debounced: streaming updates replace the markdown body's
 * innerHTML every few deltas. Without a debounce we'd re-render each
 * in-flight partial diagram dozens of times per second. Waiting for
 * the DOM to settle (150 ms without mutation) means we only render
 * complete blocks.
 */

const RENDER_MARK = 'mermaidRendered';
const DEBOUNCE_MS = 150;
// Security-level strict: mermaid won't execute arbitrary scripts
// from diagram source. This matches the trust boundary for all other
// agent-emitted content — the renderer assumes the source came from
// a (potentially malicious) agent, not a trusted human.
const MERMAID_CONFIG = { startOnLoad: false, securityLevel: 'strict' as const };

let registered = false;
let loader: Promise<typeof import('mermaid').default> | null = null;
let debounceHandle: ReturnType<typeof setTimeout> | null = null;

export function registerMermaidRenderer(): void {
  if (registered || typeof document === 'undefined') return;
  registered = true;
  scheduleScan();
  new MutationObserver(() => scheduleScan()).observe(document.body, {
    childList: true,
    subtree: true,
  });
}

function scheduleScan(): void {
  if (debounceHandle !== null) clearTimeout(debounceHandle);
  debounceHandle = setTimeout(() => {
    debounceHandle = null;
    void scanAndRender();
  }, DEBOUNCE_MS);
}

async function scanAndRender(): Promise<void> {
  const pending = Array.from(
    document.querySelectorAll<HTMLElement>(
      `pre.mermaid:not([data-${RENDER_MARK}])`,
    ),
  );
  if (pending.length === 0) return;

  const mermaid = await ensureLoaded();

  for (const el of pending) {
    const source = el.textContent ?? '';
    if (!source.trim()) continue;
    el.dataset[RENDER_MARK] = 'true';
    try {
      const id = `mermaid-${Math.random().toString(36).slice(2, 10)}`;
      const { svg } = await mermaid.render(id, source);
      el.innerHTML = svg;
      el.classList.add('mermaid-rendered');
    } catch (err) {
      // Surface the parse error inline rather than swallowing. Mermaid
      // tends to emit readable messages; we prepend a warning glyph.
      const msg = err instanceof Error ? err.message : String(err);
      el.textContent = `⚠ Mermaid diagram failed: ${msg}`;
      el.classList.add('mermaid-error');
    }
  }
}

function ensureLoaded(): Promise<typeof import('mermaid').default> {
  if (loader) return loader;
  loader = import('mermaid').then((mod) => {
    const api = mod.default;
    api.initialize(MERMAID_CONFIG);
    return api;
  });
  return loader;
}
