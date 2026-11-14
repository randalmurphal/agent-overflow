import { afterEach, describe, expect, it } from 'vitest';
import {
  createFindHighlighter,
  findHighlightName,
  findHighlightsSupported,
  type FindHighlighter,
} from './findHighlights';

// Real Chromium: the CSS Custom Highlight API, ranges over real text nodes
// and MutationObserver delivery are what this utility is made of.

const roots: HTMLElement[] = [];
const highlighters: FindHighlighter[] = [];

function mount(html: string): HTMLElement {
  const root = document.createElement('div');
  root.innerHTML = html;
  document.body.appendChild(root);
  roots.push(root);
  return root;
}

function highlighter(root: HTMLElement, paneId: string): FindHighlighter {
  const h = createFindHighlighter({ root, name: findHighlightName(paneId), rowSelector: '[data-item-id]', rowKeyAttribute: 'data-item-id' });
  highlighters.push(h);
  return h;
}

function painted(name: string): string[] {
  const highlight = CSS.highlights.get(name);
  return highlight ? [...highlight].map((range) => range.toString()) : [];
}

/** Resolves after MutationObserver delivery and the coalesced recompute. */
async function settle(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 250));
}

afterEach(() => {
  for (const h of highlighters.splice(0)) h.dispose();
  for (const root of roots.splice(0)) root.remove();
});

