import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ChatMarkdown from './ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './markdown/forgeAttachmentContext';
import { markdownFindText } from './markdown/markdownFindText';
import { findTextSegments } from '../../utils/findHighlights';
import { foldFindText } from '../../utils/findMatch';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// An island drawn after the render (math, a Mermaid diagram) whose
// renderer rejects its source shows that source, at the size of the text,
// on every surface; find skips it as it skips a drawn island.

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
      ? { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' }
      : { source, pathRefs: EMPTY_PATH_REFS },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
  });
  await waitFor(() => expect(container.querySelector('.markdown-body')?.textContent).toContain('last line'));
  return container.querySelector<HTMLElement>('.markdown-body')!;
}

const BAD_DIAGRAM = 'graph TD\n  A--> this is not valid ((';

const SURFACES = [
  // Each surface's inline and display math forms.
  ['GitHub', GITHUB, 'Invalid: $\\frac{1}{$ and $`\\undefinedmacro`$', '```math\n\\frac{1}{\n```'],
  ['GitLab', GITLAB, 'Invalid: $\\frac{1}{$ and $`\\undefinedmacro`$', '```math\n\\frac{1}{\n```'],
  ['agent chat', null, 'Invalid: $$\\frac{1}{$$ and $$\\undefinedmacro$$', '$$\n\\frac{1}{\n$$'],
] as const;

describe('an island its renderer rejects', () => {
  it.each(SURFACES)('shows its source on %s', async (_name, forge, inline, display) => {
    const source = `${inline}\n\n${display}\n\n\`\`\`mermaid\n${BAD_DIAGRAM}\n\`\`\`\n\nlast line`;
    const root = await mounted(source, forge);

    // Inline: the formulas read where they stand.
    await waitFor(() => expect(root.querySelectorAll('.math-inline [data-render-failed]')).toHaveLength(2));
    expect(root.querySelector('p')!.innerText).toBe('Invalid: \\frac{1}{ and \\undefinedmacro');

    // Display math: the source once, the host settled on it.
    const display_ = root.querySelector<HTMLElement>('.math-display')!;
    await waitFor(() => {
      expect(display_.querySelector('[data-render-failed]')).not.toBeNull();
      expect(display_.classList.contains('math-rendered')).toBe(true);
    });
    expect(display_.innerText.trim()).toBe('\\frac{1}{');
    expect(display_.querySelectorAll('pre')).toHaveLength(1);

    // The diagram: its source once, in a box the size of that text.
    const diagram = root.querySelector<HTMLElement>('.streamdown-mermaid-host')!;
    await waitFor(() => {
      expect(diagram.querySelector('[data-render-failed]')).not.toBeNull();
      expect(diagram.classList.contains('mermaid-rendered')).toBe(true);
    }, { timeout: 10_000 });
    expect(diagram.innerText.trim()).toBe(BAD_DIAGRAM);
    expect(diagram.querySelectorAll('pre')).toHaveLength(1);
    const pre = diagram.querySelector('pre')!;
    const margins = parseFloat(getComputedStyle(pre).marginTop) + parseFloat(getComputedStyle(pre).marginBottom);
    expect(Math.abs(diagram.getBoundingClientRect().height - pre.getBoundingClientRect().height - margins)).toBeLessThan(1);
    expect(pre.getBoundingClientRect().height).toBeLessThan(120);

    // Find skips every island, drawn or not, as its projection does.
    const text = (run: string) => foldFindText(run).replace(/\s+/g, ' ').trim();
    const dom = findTextSegments(root).map(text).filter(Boolean);
    const projected = markdownFindText(source, { embeddedHtml: forge !== null, forgeSource: forge }).runs.map(text).filter(Boolean);
    expect(projected).toEqual(dom);
  });

  it('still typesets a valid formula and draws a valid diagram', async () => {
    const root = await mounted('Valid $x^2$ here.\n\n```mermaid\ngraph TD\n  A --> B\n```\n\nlast line', GITHUB);
    await waitFor(() => expect(root.querySelector('.math-inline .katex')).not.toBeNull());
    await waitFor(() => expect(root.querySelector('[data-mermaid-svg] *')).not.toBeNull(), { timeout: 10_000 });
    expect(root.querySelector('[data-render-failed]')).toBeNull();
  });
});
