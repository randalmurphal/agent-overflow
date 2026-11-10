import { render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import { SHORT_CODESPAN_CLASS, SHORT_CODESPAN_MAX_LENGTH } from '../../markdown/render/codespan';

// The short-codespan marker is published by two renderers of one token
// stream: `render/Block.svelte` on the component path and
// `render/staticHtml.ts` on the compact fixed-tag path. The table cell rule in
// app.css keys on it, so a path that drops or misplaces the marker splits
// identifiers mid-token again on that path only.

const SHORT = 'x'.repeat(SHORT_CODESPAN_MAX_LENGTH);
const LONG = 'x'.repeat(SHORT_CODESPAN_MAX_LENGTH + 1);

function mount(source: string) {
  const { container } = render(CompactStaticMarkdownHarness, { source });
  const componentPath = container.querySelector('[data-full-token-tree] > div');
  const staticPath = container.querySelector('[data-compact-static] > div');
  if (!componentPath || !staticPath) {
    throw new Error('differential Streamdown roots did not mount');
  }
  return { componentPath, staticPath };
}

/** `text → marked` for every inline code element under `root`. */
function markedByText(root: Element): Record<string, boolean> {
  const out: Record<string, boolean> = {};
  for (const code of root.querySelectorAll('code[data-streamdown-codespan]')) {
    out[code.textContent ?? ''] = code.classList.contains(SHORT_CODESPAN_CLASS);
  }
  return out;
}

const CASES: Array<[string, string]> = [
  ['a paragraph', `at the limit \`${SHORT}\` and one past it \`${LONG}\``],
  ['a table cell', `| key | value |\n| --- | --- |\n| \`${SHORT}\` | \`${LONG}\` |`],
  ['a link label', `[\`${SHORT}\`](https://example.test/a) [\`${LONG}\`](https://example.test/b)`],
];

describe('short inline code marker, both render paths', () => {
  it.each(CASES)('marks code up to the limit and nothing longer in %s', async (_name, source) => {
    const { componentPath, staticPath } = mount(source);
    const expected = { [SHORT]: true, [LONG]: false };

    await waitFor(() => {
      expect(markedByText(componentPath)).toEqual(expected);
      expect(markedByText(staticPath)).toEqual(expected);
    });
  });
});
