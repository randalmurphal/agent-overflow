import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import { markedCommonmarkList } from '../../markdown';
import { markdownFindText } from './markdown/markdownFindText';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Block structure and HTML as each forge renders them, on its own surface:
// list numbers, alerts, math and the HTML allowlist. Agent chat keeps its
// own rendering of the same sources.

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
const FORGES = [
  ['GitHub', GITHUB],
  ['GitLab', GITLAB],
] as const;

async function forge(source: string, forgeSource: ForgeAttachmentSource, streaming = false): Promise<HTMLElement> {
  const { container } = render(ForgeAttachmentChatHarness, { props: { source: `${source}\n\nlast line`, forgeSource, streaming } });
  await waitFor(() => expect(container.textContent).toContain('last line'));
  return container;
}

async function chat(source: string): Promise<HTMLElement> {
  const { container } = render(ChatMarkdown, { props: { source: `${source}\n\nlast line` } });
  await waitFor(() => expect(container.textContent).toContain('last line'));
  return container;
}

const numbering = (container: HTMLElement) =>
  Array.from(container.querySelectorAll('ol'), (list) => ({
    start: list.getAttribute('start'),
    values: Array.from(list.children, (item) => item.getAttribute('value')),
  }));

describe('ordered list numbers', () => {
  it.each(FORGES)('number from the first item on %s, on both render paths', async (_name, source) => {
    for (const streaming of [false, true]) {
      const container = await forge('1. a\n1. b\n\ntext\n\n3. three\n\n7. seven\n\ntext\n\n0. zero', source, streaming);
      expect(numbering(container), `streaming ${streaming}`).toEqual([
        { start: null, values: [null, null] },
        { start: '3', values: [null, null] },
        { start: '0', values: [null] },
      ]);
    }
  });

  it.each(FORGES)('number a list inside an HTML wrapper on %s', async (_name, source) => {
    const container = await forge('<div align="center">\n\n3. three\n3. four\n\n</div>', source);
    expect(numbering(container)).toEqual([{ start: '3', values: [null, null] }]);
  });

  it.each(FORGES)('start a new list at a new bullet character on %s', async (_name, source) => {
    const container = await forge('* star\n+ plus\n- dash', source);
    expect(container.querySelectorAll('ul')).toHaveLength(3);
  });

  it('keep the number typed for each item in agent chat', async () => {
    const container = await chat('1. a\n1. b\n\ntext\n\n3. three\n\n7. seven\n\n* star\n+ plus');
    expect(numbering(container)).toEqual([
      { start: null, values: ['1', '1'] },
      { start: '3', values: ['3', '7'] },
    ]);
    expect(container.querySelectorAll('ul')).toHaveLength(1);
  });

  it('render the same start on the component and compact static paths', async () => {
    const { container } = render(CompactStaticMarkdownHarness, {
      source: '5. five\n9. six\n\ntail',
      extensions: [markedCommonmarkList],
    });
    const componentPath = container.querySelector('[data-full-token-tree]')!;
    const staticPath = container.querySelector('[data-compact-static]')!;
    await waitFor(() => {
      expect(componentPath.textContent).toContain('tail');
      expect(staticPath.textContent).toContain('tail');
    });
    for (const root of [componentPath, staticPath]) {
      const list = root.querySelector('ol')!;
      expect(list.getAttribute('start')).toBe('5');
      expect(Array.from(list.children, (item) => item.getAttribute('value'))).toEqual([null, null]);
    }
  });
});

describe('alerts', () => {
  const ALERTS = '> [!warning] Data deletion\n> Body text.\n\n- item\n\n  > [!NOTE]\n  > In a list.';

  it('follow GitHub: a marker with text after it, or in a list, is a plain quote', async () => {
    const container = await forge(ALERTS, GITHUB);
    expect(container.querySelector('[data-streamdown-alert]')).toBeNull();
    expect(container.querySelectorAll('blockquote')).toHaveLength(2);
    expect(container.querySelector('blockquote')?.textContent).toContain('[!warning] Data deletion');
  });

  it('follow GitLab: the marker line titles the alert, and an alert in a list is one', async () => {
    const container = await forge(ALERTS, GITLAB);
    const alerts = container.querySelectorAll('[data-streamdown-alert]');
    expect(alerts).toHaveLength(2);
    expect(alerts[0].querySelector('[data-alert-title]')?.textContent?.trim()).toBe('Data deletion');
    expect(alerts[0].querySelector('p')?.textContent).toBe('Body text.');
    expect(alerts[1].closest('li')).not.toBeNull();
    expect(alerts[1].querySelector('[data-alert-title]')?.textContent?.trim()).toBe('note');
    const found = markdownFindText(ALERTS, { embeddedHtml: true, forgeSource: GITLAB });
    expect(found.runs).toContain('Data deletion');
    expect(found.runs).not.toContain('warning');
  });

  it('stay as they were in agent chat', async () => {
    const container = await chat(ALERTS);
    const alerts = container.querySelectorAll('[data-streamdown-alert]');
    expect(alerts).toHaveLength(2);
    expect(alerts[0].querySelector('[data-alert-title]')?.textContent?.trim()).toBe('warning');
    expect(alerts[0].querySelector('p')?.textContent).toContain('Data deletion');
  });
});

