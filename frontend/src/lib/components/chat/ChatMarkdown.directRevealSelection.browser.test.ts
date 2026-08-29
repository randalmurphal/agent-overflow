import { render } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import DirectMarkdownDifferentialHarness from './DirectMarkdownDifferentialHarness.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

type Harness = {
  append(delta: string): Promise<boolean>;
};

function textRangeWidth(root: Element): number {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const first = walker.nextNode();
  if (!(first instanceof Text)) return 0;
  let last = first;
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (node instanceof Text) last = node;
  }
  const range = document.createRange();
  range.setStart(first, 0);
  range.setEnd(last, last.length);
  return range.getBoundingClientRect().width;
}

beforeEach(() => {
  setBindingMock('HighlightClassNames', async () => []);
  setBindingMock('HighlightCode', async ({ lang }: { lang: string }) => ({
    lang,
    lines: [],
    truncated: false,
  }));
});

describe('direct markdown reveal selection', () => {
  it('keeps a range in a compact committed block while the live tail changes', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;

    await harness.append('Committed prefix words stay selected.\n\n');
    await harness.append('Live tail ');

    const paragraph = view.container.querySelector<HTMLElement>(
      '[data-differential-direct] .md-committed p',
    );
    const selectedNode = paragraph?.firstChild;
    if (!paragraph || !(selectedNode instanceof Text)) {
      throw new Error('compact committed paragraph did not mount');
    }
    const start = selectedNode.data.indexOf('prefix');
    if (start < 0) throw new Error('committed selection text was absent');

    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.setBaseAndExtent(selectedNode, start, selectedNode, start + 6);
    expect(selection?.toString()).toBe('prefix');

    await harness.append('continues ');
    await harness.append('with **structure**.');

    expect(paragraph.isConnected).toBe(true);
    expect(
      view.container.querySelector('[data-differential-direct] .md-committed p'),
    ).toBe(paragraph);
    expect(paragraph.firstChild).toBe(selectedNode);
    expect(selection?.toString()).toBe('prefix');
    expect(selection?.anchorNode).toBe(selectedNode);
  });

  it('keeps a range when a closed list moves from the volatile tail to the prefix', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;

    await harness.append('- first item\n- second item');
    const item = view.container.querySelector<HTMLElement>(
      '[data-differential-direct] .md-volatile li',
    );
    const selectedNode = item
      ? Array.from(item.childNodes).find(
          (node): node is Text => node instanceof Text && node.data.includes('first'),
        )
      : undefined;
    if (!item || !(selectedNode instanceof Text)) {
      throw new Error('volatile list item did not mount');
    }
    const start = selectedNode.data.indexOf('first');
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.setBaseAndExtent(selectedNode, start, selectedNode, start + 5);
    expect(selection?.toString()).toBe('first');

    await harness.append('\n\nPartial table row still growing');

    expect(
      view.container.querySelector('[data-differential-direct] .md-committed li'),
    ).not.toBeNull();
    expect(selection?.toString()).toBe('first');
  });

  it('keeps an existing range while literal appends cross Text-node chunks', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;

    await harness.append('Alpha ');
    await harness.append('beta ');
    await harness.append('gamma ');

    const host = view.container.querySelector<HTMLElement>(
      '[data-differential-direct] [data-streamdown-direct-append-safe]',
    );
    const selectedNode = host?.firstChild;
    if (!host || !(selectedNode instanceof Text)) {
      throw new Error('direct streaming text host did not mount');
    }

    const range = document.createRange();
    range.setStart(selectedNode, 0);
    range.setEnd(selectedNode, 5);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    expect(selection?.toString()).toBe('Alpha');

    for (let index = 0; index < 100; index++) {
      expect(await harness.append(`word${index} `)).toBe(true);
    }

    expect(selection?.toString()).toBe('Alpha');
    expect(selection?.getRangeAt(0).startContainer).toBe(selectedNode);
    expect(host.childNodes.length).toBeGreaterThan(1);
  });

  it('keeps a range in direct text when punctuation falls back to the parser', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;

    await harness.append('Alpha ');
    expect(await harness.append('beta ')).toBe(true);

    const host = view.container.querySelector<HTMLElement>(
      '[data-differential-direct] [data-streamdown-direct-append-safe]',
    );
    const selectedNode = host?.childNodes.item(1);
    if (!host || !(selectedNode instanceof Text)) {
      throw new Error('direct streaming suffix did not mount');
    }

    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.setBaseAndExtent(selectedNode, 5, selectedNode, 1);
    expect(selection?.toString()).toBe('beta');

    expect(await harness.append('.')).toBe(false);

    expect(host.textContent).toBe('Alpha beta .');
    expect(selection?.toString()).toBe('beta');
    expect(selection?.getRangeAt(0).startContainer).toBe(host.firstChild);
    expect(selection?.anchorOffset).toBe(10);
    expect(selection?.focusOffset).toBe(6);
  });

  it.each([
    ['focus', 5, 10],
    ['anchor', 1, 6],
  ] as const)(
    'keeps a cross-message selection whose %s endpoint is in direct text',
    async (insideEndpoint, directOffset, authoritativeOffset) => {
      const view = render(DirectMarkdownDifferentialHarness);
      const harness = view.component as unknown as Harness;
      await harness.append('Alpha ');
      expect(await harness.append('beta ')).toBe(true);

      const directRoot = view.container.querySelector<HTMLElement>(
        '[data-differential-direct]',
      );
      const host = directRoot?.querySelector<HTMLElement>(
        '[data-streamdown-direct-append-safe]',
      );
      const directNode = host?.childNodes.item(1);
      if (!directRoot || !host || !(directNode instanceof Text)) {
        throw new Error('direct streaming suffix did not mount');
      }
      const outside = document.createElement('div');
      const outsideText = document.createTextNode('Earlier message');
      outside.append(outsideText);
      view.container.insertBefore(outside, directRoot);

      const selection = window.getSelection();
      selection?.removeAllRanges();
      if (insideEndpoint === 'focus') {
        selection?.setBaseAndExtent(
          outsideText,
          outsideText.length,
          directNode,
          directOffset,
        );
      } else {
        selection?.setBaseAndExtent(
          directNode,
          directOffset,
          outsideText,
          outsideText.length,
        );
      }

      expect(await harness.append('.')).toBe(false);
      const insideNode = insideEndpoint === 'focus'
        ? selection?.focusNode
        : selection?.anchorNode;
      const insideOffset = insideEndpoint === 'focus'
        ? selection?.focusOffset
        : selection?.anchorOffset;
      const outsideNode = insideEndpoint === 'focus'
        ? selection?.anchorNode
        : selection?.focusNode;
      const outsideOffset = insideEndpoint === 'focus'
        ? selection?.anchorOffset
        : selection?.focusOffset;

      expect(insideNode).toBe(host.firstChild);
      expect(insideOffset).toBe(authoritativeOffset);
      expect(outsideNode).toBe(outsideText);
      expect(outsideOffset).toBe(outsideText.length);
    },
  );

  it('restores a direct-text range when a closing marker rebuilds the leaf', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;

    expect(await harness.append('*Alpha ')).toBe(false);
    expect(await harness.append('beta ')).toBe(true);
    expect(await harness.append('gamma')).toBe(true);

    const oldHost = view.container.querySelector<HTMLElement>(
      '[data-differential-direct] [data-streamdown-direct-append-safe]',
    );
    const selectedNode = oldHost?.childNodes.item(1);
    if (!oldHost || !(selectedNode instanceof Text)) {
      throw new Error('direct streaming emphasis suffix did not mount');
    }
    const start = selectedNode.data.indexOf('gamma');
    if (start < 0) throw new Error('direct streaming emphasis suffix was incomplete');

    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.setBaseAndExtent(selectedNode, start, selectedNode, start + 5);
    expect(selection?.toString()).toBe('gamma');

    expect(await harness.append('*')).toBe(false);

    const direct = view.container.querySelector(
      '[data-differential-direct] .markdown-body',
    );
    const baseline = view.container.querySelector(
      '[data-differential-baseline] .markdown-body',
    );
    expect(direct?.textContent).toBe(baseline?.textContent);
    expect(direct?.querySelectorAll('em')).toHaveLength(1);
    expect(baseline?.querySelectorAll('em')).toHaveLength(1);
    expect(selection?.toString()).toBe('gamma');
  });

  it.each([
    ['Latin ligature boundary', ['of', 'fice ']],
    ['combining-mark boundary', ['cafe', '\u0301 ']],
    ['Arabic joining boundary', ['الس', 'لام ']],
  ])('preserves glyph shaping across a %s', async (_name, chunks) => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;
    for (const chunk of chunks) await harness.append(chunk);

    const baseline = view.container.querySelector(
      '[data-differential-baseline] .markdown-body',
    );
    const direct = view.container.querySelector(
      '[data-differential-direct] .markdown-body',
    );
    if (!baseline || !direct) throw new Error('markdown comparison roots did not mount');

    expect(direct.textContent).toBe(baseline.textContent);
    expect(textRangeWidth(direct)).toBeCloseTo(textRangeWidth(baseline), 1);
  });

  it('keeps supplementary code points intact at the direct Text-node limit', async () => {
    const view = render(DirectMarkdownDifferentialHarness);
    const harness = view.component as unknown as Harness;
    await harness.append('seed ');
    expect(await harness.append(`${'a'.repeat(254)}𐐀b `)).toBe(true);

    const baseline = view.container.querySelector(
      '[data-differential-baseline] .markdown-body',
    );
    const direct = view.container.querySelector(
      '[data-differential-direct] .markdown-body',
    );
    const host = direct?.querySelector('[data-streamdown-direct-append-safe]');
    if (!baseline || !direct || !host) {
      throw new Error('markdown comparison roots did not mount');
    }

    expect(direct.textContent).toBe(baseline.textContent);
    expect(textRangeWidth(direct)).toBeCloseTo(textRangeWidth(baseline), 1);
    expect(host.childNodes.length).toBeGreaterThan(2);
    for (const node of host.childNodes) {
      if (!(node instanceof Text) || node.length === 0) continue;
      const first = node.data.charCodeAt(0);
      const last = node.data.charCodeAt(node.length - 1);
      expect(first >= 0xdc00 && first <= 0xdfff).toBe(false);
      expect(last >= 0xd800 && last <= 0xdbff).toBe(false);
    }
  });
});
