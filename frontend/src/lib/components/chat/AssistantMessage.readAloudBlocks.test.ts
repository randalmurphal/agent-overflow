// A read-aloud section's block indexes name the elements a settled reply
// renders: `renderedBlocks(lex(markdown))[i]` is the i-th element child of
// the reply's Markdown wrapper, for every kind of top-level token.

import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import AssistantMessage from './AssistantMessage.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { lex } from '../../markdown';
import { renderedBlocks, speakableSections } from '../../utils/speakableText';

const REPLY = [
  '# Title', '', 'Para one.', '', '- a', '- b', '', '```ts', 'x', '```', '', '$$', 'e', '$$', '',
  '| a | b |', '|---|---|', '| 1 | 2 |', '', '<div>hidden</div>', '', '---', '', '> quote', '',
  '> [!NOTE]', '> heads up', '', '<Aside>', 'Inside.', '', '<Inner>', 'Deep.', '</Inner>', '</Aside>', '',
  '<Note>inline words</Note>', '', '[center]', 'Centered.', '[/center]', '', '![img](x.png)', '',
  '[ref]: https://example.com', '', '<!-- comment -->', '', 'Done[^1].', '', '[^1]: note', '',
  '```mermaid', 'graph TD; A-->B', '```', '', 'Last.',
].join('\n');

const TAGS: Record<string, string> = { paragraph: 'P', heading: 'H1', list: 'UL', blockquote: 'BLOCKQUOTE', hr: 'HR' };

describe('read-aloud block indexes', () => {
  it('match the element children of a settled reply’s Markdown wrapper', async () => {
    const view = render(AssistantMessage, { props: { item: makeItem({ summary: REPLY }) } });
    const body = await waitFor(() => {
      const found = view.container.querySelector('.markdown-body');
      expect(found?.textContent).toContain('Last.');
      return found!;
    });
    // A settled reply renders one wrapper, the committed blocks.
    expect(Array.from(body.children).map((el) => el.className.split(' ')[0])).toEqual(['md-committed']);
    const elements = Array.from(body.children[0].children);
    const blocks = renderedBlocks(lex(REPLY));
    expect(elements).toHaveLength(blocks.length);
    blocks.forEach((token, i) => expect(elements[i].tagName, `${i} ${token.type}`).toBe(TAGS[token.type] ?? 'DIV'));

    const sections = speakableSections(REPLY);
    expect(sections.length).toBeGreaterThan(3);
    // A block's text as rendered, without what is not read (a footnote's
    // number), holds the section's first and last sentence.
    const shown = (i: number): string => (elements[i].textContent ?? '').replace(/\d/g, '').replace(/\s+/g, ' ');
    for (const section of sections) {
      expect(shown(section.firstBlock)).toContain(section.sentences[0]);
      expect(shown(section.lastBlock)).toContain(section.sentences.at(-1));
    }
  });
});
