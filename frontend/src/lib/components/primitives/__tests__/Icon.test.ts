// Verifies the Icon primitive's contract:
//   - renders the @lucide/svelte component it's handed via `icon`
//   - forwards size (as inline px) and strokeWidth (into the mask URI)
//   - merges opacity-80 default with caller's additional classes
//   - swaps icon when the prop changes (via re-render)
//
// Since the mask-icons patch (frontend/AGENTS.md §Vendor Patches), a lucide
// icon renders as a CSS-mask <span>, not an <svg> root: the shape lives in
// `--mask-icon` (a data-URI) and the box size is inline width/height. These
// assertions pin that patched contract; if they fail against an unpatched
// @lucide/svelte, the patch was dropped or failed to apply.

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import Search from '@lucide/svelte/icons/search';
import X from '@lucide/svelte/icons/x';
import Icon from '../Icon.svelte';

describe('<Icon>', () => {
  it('renders the passed lucide component as a mask span, not an svg root', () => {
    const { container } = render(Icon, { props: { icon: Search } });
    expect(container.querySelector('svg')).toBeNull();
    const span = container.querySelector('span.lucide-icon');
    expect(span).not.toBeNull();
    expect(span!.classList.contains('lucide-search')).toBe(true);
    expect(span!.getAttribute('style') ?? '').toContain('--mask-icon: url(');
  });

  it('defaults to size=16', () => {
    const { container } = render(Icon, { props: { icon: Search } });
    const style = container.querySelector('span.lucide-icon')!.getAttribute('style') ?? '';
    expect(style).toContain('width: 16px');
    expect(style).toContain('height: 16px');
  });

  it('forwards a custom size as inline px width/height', () => {
    const { container } = render(Icon, { props: { icon: Search, size: 12 } });
    const style = container.querySelector('span.lucide-icon')!.getAttribute('style') ?? '';
    expect(style).toContain('width: 12px');
    expect(style).toContain('height: 12px');
  });

  it('forwards a custom strokeWidth into the mask URI', () => {
    const { container } = render(Icon, { props: { icon: Search, strokeWidth: 1.5 } });
    const style = container.querySelector('span.lucide-icon')!.getAttribute('style') ?? '';
    // encodeURIComponent('stroke-width="1.5"')
    expect(style).toContain('stroke-width%3D%221.5%22');
  });

  it('applies opacity-80 by default and merges caller classes', () => {
    const { container } = render(Icon, {
      props: { icon: Search, class: 'rotate-90 custom-bonus' },
    });
    const span = container.querySelector('span.lucide-icon')!;
    expect(span.classList.contains('opacity-80')).toBe(true);
    expect(span.classList.contains('rotate-90')).toBe(true);
    expect(span.classList.contains('custom-bonus')).toBe(true);
  });

  it('renders a different lucide icon when the icon prop changes', () => {
    const { container, rerender } = render(Icon, { props: { icon: Search } });
    expect(container.querySelector('span.lucide-search')).not.toBeNull();
    rerender({ icon: X });
    expect(container.querySelector('span.lucide-x')).not.toBeNull();
  });
});
