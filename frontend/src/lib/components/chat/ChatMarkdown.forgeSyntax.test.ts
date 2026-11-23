import { beforeEach, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import {
  markedColorChips,
  markedCssColorChips,
  markedDescriptionLists,
  markedInapplicableTaskList,
  markedInlineDiff,
  markedJsonTable,
  markedMultilineBlockquote,
  markedStrikethrough,
  markedTableTaskCells,
  type Extension,
} from '../../markdown';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';
import { setBindingMock } from '../../../test/mocks/bindings-app';

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
});

// Each forge's own Markdown syntax as the review pane renders it: on the
// forge that has it, not on the other one, and never in agent chat; and the
// same DOM from the component and compact static paths.

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

const SOURCE = [
  'Struck ~one~ and ~~two~~ and H~2~O.',
  'Diff {+ added +} and {- removed -}.',
  'Colors `#0969DA` `#F00` `RGB(0,255,0)`.',
  '>>>\nquoted one\n\nquoted two\n>>>',
  '- [~] skipped\n- [x] done',
  '| Done | Task |\n| --- | --- |\n| [x] | one |',
  'Fruits\n: apple\n: orange',
  '```json:table\n{"items": [{"a": "11", "b": "22"}], "caption": "Numbers"}\n```',
  'last line',
].join('\n\n');

/** What a reader sees: elements, the attributes that change what shows,
 * checkbox state, and text. */
function shape(node: Node): string {
  if (node.nodeType === Node.TEXT_NODE) return (node.textContent ?? '').replace(/\s+/g, ' ');
  if (!(node instanceof Element)) return '';
  if (node.matches('button, [aria-hidden="true"]:not([data-streamdown-color-swatch])')) return '';
  const tag = node.tagName.toLowerCase();
  const own: string[] = [];
  if (node instanceof HTMLInputElement) own.push(`checked=${node.checked}`);
  if (node instanceof HTMLElement && node.style.backgroundColor) own.push(`bg=${node.style.backgroundColor}`);
  const inner = [...node.childNodes].map(shape).join('');
  return tag === 'div' || tag === 'span' && own.length === 0 ? inner : `<${tag}${own.length ? ' ' + own.join(' ') : ''}>${inner}</${tag}>`;
}

async function settled(container: HTMLElement): Promise<void> {
  await waitFor(() => expect(container.textContent).toContain('last line'));
}

describe('forge syntax on the two render paths', () => {
  it.each([
    ['GitHub', [markedStrikethrough, markedColorChips]],
    [
      'GitLab',
      [
        markedStrikethrough,
        markedCssColorChips,
        markedInlineDiff,
        markedMultilineBlockquote,
        markedInapplicableTaskList,
        markedTableTaskCells,
        markedDescriptionLists,
        markedJsonTable,
      ],
    ],
  ] as [string, Extension[]][])('render the same %s DOM', async (_name, extensions) => {
    const { container } = render(CompactStaticMarkdownHarness, { source: SOURCE, extensions });
    const componentPath = container.querySelector<HTMLElement>('[data-full-token-tree]')!;
    const staticPath = container.querySelector<HTMLElement>('[data-compact-static]')!;
    await settled(componentPath);
    await settled(staticPath);
    const shown = shape(staticPath);
    expect(shape(componentPath)).toBe(shown);
    // Every block is fixed static HTML: a component island carries Svelte's
    // comment anchors inside its block elements. Only the GitLab set claims
    // every box: here, without a forge scope, `[~]` is otherwise a chat
    // citation, a component.
    if (extensions.includes(markedJsonTable)) {
      for (const block of staticPath.querySelectorAll('[data-streamdown-last-block] > *')) {
        expect(document.createTreeWalker(block, NodeFilter.SHOW_COMMENT).nextNode(), block.tagName).toBeNull();
      }
    }
    expect(shown).toContain('<del>one</del>');
    expect(shown).toContain('#0969DA<span bg=');
    if (extensions.includes(markedJsonTable)) {
      for (const part of ['<ins> added </ins>', '<del>skipped</del>', '<td><input checked=true></input></td>', '<dt>Fruits</dt>', '<caption>Numbers</caption>']) {
        expect(shown).toContain(part);
      }
    }
  });
});

