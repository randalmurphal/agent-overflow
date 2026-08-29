import { render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ChatMarkdown from './ChatMarkdown.svelte';

const SECTION_COUNT = 16;

function richSection(index: number): string {
  return [
    `### Working set ${index}`,
    '',
    `The active pane keeps **streamed Markdown**, \`inline code\`, and [a link](https://example.test/${index}) readable.`,
    '',
    '- The parser carries state across wire chunks.',
    '- The reveal queue stays bounded.',
    '- The spring follows the live edge.',
    '',
    '| Iteration | Parser | Scroll |',
    '| ---: | :--- | :--- |',
    `| ${index} | active | following |`,
    '',
    `> Visible progress marker ${index}.`,
  ].join('\n');
}

function countNodes(root: Element): {
  elements: number;
  comments: number;
  emptyText: number;
} {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_ALL);
  let elements = 0;
  let comments = 0;
  let emptyText = 0;
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (node.nodeType === Node.ELEMENT_NODE) elements++;
    if (node.nodeType === Node.COMMENT_NODE) comments++;
    if (node.nodeType === Node.TEXT_NODE && node.textContent === '') emptyText++;
  }
  return { elements, comments, emptyText };
}

describe('<ChatMarkdown> DOM budget', () => {
  it('does not retain control-flow nodes for every plain text leaf', async () => {
    const source = Array.from({ length: SECTION_COUNT }, (_, index) =>
      richSection(index + 1),
    ).join('\n\n');
    const { container } = render(ChatMarkdown, { props: { source } });

    await waitFor(() => {
      expect(container.textContent).toContain(`Visible progress marker ${SECTION_COUNT}.`);
    });

    const markdown = container.querySelector('.md-committed');
    if (!markdown) throw new Error('settled Markdown root did not mount');
    const counts = countNodes(markdown);

    // A component, slot, and nested branch for every leaf retain many empty
    // Text nodes and Svelte anchors in Blink after the answer settles. Fixed
    // completed blocks need fewer control nodes than visible elements; this
    // leaves room for Streamdown's per-block boundary without permitting the
    // old per-token multiplier.
    expect(counts.emptyText).toBeLessThanOrEqual(counts.elements);
    expect(counts.comments).toBeLessThanOrEqual(counts.elements);
  });
});
