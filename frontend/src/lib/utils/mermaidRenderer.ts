/**
 * Mermaid-diagram painter. The Go markdown pipeline rewrites
 * properly-closed ```mermaid fences into `<pre class="mermaid">`
 * elements carrying the raw source; unclosed fences stay as plain
 * code blocks. This module registers a painter with
 * `lazyCompleteSourceRenderer` that hydrates each `<pre class="mermaid">`
 * into an inline SVG diagram. Lazy-imports mermaid.js on first sighting
 * so threads without diagrams don't pay the bundle cost.
 */

import { registerPainter } from './lazyCompleteSourceRenderer';

// Security-level strict: mermaid won't execute arbitrary scripts from
// diagram source. The source came from a (potentially malicious) agent,
// not a trusted human, so we assume it is untrusted.
const MERMAID_CONFIG = { startOnLoad: false, securityLevel: 'strict' as const };

type Mermaid = typeof import('mermaid').default;

export function registerMermaidRenderer(): void {
  registerPainter<Mermaid>({
    selector: 'pre.mermaid',
    key: 'mermaid',
    readSource: (el) => el.textContent ?? '',
    render: async (el, src, mermaid) => {
      const id = `mermaid-${Math.random().toString(36).slice(2, 10)}`;
      const { svg } = await mermaid.render(id, src);
      el.innerHTML = svg;
      el.classList.add('mermaid-rendered');
    },
    // On cache hit a fresh copy of the cached SVG would carry the same
    // id it was first rendered with. If the same diagram source appears
    // twice on screen their SVGs would share element ids and
    // `url(#id)` references could resolve to the wrong `<defs>` entry.
    // Rewrite the id prefix on every cache paint so each DOM instance
    // is independent. Internal sub-ids all share the root prefix, so a
    // single substitution updates every reference consistently.
    rewriteCached: (html) => {
      const fresh = `mermaid-${Math.random().toString(36).slice(2, 10)}`;
      return html.replace(/mermaid-[a-z0-9]+/g, fresh);
    },
    load: async () => {
      const mod = await import('mermaid');
      mod.default.initialize(MERMAID_CONFIG);
      return mod.default;
    },
  });
}
