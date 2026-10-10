import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import '../../../app.css';
import ChatMarkdown from './ChatMarkdown.svelte';
import { CHAT_MARKDOWN_SETTLED_CONTEXT } from './markdownSettledContext';

// Math.svelte imports KaTeX on mount and renders no KaTeX output until the
// module arrives. Under a loaded run that import takes most of a second, so a
// polling budget is a race. Streamdown reports settled through this context
// once every registered async resource, the KaTeX import included, has
// finished, which is the completion the assertions below depend on.
function renderSettled(source: string) {
  let markSettled!: () => void;
  const settled = new Promise<void>((resolve) => {
    markSettled = resolve;
  });
  const view = render(ChatMarkdown, {
    props: { source, pathRefs: [] },
    context: new Map([[CHAT_MARKDOWN_SETTLED_CONTEXT, () => markSettled()]]),
  });
  return { view, settled };
}

// The happy-dom math suites all `vi.mock('katex')`, so nothing in the repo
// pinned the DOM KaTeX actually produces. That gap let the 0.16 -> 0.18
// upgrade ship blind: 0.18 renamed 21 bare structural classes (`base`,
// `strut`, `vbox`, `rule`, ...) to `katex-`-prefixed ones, and a stylesheet
// or selector still reaching for the old names would only fail visually.
// These assertions are the tripwire: they name the prefixed classes, so a
// future KaTeX bump that renames or unprefixes them fails here instead of in
// someone's chat window.
describe('KaTeX rendered output', () => {
  it('typesets block math into prefixed KaTeX structure with real geometry', async () => {
    const { view, settled } = renderSettled('$$\n\\frac{a}{b} = \\sqrt{x}\n$$');
    await settled;

    const root = view.container.querySelector<HTMLElement>('.katex-display');
    expect(root).not.toBeNull();
    const katex = root!.querySelector<HTMLElement>(':scope > .katex');
    expect(katex).not.toBeNull();
    // 0.18 prefixes: `.katex-html`, `.katex-base`, `.katex-strut`. Their
    // 0.16 spellings were `.katex-html` (already prefixed), `.base` and
    // `.strut`.
    expect(katex!.querySelector('.katex-html')).not.toBeNull();
    expect(katex!.querySelector('.katex-base')).not.toBeNull();
    expect(katex!.querySelector('.katex-strut')).not.toBeNull();
    expect(katex!.querySelector('.base')).toBeNull();
    expect(katex!.querySelector('.strut')).toBeNull();
    // Unprefixed classes KaTeX deliberately kept — asserted so a bump that
    // renames them is caught too.
    expect(katex!.querySelector('.mfrac')).not.toBeNull();
    expect(katex!.querySelector('.sqrt')).not.toBeNull();

    // Real layout ran: the typeset fraction occupies more than one line box.
    const rect = katex!.getBoundingClientRect();
    expect(rect.width).toBeGreaterThan(0);
    expect(rect.height).toBeGreaterThan(0);

    view.unmount();
  });

  it('typesets inline math without the display wrapper', async () => {
    const { view, settled } = renderSettled('mass is $E = mc^2$ today');
    await settled;

    const katex = view.container.querySelector<HTMLElement>('.katex');
    expect(katex).not.toBeNull();
    expect(view.container.querySelector('.katex-display')).toBeNull();
    expect(katex!.querySelector('.katex-base')).not.toBeNull();
    expect(katex!.textContent).toContain('E');
    expect(katex!.getBoundingClientRect().width).toBeGreaterThan(0);

    view.unmount();
  });
});
