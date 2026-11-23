import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ChatMarkdown from './ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './markdown/forgeAttachmentContext';
import { markdownFindText } from './markdown/markdownFindText';
import { findTextSegments } from '../../utils/findHighlights';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// HTML block lines as both forges render them, on the settled (compact) and
// streaming (component) paths, with find's projection held to the DOM: the
// lines of an HTML block are HTML, and Markdown resumes after a blank line.

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
const FORGES = [['GitHub', GITHUB], ['GitLab', GITLAB]] as const;

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  document.body.innerHTML = '';
});

async function mounted(source: string, forge: ForgeAttachmentSource, streaming: boolean): Promise<HTMLElement> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  const { container } = render(ChatMarkdown, {
    target: host,
    props: { source: `${source}\n\nlast line`, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose', streaming },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
  });
  await waitFor(() => expect(container.querySelector('.markdown-body')?.textContent).toContain('last line'));
  return container.querySelector<HTMLElement>('.markdown-body')!;
}

const words = (runs: readonly string[]) => runs.map((run) => run.replace(/\s+/g, ' ').trim()).filter(Boolean);

/** The rendered text of `element`, whitespace folded. */
const text = (element: Element | null | undefined) => element?.textContent?.replace(/\s+/g, ' ').trim();

const PATHS = [false, true].flatMap((streaming) => FORGES.map(([name, forge]) => [name, streaming, forge] as const));

describe('the lines of a <details> HTML block', () => {
  it.each(PATHS)('are HTML on %s (streaming %s)', async (_name, streaming, forge) => {
    const cases: [string, string, string, string][] = [
      // [source, summary text, body text, a selector the summary holds]
      ['<details><summary>Inline summary with `code` and **bold**</summary>\n\nbody **md**\n\n</details>', 'Inline summary with `code` and **bold**', 'body md', ''],
      ['<details open>\n<summary>Tight</summary>\nbody with **stars** and `code`\n</details>', 'Tight', 'body with **stars** and `code`', ''],
      ['<details open>\n<summary>S</summary>\na snake_case_name and 2 * 3 * 4\n</details>', 'S', 'a snake_case_name and 2 * 3 * 4', ''],
      ['<details open><summary><b>Bold</b> &amp; <code>x</code></summary>\n\nbody\n\n</details>', 'Bold & x', 'body', 'b + code'],
      ['<details open>\n<summary>S</summary>\nraw **one**\n\nmarkdown **two**\n</details>', 'S', 'raw **one** markdown two', ''],
    ];
    for (const [source, summary, body, holds] of cases) {
      document.body.innerHTML = '';
      const root = await mounted(source, forge, streaming);
      const details = root.querySelector('details')!;
      expect(details.open, source).toBe(source.startsWith('<details open'));
      const summaryElement = details.querySelector('summary')!;
      expect(text(summaryElement), source).toBe(summary);
      if (holds) expect(summaryElement.querySelector(holds), source).not.toBeNull();
      expect(text(details)?.slice(summary.length).trim(), source).toBe(body);
      // Markdown only where the source resumes it.
      expect(details.querySelectorAll('strong').length, source).toBe(source.includes('markdown **two**') || source.includes('body **md**') ? 1 : 0);
      expect(words(markdownFindText(source, { embeddedHtml: true, forgeSource: forge }).runs)).toEqual(words(findTextSegments(root)).slice(0, -1));
    }
  });

  it.each(PATHS)('keeps a Markdown summary set apart by blank lines Markdown on %s (streaming %s)', async (_name, streaming, forge) => {
    const root = await mounted('<details>\n<summary>\n\n**Markdown** summary\n\n</summary>\n\nbody\n\n</details>', forge, streaming);
    expect(root.querySelector('summary strong')?.textContent).toBe('Markdown');
  });
});

describe('an image a stripped wrapper leaves at the top of an HTML block', () => {
  const FIGURE = '<figure><img src="https://example.invalid/f.png" alt="fig"><figcaption>Figure caption</figcaption></figure>';
  const NO_IMAGE = '<figure>lead <b>bold</b><figcaption>Cap two</figcaption></figure>';
  const SECTION = '<section><img src="https://example.invalid/s.png" alt="s">Section text</section>';

  it.each([false, true])('is a paragraph of its own on GitHub (streaming %s)', async (streaming) => {
    const root = await mounted(`${FIGURE}\n\n${NO_IMAGE}\n\n${SECTION}`, GITHUB, streaming);
    expect(root.querySelector('figure, figcaption')).toBeNull();
    const image = root.querySelector('img[alt="fig"]')!;
    expect(image.parentElement!.localName).toBe('p');
    expect(image.parentElement!.textContent).toBe('');
    // The caption starts the line below the image.
    const caption = document.createRange();
    caption.selectNodeContents(image.parentElement!.parentElement!);
    caption.setStartAfter(image.parentElement!);
    expect(caption.toString()).toContain('Figure caption');
    expect(caption.getClientRects()[0].top).toBeGreaterThanOrEqual(image.getBoundingClientRect().bottom - 1);
    // No image, no paragraph: the caption runs on.
    expect(text(root)).toContain('lead boldCap two');
    expect(root.querySelector('b')!.closest('p')).toBeNull();
    // A kept wrapper holds its image in its line.
    const section = root.querySelector('section')!;
    expect(section.querySelector(':scope > img[alt="s"]')).not.toBeNull();
    expect(text(section)).toBe('Section text');
  });

  it.each([false, true])('stays in its figure on GitLab (streaming %s)', async (streaming) => {
    const root = await mounted(`${FIGURE}\n\n${SECTION}`, GITLAB, streaming);
    expect(root.querySelector('figure > img[alt="fig"]')).not.toBeNull();
    expect(text(root.querySelector('figure > figcaption'))).toBe('Figure caption');
    expect(root.querySelector('section > img[alt="s"]')).not.toBeNull();
  });

  it.each(FORGES)('is counted by find as %s shows it', async (_name, forge) => {
    const source = `${FIGURE}\n\nbetween\n\n${NO_IMAGE}\n\n${SECTION}`;
    const root = await mounted(source, forge, false);
    expect(words(markdownFindText(source, { embeddedHtml: true, forgeSource: forge }).runs)).toEqual(words(findTextSegments(root)).slice(0, -1));
  });
});
