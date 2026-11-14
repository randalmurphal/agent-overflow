import { afterEach, describe, expect, it } from 'vitest';
import { createScrollTextFind, type ScrollTextFind } from './scrollTextFind.svelte';
import { findHighlightName } from '../../utils/findHighlights';

// Real Chromium: stepping is judged by real scroll geometry.

const roots: HTMLElement[] = [];
const finds: ScrollTextFind[] = [];

function mount(paragraphs: string[]): HTMLElement {
  const root = document.createElement('div');
  root.style.cssText = 'height:200px;overflow-y:auto;width:300px';
  root.innerHTML = paragraphs.map((text) => `<p style="height:150px;margin:0">${text}</p>`).join('');
  document.body.appendChild(root);
  roots.push(root);
  return root;
}

function find(root: HTMLElement): ScrollTextFind {
  const f = createScrollTextFind({ root: () => root, name: findHighlightName('plan-pane'), noun: () => 'plan' });
  finds.push(f);
  return f;
}

function currentPainted(): string[] {
  const highlight = CSS.highlights.get(`${findHighlightName('plan-pane')}-current`);
  return highlight ? [...highlight].map((range) => range.toString()) : [];
}

function visible(root: HTMLElement, index: number): boolean {
  const p = root.querySelectorAll('p')[index]!;
  const view = root.getBoundingClientRect();
  const rect = p.getBoundingClientRect();
  return rect.bottom > view.top && rect.top < view.bottom;
}

afterEach(() => {
  for (const f of finds.splice(0)) f.clear();
  for (const root of roots.splice(0)) root.remove();
});

describe('scroll text find', () => {
  it('counts the rendered matches and starts at the first one at or below the viewport top', () => {
    const root = mount(['alpha needle', 'beta', 'gamma needle', 'delta', 'epsilon Needle']);
    root.scrollTop = 160;
    const f = find(root);
    f.search('needle');
    expect(f.result).toMatchObject({ count: 3, current: 1, pending: false });
    expect(root.scrollTop).toBe(160);
  });

  it('steps with wrap and scrolls each match into view', () => {
    const root = mount(['alpha needle', 'beta', 'gamma', 'delta', 'epsilon needle']);
    const f = find(root);
    f.search('needle');
    expect(f.result.current).toBe(0);
    f.next();
    expect(f.result.current).toBe(1);
    expect(visible(root, 4)).toBe(true);
    expect(currentPainted()).toEqual(['needle']);
    f.next();
    expect(f.result.current).toBe(0);
    expect(visible(root, 0)).toBe(true);
    f.previous();
    expect(f.result.current).toBe(1);
  });

  it('takes the last match when none is below, and reports no results for a miss', () => {
    const root = mount(['needle', 'needle', 'c', 'd', 'e']);
    root.scrollTop = 500;
    const f = find(root);
    f.search('needle');
    expect(f.result).toMatchObject({ count: 2, current: 1 });
    expect(root.scrollTop).toBeLessThan(500);
    f.search('absent');
    expect(f.result).toMatchObject({ count: 0, current: -1 });
  });

  it('follows the content and drops everything on clear', async () => {
    const root = mount(['needle']);
    const f = find(root);
    f.search('needle');
    root.insertAdjacentHTML('beforeend', '<p>another needle</p>');
    await new Promise((resolve) => setTimeout(resolve, 250));
    expect(f.result.count).toBe(2);
    f.clear();
    expect(f.result).toMatchObject({ count: 0, current: -1 });
    expect(CSS.highlights.has(findHighlightName('plan-pane'))).toBe(false);
  });
});
