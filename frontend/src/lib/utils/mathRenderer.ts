/**
 * KaTeX math painter. The Go markdown pipeline emits
 * `<span class="math-inline">` for inline math and
 * `<div class="math-display">` for closed multi-line blocks.
 * (Unclosed `$$…` blocks stay as plain code blocks server-side so
 * KaTeX never sees partial source.) This module registers a painter
 * with `lazyCompleteSourceRenderer` that hydrates each element into
 * typeset math via KaTeX.
 *
 * KaTeX config:
 *   - `throwOnError: false` — on parse failure KaTeX falls back to
 *     the raw LaTeX source inside the element. The painter's own
 *     failure path never triggers in this mode; the user sees an
 *     in-band error rendered by KaTeX itself (red source).
 *   - `strict: 'warn'` — unsupported LaTeX commands log to console
 *     but don't break the render.
 *   - `trust: false` — reject `\href`, `\url`, and other commands
 *     that could inject arbitrary HTML. Agent-emitted content is
 *     untrusted.
 */

import { registerPainter } from './lazyCompleteSourceRenderer';

type Katex = typeof import('katex').default;

function registerFor(selector: string, key: string, displayMode: boolean): void {
  registerPainter<Katex>({
    selector,
    key,
    readSource: (el) => el.textContent ?? '',
    render: (el, src, katex) => {
      katex.render(src, el, {
        displayMode,
        throwOnError: false,
        strict: 'warn',
        trust: false,
      });
      // Post-render class lets the CSS drop the mono pre-render
      // fallback without the pre-render styling flashing during the
      // short window where the idempotency attribute is already
      // written but KaTeX has not yet mutated the element.
      el.classList.add('math-rendered');
    },
    load: async () => {
      const [mod] = await Promise.all([
        import('katex'),
        // Side-effect import — KaTeX ships its font CSS separately
        // and the typeset breaks without it. Vite resolves the css
        // import to the fonts via URL rewriting.
        import('katex/dist/katex.min.css'),
      ]);
      return mod.default;
    },
  });
}

export function registerMathRenderer(): void {
  registerFor('.math-inline', 'math-inline', false);
  registerFor('.math-display', 'math-display', true);
}
