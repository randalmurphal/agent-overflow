// Verifies the Icon primitive's contract:
//   - renders the lucide-svelte component it's handed via `icon`
//   - forwards size (as pixels) and strokeWidth to the underlying svg
//   - merges opacity-80 default with caller's additional classes
//   - swaps icon when the prop changes (via re-render)

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import Search from 'lucide-svelte/icons/search';
import X from 'lucide-svelte/icons/x';
import Icon from '../Icon.svelte';

describe('<Icon>', () => {
  it('renders the passed lucide component as an <svg>', () => {
    const { container } = render(Icon, { props: { icon: Search } });
    const svg = container.querySelector('svg');
    expect(svg).not.toBeNull();
    expect(svg!.classList.contains('lucide-search')).toBe(true);
  });

  it('defaults to size=16', () => {
    const { container } = render(Icon, { props: { icon: Search } });
    const svg = container.querySelector('svg')!;
    expect(svg.getAttribute('width')).toBe('16');
    expect(svg.getAttribute('height')).toBe('16');
  });

  it('forwards a custom size as SVG width/height', () => {
    const { container } = render(Icon, { props: { icon: Search, size: 12 } });
    const svg = container.querySelector('svg')!;
    expect(svg.getAttribute('width')).toBe('12');
    expect(svg.getAttribute('height')).toBe('12');
  });

  it('forwards a custom strokeWidth', () => {
    const { container } = render(Icon, { props: { icon: Search, strokeWidth: 1.5 } });
    const svg = container.querySelector('svg')!;
    expect(svg.getAttribute('stroke-width')).toBe('1.5');
  });

  it('applies opacity-80 by default and merges caller classes', () => {
    const { container } = render(Icon, {
      props: { icon: Search, class: 'rotate-90 custom-bonus' },
    });
    const svg = container.querySelector('svg')!;
    expect(svg.classList.contains('opacity-80')).toBe(true);
    expect(svg.classList.contains('rotate-90')).toBe(true);
    expect(svg.classList.contains('custom-bonus')).toBe(true);
  });

  it('renders a different lucide icon when the icon prop changes', () => {
    const { container, rerender } = render(Icon, { props: { icon: Search } });
    expect(container.querySelector('svg.lucide-search')).not.toBeNull();
    rerender({ icon: X });
    expect(container.querySelector('svg.lucide-x')).not.toBeNull();
  });
});
