import { afterEach, describe, expect, it } from 'vitest';
import '../../../app.css';

const mounted: HTMLElement[] = [];

function paragraph(text: string, first = false): HTMLParagraphElement {
  const element = document.createElement('p');
  element.className = first ? 'md-blk sd-first-block' : 'md-blk';
  element.textContent = text;
  return element;
}

function mountBody(split: boolean): {
  first: HTMLParagraphElement;
  second: HTMLParagraphElement;
} {
  const body = document.createElement('div');
  body.className = 'markdown-body';
  body.style.width = '500px';
  const committed = document.createElement('div');
  committed.className = 'md-committed';
  committed.dataset.streamdownLastBlock = 'paragraph';
  const first = paragraph('First paragraph', true);
  const second = paragraph('Second paragraph');
  committed.append(first);
  body.append(committed);
  if (split) {
    const volatile = document.createElement('div');
    volatile.className = 'md-volatile';
    second.classList.add('sd-first-block');
    volatile.append(second);
    body.append(volatile);
  } else {
    second.classList.add('sd-paragraph-gap');
    committed.append(second);
  }
  document.body.append(body);
  mounted.push(body);
  return { first, second };
}

function gap(first: Element, second: Element): number {
  const firstRect = first.getBoundingClientRect();
  const secondRect = second.getBoundingClientRect();
  return secondRect.top - firstRect.bottom;
}

afterEach(() => {
  for (const element of mounted.splice(0)) element.remove();
});

describe('streaming Markdown paragraph seam geometry', () => {
  it('keeps the split paragraph gap pixel-identical to settled markup', () => {
    const split = mountBody(true);
    const settled = mountBody(false);
    const splitGap = gap(split.first, split.second);
    const settledGap = gap(settled.first, settled.second);

    expect(splitGap).toBeGreaterThan(0);
    expect(Math.abs(splitGap - settledGap)).toBeLessThan(0.1);
  });

  it('does not add the paragraph gap when the committed type is not paragraph', () => {
    const { first, second } = mountBody(true);
    const committed = first.parentElement;
    if (!committed) throw new Error('committed seam root did not mount');
    committed.dataset.streamdownLastBlock = 'math';

    expect(gap(first, second)).toBe(0);
  });
});
