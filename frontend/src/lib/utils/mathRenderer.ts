/**
 * Lazy KaTeX math renderer. Mirrors the mermaid loader pattern: a
 * single debounced MutationObserver scans for `.math-inline` and
 * `.math-display` blocks emitted by the Go markdown pipeline (see
 * internal/highlight/math.go) and converts each element's textContent
 * into typeset math.
 *
 * Why lazy: KaTeX is ~280 KB minified + 170 KB of woff2 fonts.
 * Threads without math never pay that cost — the dynamic import
 * only fires when a math node appears.
 *
 * Why debounced: streaming updates replace a message's innerHTML every
 * few deltas. Rendering mid-stream produces broken partial LaTeX ("\fr"
 * for "\frac" etc.); waiting for the DOM to settle avoids the churn.
 *
 * KaTeX config:
 *   - `throwOnError: false` — on parse failure KaTeX falls back to the
 *     raw LaTeX source inside the element rather than throwing. The
 *     source is visible to the user but the whole message doesn't
 *     break.
 *   - `strict: 'warn'` — unsupported LaTeX commands log to console but
 *     don't break the render.
 *   - `trust: false` — reject `\href`, `\url`, and other commands that
 *     could inject arbitrary HTML. Agents emit untrusted markdown.
 */

const RENDER_MARK = 'mathRendered';
const DEBOUNCE_MS = 150;

let registered = false;
let loader: Promise<typeof import('katex').default> | null = null;
let debounceHandle: ReturnType<typeof setTimeout> | null = null;

export function registerMathRenderer(): void {
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
      `.math-inline:not([data-${RENDER_MARK}]), .math-display:not([data-${RENDER_MARK}])`,
    ),
  );
  if (pending.length === 0) return;

  const katex = await ensureLoaded();

  for (const el of pending) {
    const source = el.textContent ?? '';
    if (!source.trim()) continue;
    el.dataset[RENDER_MARK] = 'true';
    try {
      katex.render(source, el, {
        displayMode: el.classList.contains('math-display'),
        throwOnError: false,
        strict: 'warn',
        trust: false,
      });
    } catch (err) {
      // Defensive: throwOnError=false keeps katex from throwing in
      // normal parse-fail cases. Anything that still throws is
      // surfaced inline rather than breaking the whole message.
      const msg = err instanceof Error ? err.message : String(err);
      el.textContent = `⚠ Math render failed: ${msg}`;
      el.classList.add('math-error');
    }
  }
}

async function ensureLoaded(): Promise<typeof import('katex').default> {
  if (loader) return loader;
  loader = Promise.all([
    import('katex'),
    // Side-effect import — KaTeX ships its font CSS separately and
    // the typeset breaks without it. Vite resolves the css import to
    // the fonts via URL rewriting.
    import('katex/dist/katex.min.css'),
  ]).then(([mod]) => mod.default);
  return loader;
}
