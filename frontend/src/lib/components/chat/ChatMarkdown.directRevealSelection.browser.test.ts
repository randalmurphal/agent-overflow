import { render } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import DirectMarkdownDifferentialHarness from './DirectMarkdownDifferentialHarness.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

type Harness = {
  append(delta: string): Promise<boolean>;
};

beforeEach(() => {
  setBindingMock('HighlightClassNames', async () => []);
  setBindingMock('HighlightCode', async ({ lang }: { lang: string }) => ({
    lang,
    lines: [],
    truncated: false,
  }));
});

describe('direct markdown reveal selection', () => {
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
});
