import { describe, it, expect, afterEach } from 'vitest';
// Import the REAL production stylesheet: the mask-icon contract is split
// between the components (shape + box size, via --mask-icon and inline px)
// and app.css (`.lucide-icon`/`.mask-icon` — display, background-color:
// currentColor, the mask shorthand, and the `forced-colors: active` →
// CanvasText fallback). These assertions are cascade-coupled on purpose:
// delete the app.css rule and every icon in the app renders as an empty box,
// which happy-dom cannot see.
import '../../../../app.css';
import { mount, unmount } from 'svelte';
import Search from '@lucide/svelte/icons/search';
import Icon from '../Icon.svelte';
import ToolKindIcon from '../../chat/ToolKindIcon.svelte';

const roots: Array<{ host: HTMLElement; instance: Record<string, unknown> }> = [];

function mountIn(component: unknown, props: Record<string, unknown>): HTMLElement {
  const host = document.createElement('div');
  // A known inherited color so currentColor resolution is assertable.
  host.style.color = 'rgb(10, 200, 30)';
  document.body.appendChild(host);
  const instance = mount(component as never, { target: host, props }) as Record<string, unknown>;
  roots.push({ host, instance });
  return host;
}

afterEach(() => {
  for (const { host, instance } of roots.splice(0)) {
    void unmount(instance as never);
    host.remove();
  }
});

describe('mask icons (cascade-coupled)', () => {
  it('a lucide icon renders as a sprite-masked span with currentColor paint and the exact box', () => {
    const host = mountIn(Icon, { icon: Search, size: 12 });
    const span = host.querySelector('span.lucide-icon') as HTMLElement;
    expect(span).not.toBeNull();
    expect(host.querySelector('svg')).toBeNull();
    const cs = getComputedStyle(span);
    expect(cs.display).toBe('inline-block');
    // Same-document sprite reference, never a data-URI image: each distinct
    // image URI costs an isolated SVG document (page + LocalDOMWindow +
    // singleton roster) in Blink, and those documents' tiny long-lived
    // singletons pinned near-empty Oilpan pages (renderer floor ratchet,
    // 2026-08-25).
    expect(cs.maskImage).toContain('#ao-lucide-');
    expect(cs.maskImage).not.toContain('data:');
    expect(cs.backgroundColor).toBe('rgb(10, 200, 30)');
    const rect = span.getBoundingClientRect();
    expect(rect.width).toBe(12);
    expect(rect.height).toBe(12);
    // The referenced mask exists in the patch's hidden sprite root.
    const id = /#(ao-lucide-\d+)/.exec(cs.maskImage)![1];
    const mask = document.getElementById(id)!;
    expect(mask.closest('svg[data-mask-sprite="lucide"]')).not.toBeNull();
    expect(getComputedStyle(mask).maskType).toBe('alpha');
  });

  it('a ToolKindIcon renders as a sprite-masked span through the same rule', () => {
    const host = mountIn(ToolKindIcon, { kind: 'terminal' });
    const span = host.querySelector('span.mask-icon') as HTMLElement;
    expect(span).not.toBeNull();
    const cs = getComputedStyle(span);
    expect(cs.maskImage).toContain('#ao-mi-');
    expect(cs.maskImage).not.toContain('data:');
    expect(cs.display).toBe('inline-block');
    const id = /#(ao-mi-\d+)/.exec(cs.maskImage)![1];
    expect(document.getElementById(id)!.closest('svg[data-mask-sprite]')).not.toBeNull();
  });

  it('app.css carries the forced-colors CanvasText fallback for both classes', () => {
    // Windows High Contrast strips background-color; without this media
    // block every mask icon disappears. Cannot be emulated per-test in
    // vitest-browser, so pin the rule's existence in the loaded cascade.
    let found = false;
    for (const sheet of Array.from(document.styleSheets)) {
      let rules: CSSRuleList;
      try {
        rules = sheet.cssRules;
      } catch {
        continue;
      }
      for (const rule of Array.from(rules)) {
        if (
          rule instanceof CSSMediaRule &&
          rule.conditionText.includes('forced-colors') &&
          Array.from(rule.cssRules).some(
            (r) =>
              r instanceof CSSStyleRule &&
              r.selectorText.includes('.lucide-icon') &&
              r.selectorText.includes('.mask-icon') &&
              r.style.backgroundColor.toLowerCase() === 'canvastext',
          )
        ) {
          found = true;
        }
      }
    }
    expect(found).toBe(true);
  });
});