describe('math', () => {
  const MATH = 'Inline $`c^2`$ here.\n\n```math\nx = y\n```';

  it.each(FORGES)('renders $`…`$ and a math fence as math on %s', async (_name, source) => {
    const container = await forge(MATH, source);
    await waitFor(() => expect(container.querySelectorAll('[data-math-source]')).toHaveLength(2));
    const [inline, display] = Array.from(container.querySelectorAll('[data-math-source]'));
    expect(inline.getAttribute('data-math-source')).toBe('c^2');
    expect(inline.closest('p')).not.toBeNull();
    expect(display.getAttribute('data-math-source')).toBe('x = y');
    expect(container.textContent).not.toContain('$');
    expect(container.querySelector('pre code')).toBeNull();
  });

  it('renders a dollar code span and a math fence as code in agent chat', async () => {
    const container = await chat(MATH);
    expect(container.querySelector('[data-math-source]')).toBeNull();
    expect(container.querySelector('p code')?.textContent).toBe('c^2');
    expect(container.textContent).toContain('$');
  });
});

describe('HTML off the allowlist', () => {
  const HTML =
    'a <big>big</big> <font color="red">font</font> <script>steal()</script> b\n\n' +
    '<style>p { display: none }</style>\n\n' +
    '<form><input type="checkbox" checked> box <textarea>area</textarea></form>\n\n' +
    'stray </i> end <span style="color:red" title="tip">tip</span>\n\n' +
    'c <textarea>t</textarea> d';

  it.each(FORGES)('shows the text a forge keeps and no tag text on %s', async (name, source) => {
    const container = await forge(HTML, source);
    const body = container.querySelector('.markdown-body')!;
    expect(body.querySelector('big, font, script, style, form, input, textarea, [style]')).toBeNull();
    const text = body.textContent ?? '';
    expect(text).toContain('a big font  b');
    expect(text).not.toContain('steal');
    expect(text).not.toContain('display');
    expect(text).toContain('stray  end tip');
    expect(body.querySelector('span[title="tip"]')?.textContent).toBe('tip');
    // GitHub shows a textarea as its source; GitLab its text.
    expect(text).toContain(name === 'GitHub' ? ' box <textarea>area</textarea>' : ' box area');
    expect(text).not.toMatch(/<(big|font|script|style|form|input|i)\b/);
    // Find reads what the reader sees.
    const found = markdownFindText(HTML, { embeddedHtml: true, forgeSource: source }).runs.join('|');
    expect(found).not.toContain('<big>');
    expect(found).not.toContain('steal');
    expect(found).toContain(name === 'GitHub' ? '<textarea>area</textarea>' : 'area');
    // A tag-filtered inline tag is text in the paragraph's run.
    const inline = name === 'GitHub' ? 'c <textarea>t</textarea> d' : 'c t d';
    expect(text).toContain(inline);
    expect(found).toContain(inline);
  });

  it('links the heading an <a href> wraps', async () => {
    const container = await forge('<a href="https://example.invalid/">\n\n## Heading in a link\n\n</a>', GITHUB);
    const heading = container.querySelector('h2');
    expect(heading?.closest('a')?.getAttribute('href')).toBe('https://example.invalid/');
    expect(container.querySelectorAll('a')).toHaveLength(1);
  });

  it('renders nested <kbd> as keys', async () => {
    const container = await forge('Press <kbd><kbd>Ctrl</kbd>+<kbd>P</kbd></kbd>.', GITLAB);
    expect(Array.from(container.querySelectorAll('kbd > kbd'), (key) => key.textContent)).toEqual(['Ctrl', 'P']);
    expect(container.querySelector('p code')).toBeNull();
  });
});
