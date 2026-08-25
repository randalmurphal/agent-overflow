import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { Streamdown, type useStreamdown } from 'svelte-streamdown';

// The context class type isn't re-exported from the package index; the
// hook's return type names it without adding a vendored export.
type StreamdownContext = ReturnType<typeof useStreamdown>;

// Divergence entry 20 (vendor/svelte-streamdown/DIVERGENCE.md): the
// context's `theme` getter must serve a MEMOIZED merge. Upstream re-ran
// mergeTheme — a deep merge with a twMerge/clsx parse per subkey — on
// every `streamdown.theme` access, which the burn profile measured at
// 33MB/45s of allocation during sustained streaming (every template
// effect of every element component reads it, per delta). Identity
// stability across reads is the observable property: same props, same
// object; a theme prop change mints a new merged object.

const CUSTOM_THEME = { code: { base: 'burn-test-base' } };

function mountWithTheme(theme?: Record<string, unknown>) {
  let ctx: StreamdownContext | undefined;
  const rendered = render(Streamdown, {
    props: {
      content: 'hello `code` world',
      theme,
      // Svelte 5 function bindings work as plain props in render():
      // Streamdown assigns the bindable in its init body.
      get streamdown() {
        return ctx;
      },
      set streamdown(v: StreamdownContext | undefined) {
        ctx = v;
      },
    },
  });
  flushSync();
  if (!ctx) throw new Error('Streamdown never assigned its context bindable');
  return { ctx, rendered };
}

describe('Streamdown theme memoization', () => {
  it('serves one object identity across repeated theme reads', () => {
    const { ctx } = mountWithTheme(CUSTOM_THEME);
    const first = ctx.theme;
    expect(first.code.base).toContain('burn-test-base');
    for (let i = 0; i < 5; i++) {
      expect(ctx.theme).toBe(first);
    }
  });

  it('serves one object identity for mermaidConfig reads', () => {
    const { ctx } = mountWithTheme(CUSTOM_THEME);
    const first = ctx.mermaidConfig;
    expect(ctx.mermaidConfig).toBe(first);
  });

  it('re-merges when the theme prop changes', async () => {
    const { ctx, rendered } = mountWithTheme(CUSTOM_THEME);
    const first = ctx.theme;
    await rendered.rerender({ theme: { code: { base: 'changed-base' } } });
    flushSync();
    expect(ctx.theme).not.toBe(first);
    expect(ctx.theme.code.base).toContain('changed-base');
  });
});