describe('find highlights', () => {
  it('is supported in the browser project', () => {
    expect(findHighlightsSupported()).toBe(true);
  });

  it('registers one range per match across inline markup, and the current row on its own', () => {
    const root = mount(`
      <div data-item-id="r1"><p>A ne<b>edl</b>e and a Needle</p></div>
      <div data-item-id="r2"><p>needle two</p><p>nee</p><p>dle split by blocks</p></div>
    `);
    const h = highlighter(root, 'pane-a');
    h.setQuery('needle');
    const name = findHighlightName('pane-a');
    expect(h.matches.map((m) => [m.range.toString(), m.rowKey])).toEqual([
      ['needle', 'r1'], ['Needle', 'r1'], ['needle', 'r2'],
    ]);
    expect(painted(name)).toEqual(['needle', 'Needle', 'needle']);
    expect(painted(`${name}-current`)).toEqual([]);

    h.setCurrent({ row: 'r2' });
    expect(painted(name)).toEqual(['needle', 'Needle']);
    expect(painted(`${name}-current`)).toEqual(['needle']);

    h.setCurrent({ match: 1 });
    expect(painted(name)).toEqual(['needle', 'needle']);
    expect(painted(`${name}-current`)).toEqual(['Needle']);
  });

  it('follows rows that mount, change and unmount', async () => {
    const root = mount('<div data-item-id="r1">needle</div>');
    const h = highlighter(root, 'pane-a');
    h.setQuery('needle');
    const name = findHighlightName('pane-a');
    expect(painted(name)).toHaveLength(1);

    const row = document.createElement('div');
    row.dataset.itemId = 'r2';
    row.textContent = 'needle and needle';
    root.appendChild(row);
    await settle();
    expect(painted(name)).toHaveLength(3);

    row.firstChild!.textContent = 'no match now';
    await settle();
    expect(painted(name)).toHaveLength(1);

    root.firstElementChild!.remove();
    await settle();
    expect(painted(name)).toHaveLength(0);
    expect(h.matches).toHaveLength(0);
  });

  it('keeps each pane on its own highlight name', () => {
    const a = mount('<div data-item-id="r1">alpha beta</div>');
    const b = mount('<div data-item-id="r1">alpha beta</div>');
    highlighter(a, 'pane-a').setQuery('alpha');
    highlighter(b, 'pane-b').setQuery('beta');
    expect(findHighlightName('pane-a')).not.toBe(findHighlightName('pane-b'));
    expect(painted(findHighlightName('pane-a'))).toEqual(['alpha']);
    expect(painted(findHighlightName('pane-b'))).toEqual(['beta']);
  });

  it('retains nothing after an empty query or dispose', async () => {
    const root = mount('<div data-item-id="r1">needle</div>');
    const h = highlighter(root, 'pane-a');
    const name = findHighlightName('pane-a');
    h.setQuery('needle');
    h.setCurrent({ row: 'r1' });
    h.setQuery('');
    expect(CSS.highlights.has(name)).toBe(false);
    expect(CSS.highlights.has(`${name}-current`)).toBe(false);
    expect(h.matches).toHaveLength(0);

    h.setQuery('needle');
    h.dispose();
    expect(CSS.highlights.has(name)).toBe(false);
    expect(h.matches).toHaveLength(0);
    // A disposed highlighter no longer follows the DOM.
    root.appendChild(document.createTextNode(' needle'));
    await settle();
    expect(CSS.highlights.has(name)).toBe(false);
  });

  it('paints with the theme tokens through a rule for its name', () => {
    const root = mount('<div data-item-id="r1">needle</div>');
    highlighter(root, 'pane-a').setQuery('needle');
    const name = findHighlightName('pane-a');
    const rules = document.adoptedStyleSheets.flatMap((sheet) => [...sheet.cssRules].map((rule) => rule.cssText));
    expect(rules.some((rule) => rule.includes(`::highlight(${name})`) && rule.includes('var(--find-match)'))).toBe(true);
    expect(rules.some((rule) => rule.includes(`::highlight(${name}-current)`) && rule.includes('var(--find-match-current)'))).toBe(true);
  });

  it('skips text a reader cannot see', () => {
    const root = mount('<div data-item-id="r1">needle<span style="display:none">needle</span></div>');
    const h = highlighter(root, 'pane-a');
    h.setQuery('needle');
    expect(h.matches).toHaveLength(1);
  });

  it('matches loose text in a box-less wrapper, unless the wrapper is hidden', () => {
    const root = mount(`
      <div data-item-id="r1"><div style="display:contents"><p>one needle</p>loose needle</div></div>
      <div data-item-id="r2" style="display:none"><div style="display:contents">hidden needle</div></div>
    `);
    const h = highlighter(root, 'pane-a');
    h.setQuery('needle');
    expect(h.matches.map((m) => m.rowKey)).toEqual(['r1', 'r1']);
  });

  it('with rowsOnly matches text inside rows only, a row boundary ending a block', () => {
    const root = mount(`
      <div class="line"><span>12 13</span><span data-find-key="l1"><span data-find-skip>+</span>needle ne</span><span data-find-key="l2">edle</span></div>
      <p>needle outside every row</p>
    `);
    const h = createFindHighlighter({ root, name: findHighlightName('pane-r'), rowSelector: '[data-find-key]', rowKeyAttribute: 'data-find-key', rowsOnly: true });
    highlighters.push(h);
    h.setQuery('needle');
    expect(h.matches.map((m) => [m.range.toString(), m.rowKey])).toEqual([['needle', 'l1']]);
    h.setQuery('+needle');
    expect(h.matches).toHaveLength(0);
    h.setQuery('13');
    expect(h.matches).toHaveLength(0);
  });

  it('paints one occurrence of a row as current and answers its range', () => {
    const root = mount('<div data-find-key="r1">ab ab ab</div><div data-find-key="r2">ab</div>');
    const h = createFindHighlighter({ root, name: findHighlightName('pane-o'), rowSelector: '[data-find-key]', rowKeyAttribute: 'data-find-key', rowsOnly: true });
    highlighters.push(h);
    h.setQuery('ab');
    const name = findHighlightName('pane-o');
    h.setCurrent({ row: 'r1', occurrence: 1 });
    expect(painted(`${name}-current`)).toEqual(['ab']);
    expect(painted(name)).toHaveLength(3);
    expect(h.currentRange()?.startOffset).toBe(3);
    h.setCurrent({ row: 'r1', occurrence: 5 });
    expect(painted(`${name}-current`)).toEqual([]);
    expect(h.currentRange()).toBeNull();
    h.setCurrent({ row: 'r2', occurrence: 0 });
    expect(h.currentRange()?.startContainer.parentElement?.getAttribute('data-find-key')).toBe('r2');
  });
});