describe('forge syntax on a GitLab surface', () => {
  it.each([false, true])('renders GitLab syntax (streaming %s)', async (streaming) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SOURCE, forgeSource: GITLAB, streaming } });
    await settled(container);
    expect([...container.querySelectorAll('del')].map((del) => del.textContent)).toEqual(['one', 'two', '2', ' removed ', 'skipped']);
    expect(container.querySelector('ins')!.textContent).toBe(' added ');
    expect(container.textContent).not.toContain('{+');
    // Each swatch sits in its code; the browser test checks the painted color.
    const swatched = [...container.querySelectorAll('code')].filter((code) => code.querySelector('[data-streamdown-color-swatch]'));
    expect(swatched.map((code) => code.textContent)).toEqual(['#0969DA', '#F00', 'RGB(0,255,0)']);
    const quote = container.querySelector('blockquote')!;
    expect([...quote.querySelectorAll('p')].map((p) => p.textContent)).toEqual(['quoted one', 'quoted two']);
    expect(container.textContent).not.toContain('>>>');
    const [skipped, done] = container.querySelectorAll<HTMLInputElement>('li > input[type="checkbox"]');
    expect([skipped.checked, done.checked]).toEqual([false, true]);
    expect(container.querySelector<HTMLInputElement>('td > input[type="checkbox"]')!.checked).toBe(true);
    expect(container.querySelector('dl dt')!.textContent).toBe('Fruits');
    expect([...container.querySelectorAll('dl dd')].map((dd) => dd.textContent)).toEqual(['apple', 'orange']);
    const json = [...container.querySelectorAll('table')].at(-1)!;
    expect(json.querySelector('caption')!.textContent).toBe('Numbers');
    expect([...json.querySelectorAll('th, td')].map((cell) => cell.textContent)).toEqual(['a', 'b', '11', '22']);
  });

  it('shows GitLab front matter as a code block', async () => {
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: '---\ntitle: About\n---\n\nlast line', forgeSource: GITLAB },
    });
    await settled(container);
    expect(container.querySelector('hr')).toBeNull();
    expect(container.querySelector('h2')).toBeNull();
    expect(container.querySelector('pre')!.textContent).toContain('title: About');
  });
});

describe('forge syntax on a GitHub surface', () => {
  it('renders GitHub syntax and leaves GitLab syntax as text', async () => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SOURCE, forgeSource: GITHUB } });
    await settled(container);
    expect([...container.querySelectorAll('del')].map((del) => del.textContent)).toEqual(['one', 'two', '2']);
    expect(container.querySelector('ins')).toBeNull();
    expect(container.textContent).toContain('{+ added +}');
    const swatched = [...container.querySelectorAll('code')].filter((code) => code.querySelector('[data-streamdown-color-swatch]'));
    expect(swatched.map((code) => code.textContent)).toEqual(['#0969DA']);
    expect(container.textContent).toContain('[~] skipped');
    expect(container.querySelector('td input')).toBeNull();
    expect(container.querySelector('dl')).toBeNull();
    expect(container.querySelector('caption')).toBeNull();
    expect(container.textContent).toContain('"caption": "Numbers"');
  });

  it('keeps front matter as a rule and a heading', async () => {
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: '---\ntitle: About\n---\n\nlast line', forgeSource: GITHUB },
    });
    await settled(container);
    expect(container.querySelector('hr')).not.toBeNull();
    expect(container.querySelector('h2')!.textContent).toBe('title: About');
  });
});

describe('forge syntax in agent chat', () => {
  it('keeps subscript and renders no forge syntax', async () => {
    const { container } = render(ChatMarkdown, { props: { source: SOURCE } });
    await settled(container);
    expect([...container.querySelectorAll('sub')].map((sub) => sub.textContent)).toEqual(['one', '2']);
    expect([...container.querySelectorAll('del')].map((del) => del.textContent)).toEqual(['two']);
    expect(container.querySelector('ins, dl, caption, td input, [data-streamdown-color-swatch]')).toBeNull();
    expect(container.textContent).toContain('[~] skipped');
  });
});

describe('forge strikethrough pairs tildes by delimiter runs', () => {
  const LABELS = 'Label: ~1, ~bug, ~"feature request", ~"priority::high", group/project~bug, /group/project~bug\n\nlast line';
  const STRUCK = '<p>Label: ~1, ~bug, <del>"feature request", <del>"priority::high", group/project</del>bug, /group/project</del>bug</p>';

  it('renders the same DOM on both render paths', async () => {
    const { container } = render(CompactStaticMarkdownHarness, { source: LABELS, extensions: [markedStrikethrough] });
    const componentPath = container.querySelector<HTMLElement>('[data-full-token-tree]')!;
    const staticPath = container.querySelector<HTMLElement>('[data-compact-static]')!;
    await settled(componentPath);
    await settled(staticPath);
    expect(shape(staticPath)).toContain(STRUCK);
    expect(shape(componentPath)).toBe(shape(staticPath));
  });

  it.each([
    ['GitHub', false, GITHUB],
    ['GitHub', true, GITHUB],
    ['GitLab', false, GITLAB],
    ['GitLab', true, GITLAB],
  ] as const)('on %s (streaming %s)', async (_name, streaming, forgeSource) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: LABELS, forgeSource, streaming } });
    await settled(container);
    expect(shape(container.querySelector('p')!)).toBe(STRUCK);
  });

  it('leaves agent chat without strikethrough', async () => {
    const { container } = render(ChatMarkdown, { props: { source: LABELS } });
    await settled(container);
    expect(container.querySelector('del')).toBeNull();
  });
});
