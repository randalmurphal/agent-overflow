import { describe, it, expect, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
// Real production stylesheet: these assertions are cascade-coupled to
// the `.markdown-body ul/ol { padding-left: 2em }` rule in app.css.
import '../../../app.css';
import ChatMarkdown from './ChatMarkdown.svelte';

// Outside list markers paint inside the list's padding-left. A wide
// font (the Hack Nerd Font appearance option) needs ~1.2em for a "5."
// marker and WebKitGTK adds a fixed ~7px marker gap, so anything under
// ~1.8em pushes the marker past the list's border box — and
// overflow-auto containers (the review pane's PR description, the
// capped plan card) clip it to half-eaten digits. The 2em rule is the
// fix; em (not rem) so the room scales with the surface's font size.
// This guards that rule against a compaction pass tightening it back.

const mounted: HTMLElement[] = [];

afterEach(() => {
  cleanup();
  for (const el of mounted.splice(0)) el.remove();
});

async function renderMarkdown(source: string): Promise<HTMLElement> {
  const host = document.createElement('div');
  document.body.appendChild(host);
  mounted.push(host);
  render(ChatMarkdown, { target: host, props: { source, pathRefs: [] } });
  await new Promise((resolve) => setTimeout(resolve, 50));
  return host;
}

describe('markdown list marker room', () => {
  it('gives ordered and unordered lists 2em of marker padding', async () => {
    const host = await renderMarkdown('1. first\n2. second\n\n- bullet\n');
    for (const tag of ['ol', 'ul'] as const) {
      const list = host.querySelector(tag);
      expect(list, tag).not.toBeNull();
      const cs = getComputedStyle(list!);
      expect(cs.listStylePosition, tag).toBe('outside');
      const fontSize = Number.parseFloat(cs.fontSize);
      expect(Number.parseFloat(cs.paddingLeft), tag).toBeCloseTo(2 * fontSize, 1);
      expect(Number.parseFloat(cs.marginLeft), tag).toBe(0);
    }
  });

  it('keeps task-list checkboxes flush with the marker column', async () => {
    const host = await renderMarkdown('- [ ] todo item\n');
    const li = host.querySelector('li');
    expect(li).not.toBeNull();
    const cs = getComputedStyle(li!);
    // The negative margin mirrors the 2em list padding.
    const fontSize = Number.parseFloat(cs.fontSize);
    expect(Number.parseFloat(cs.marginLeft)).toBeCloseTo(-2 * fontSize, 1);
    expect(cs.listStyleType).toBe('none');
  });
});
