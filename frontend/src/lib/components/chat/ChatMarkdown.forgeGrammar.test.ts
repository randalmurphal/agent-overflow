import { beforeEach, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import { markedForgeGrammar } from '../../markdown';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { markdownFindText } from './markdown/markdownFindText';

// Agent chat's own grammar is text on a forge surface, as each forge shows
// it; GitLab's `>>>` quote takes an alert marker and its front matter opens
// only the document; an autolink keeps its backslash everywhere. Agent chat
// keeps its grammar.

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
});

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

const CHAT_GRAMMAR = [
  ':+1: nice work',
  '- [ ] :construction: task',
  'Power x^2^ here and H~2~O',
  // A block of its own on a forge, shaped as chat's marker.
  '[center]\n# Centered\n[/center]',
  '<Note kind="x">inside</Note>',
  'Price a $ x$ b',
].join('\n\n');
const SOURCE = [
  CHAT_GRAMMAR,
  'Autolink <https://example.invalid/a\\b>',
  '>>> [!note] Things to consider\nYou should consider this\n>>>',
  'Intro',
  '+++\nkey = 1\n+++',
  'last line',
].join('\n\n');
const HREF = 'https://example.invalid/a%5Cb';

async function settled(container: HTMLElement): Promise<void> {
  await waitFor(() => expect(container.textContent).toContain('last line'));
}

function expectNoChatGrammar(root: Element): void {
  expect(root.querySelector('dl, sup, sub, .katex, [data-streamdown-mdx]')).toBeNull();
  for (const text of ['^2^', '[center]', 'Centered', '[/center]', 'inside', '$ x$']) expect(root.textContent).toContain(text);
}

describe.each([
  ['GitHub', GITHUB],
  ['GitLab', GITLAB],
] as const)('a %s surface', (name, forgeSource) => {
  it.each([false, true])('shows agent chat grammar as text (streaming %s)', async (streaming) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SOURCE, forgeSource, streaming } });
    await settled(container);
    expectNoChatGrammar(container);
    expect([...container.querySelectorAll('del')].map((del) => del.textContent)).toEqual(['2']);
    expect(container.querySelector<HTMLAnchorElement>('a[href*="example.invalid/a"]')!.getAttribute('href')).toBe(HREF);
    // Front matter only opens a document.
    expect(container.querySelector('pre')).toBeNull();
    expect(container.textContent).toContain('key = 1');
    const alert = container.querySelector('[data-streamdown-alert]');
    if (name === 'GitLab') {
      expect(alert!.querySelector('[data-alert-title]')!.textContent!.trim()).toBe('Things to consider');
      expect(alert!.textContent).toContain('You should consider this');
      // One alert, not one inside the empty quotes `>>>` would otherwise open.
      expect(alert!.parentElement!.closest('blockquote')).toBeNull();
      expect(container.textContent).not.toContain('>>>');
    } else {
      expect(alert).toBeNull();
      expect(container.querySelector('blockquote')!.textContent).toContain('[!note] Things to consider');
    }
  });
});

describe('find on a forge surface', () => {
  it.each([
    ['GitHub', GITHUB],
    ['GitLab', GITLAB],
  ] as const)('counts the %s alignment markers, which it shows', (_name, forgeSource) => {
    const runs = markdownFindText(SOURCE, { embeddedHtml: true, forgeSource }).runs;
    expect(runs).toContain('[center]');
    expect(runs).toContain('[/center]');
    expect(markdownFindText(SOURCE, { embeddedHtml: false }).runs).not.toContain('[center]');
  });
});

describe('agent chat grammar', () => {
  it('stays agent chat grammar in agent chat', async () => {
    const { container } = render(ChatMarkdown, { props: { source: `${SOURCE}` } });
    await settled(container);
    expect(container.querySelector('dl dt')!.textContent).toBe('+1');
    expect(container.querySelector('li dl')).not.toBeNull();
    expect(container.querySelector('sup')!.textContent).toBe('2');
    expect(container.querySelector('sub')!.textContent).toBe('2');
    expect(container.textContent).not.toContain('[center]');
    expect(container.querySelector<HTMLAnchorElement>('a[href*="example.invalid/a"]')!.getAttribute('href')).toBe(HREF);
  });

  it('is text on both render paths under a forge grammar', async () => {
    const { container } = render(CompactStaticMarkdownHarness, { source: `${CHAT_GRAMMAR}\n\nlast line`, extensions: [markedForgeGrammar] });
    const componentPath = container.querySelector<HTMLElement>('[data-full-token-tree]')!;
    const staticPath = container.querySelector<HTMLElement>('[data-compact-static]')!;
    await settled(componentPath);
    await settled(staticPath);
    for (const root of [componentPath, staticPath]) expectNoChatGrammar(root);
    expect(componentPath.textContent).toBe(staticPath.textContent);
  });
});
