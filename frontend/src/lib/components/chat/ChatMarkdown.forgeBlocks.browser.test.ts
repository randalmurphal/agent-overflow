import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ChatMarkdown from './ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './markdown/forgeAttachmentContext';
import { markdownFindText } from './markdown/markdownFindText';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Forge block and HTML styling in real Chromium: keycaps, quotes and
// list markers as the review pane paints them, and GitHub's tag filter
// through Chromium's HTML parser.

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  document.body.innerHTML = '';
});

async function mounted(source: string, forge: ForgeAttachmentSource | null): Promise<HTMLElement> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  const { container } = render(ChatMarkdown, {
    target: host,
    props: forge
      ? { source: `${source}\n\nlast line`, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' }
      : { source: `${source}\n\nlast line`, pathRefs: EMPTY_PATH_REFS },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
  });
  await waitFor(() => expect(container.querySelector('.markdown-body')?.textContent).toContain('last line'));
  return container.querySelector<HTMLElement>('.markdown-body')!;
}

describe('forge blocks in Chromium', () => {
  it('draws <kbd> as a keycap, nested keys as caps inside a cap', async () => {
    const root = await mounted('Press <kbd><kbd>Ctrl</kbd>+<kbd>P</kbd></kbd>.', GITHUB);
    const keys = [...root.querySelectorAll<HTMLElement>('kbd')];
    expect(keys).toHaveLength(3);
    for (const key of keys) {
      const style = getComputedStyle(key);
      expect(style.display).toBe('inline-block');
      expect(style.borderTopStyle).toBe('solid');
      expect(parseFloat(style.borderTopWidth)).toBeGreaterThan(0);
    }
    const outer = keys[0].getBoundingClientRect();
    for (const inner of keys.slice(1)) {
      const box = inner.getBoundingClientRect();
      expect(box.left).toBeGreaterThan(outer.left);
      expect(box.right).toBeLessThan(outer.right);
    }
  });

  it('keeps quoted text upright in review prose and italic in agent chat', async () => {
    const review = await mounted('> quoted *stressed*', GITLAB);
    expect(getComputedStyle(review.querySelector('blockquote')!).fontStyle).toBe('normal');
    expect(getComputedStyle(review.querySelector('blockquote em')!).fontStyle).toBe('italic');
    document.body.innerHTML = '';
    const chat = await mounted('> quoted', null);
    expect(getComputedStyle(chat.querySelector('blockquote')!).fontStyle).toBe('italic');
  });

  it('paints the marker an HTML list type names', async () => {
    const root = await mounted(
      '<ol type="a" start="3"><li>c</li><li value="7">g</li></ol>\n\n<ul type="square"><li>x</li></ul>',
      GITHUB,
    );
    const ol = root.querySelector('ol')!;
    expect(getComputedStyle(ol).listStyleType).toBe('lower-alpha');
    expect(ol.start).toBe(3);
    expect(root.querySelector('li[value]')?.getAttribute('value')).toBe('7');
    expect(getComputedStyle(root.querySelector('ul')!).listStyleType).toBe('square');
  });

  it('shows a tag-filtered tag as its source on GitHub only', async () => {
    const source = 'x <textarea>t</textarea> y\n\n<div><textarea>u</textarea> z</div>';
    const github = await mounted(source, GITHUB);
    expect(github.querySelector('textarea')).toBeNull();
    expect(github.textContent).toContain('x <textarea>t</textarea> y');
    expect(github.textContent).toContain('<textarea>u</textarea> z');
    const found = markdownFindText(`${source}\n\nlast line`, { embeddedHtml: true, forgeSource: GITHUB }).runs.join('|');
    expect(found).toContain('x <textarea>t</textarea> y');
    expect(found).toContain('<textarea>u</textarea> z');
    document.body.innerHTML = '';
    const gitlab = await mounted(source, GITLAB);
    expect(gitlab.querySelector('textarea')).toBeNull();
    expect(gitlab.textContent).toContain('x t y');
    expect(gitlab.textContent).not.toContain('<textarea>');
  });
});
