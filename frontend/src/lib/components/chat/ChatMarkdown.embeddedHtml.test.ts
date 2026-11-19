import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import { markdownFindText } from './markdown/markdownFindText';

// The embedded-HTML gate, both directions. Forge comment surfaces pass
// `embeddedHtml` and get structural details/summary plus the allowlist
// sanitizer; agent chat leaves the prop off and renders NO html at all —
// the same `renderHtml={false}` posture as before the feature existed.

const FORGE_BODY = [
  '<details>',
  '<summary><b>3 issues</b> found</summary>',
  '',
  'A paragraph with **markdown** and <sub>inline html</sub>.',
  '',
  '</details>',
  '',
  '<table><tr><td>raw html cell</td></tr></table>',
  '',
  '<blink>mystery</blink>',
].join('\n');

describe('<ChatMarkdown> embedded HTML', () => {
  it('renders details, inline pairs and sanitized blocks when opted in', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: FORGE_BODY, embeddedHtml: true },
    });

    await waitFor(() => {
      const details = container.querySelector('details');
      expect(details).not.toBeNull();
      expect(details?.hasAttribute('open')).toBe(false);
      const summary = details?.querySelector('summary');
      expect(summary?.textContent).toContain('3 issues');
      // A summary on its HTML block's lines is HTML: the sanitizer keeps <b>.
      expect(summary?.querySelector('b')?.textContent).toBe('3 issues');
      // Markdown inside the dropdown still parses as markdown.
      expect(details?.querySelector('p strong')?.textContent).toBe('markdown');
      expect(details?.querySelector('sub')?.textContent).toBe('inline html');
      // The raw table went through the sanitizer.
      expect(container.querySelector('td')?.textContent).toBe('raw html cell');
      // An element off the allowlist is not rendered; its text shows, as
      // a forge shows it.
      expect(container.querySelector('blink')).toBeNull();
      expect(container.textContent).not.toContain('<blink>');
      expect(container.textContent).toContain('mystery');
    });
  });

  it('keeps an image title however the image is written', async () => {
    for (const source of [
      'see ![alt](https://example.invalid/a.png "badge title") here',
      'see <img src="https://example.invalid/a.png" alt="alt" title="badge title"> here',
      '<p><img src="https://example.invalid/a.png" alt="alt" title="badge title"></p>',
    ]) {
      const { container, unmount } = render(ChatMarkdown, { props: { source, embeddedHtml: true } });
      await waitFor(() => {
        expect(container.querySelector('img')?.getAttribute('title'), source).toBe('badge title');
      });
      unmount();
    }
  });

  it('finds a closed dropdown summary that follows a comment', async () => {
    const source = '<details><!-- rationale -->\n<summary>needle label</summary>\n\nhidden body\n\n</details>';
    const { container } = render(ChatMarkdown, { props: { source, embeddedHtml: true } });
    await waitFor(() => {
      expect(container.querySelector('details > summary')?.textContent).toBe('needle label');
    });
    const found = markdownFindText(source, { embeddedHtml: true });
    expect(found.runs).toEqual(['needle label']);
    expect(found.all.map((run) => run.text)).toEqual(['needle label', 'hidden body']);
  });

  it('honors an authored open attribute', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '<details open>\n<summary>s</summary>\n\nbody\n\n</details>',
        embeddedHtml: true,
      },
    });
    await waitFor(() => {
      expect(container.querySelector('details')?.hasAttribute('open')).toBe(true);
    });
  });

  it('renders no html at all without the opt-in (agent chat posture)', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: FORGE_BODY },
    });

    await new Promise((resolve) => setTimeout(resolve, 16));

    expect(container.querySelector('details')).toBeNull();
    expect(container.querySelector('table')).toBeNull();
    // The html tokens render as nothing — no literal tag text either.
    expect(container.textContent).not.toContain('<blink>');
  });

  it('never renders script even when opted in', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '<script>window.__pwned = true</script>\n\nprose',
        embeddedHtml: true,
      },
    });
    await waitFor(() => {
      expect(container.textContent).toContain('prose');
      expect(container.querySelector('script')).toBeNull();
      expect((window as { __pwned?: boolean }).__pwned).toBeUndefined();
      // Removed with its source, which neither shows as text.
      expect(container.textContent).not.toContain('script');
      expect(container.textContent).not.toContain('__pwned');
    });
  });
});

// A CodeRabbit "review in progress" comment as the forge holds it: a linked
// themed button, then an alert whose body is three dropdowns and a fence.
const PICTURE =
  '<picture><source media="(prefers-color-scheme: dark)" srcset="https://assets.example/explain-dark.svg">' +
  '<source media="(prefers-color-scheme: light)" srcset="https://assets.example/explain.svg">' +
  '<img src="https://assets.example/explain.svg" alt="Explain Change"></picture>';
const BOT_COMMENT = [
  '<!-- This is an auto-generated comment -->',
  `<a href="https://app.example/change/1">${PICTURE}</a>`,
  '',
  '> [!NOTE]',
  '> Currently processing new changes in this PR.',
  '> ',
  '> <details>',
  '> <summary>Run configuration</summary>',
  '> ',
  '> - **Source mode**: Auto',
  '> - **Review profile**: CHILL',
  '> ',
  '> </details>',
  '> ',
  '> <details>',
  '> <summary>Commits</summary>',
  '> ',
  '> Reviewing files that changed from the base of the PR.',
  '> ',
  '> </details>',
  '> ',
  '> ```ascii',
  '> | Plot twist |',
  '> ```',
].join('\n');

describe('<ChatMarkdown> embedded HTML in a bot comment', () => {
  it('keeps each dropdown body inside its dropdown, closed', async () => {
    const { container } = render(ChatMarkdown, { props: { source: BOT_COMMENT, embeddedHtml: true } });
    await waitFor(() => expect(container.querySelectorAll('details')).toHaveLength(2));

    const alert = container.querySelector('[data-streamdown-alert]');
    const [config, commits] = Array.from(container.querySelectorAll('details'));
    expect(alert?.contains(config)).toBe(true);
    expect(config.open).toBe(false);
    expect(config.querySelector('summary')?.textContent?.trim()).toBe('Run configuration');
    expect(Array.from(config.querySelectorAll('li'), (item) => item.textContent?.trim())).toEqual([
      'Source mode: Auto',
      'Review profile: CHILL',
    ]);
    expect(commits.querySelector('p')?.textContent).toBe('Reviewing files that changed from the base of the PR.');
    // Nothing a dropdown holds renders beside it.
    expect(alert?.querySelectorAll(':scope > ul, :scope > details ~ p')).toHaveLength(0);
    expect(alert?.textContent).toContain('| Plot twist |');
  });

  it('renders the linked picture as one linked image, no tag text', async () => {
    const { container } = render(ChatMarkdown, { props: { source: BOT_COMMENT, embeddedHtml: true } });
    await waitFor(() => expect(container.querySelector('a img')).not.toBeNull());

    const link = container.querySelector('a[href="https://app.example/change/1"]');
    const images = link?.querySelectorAll('img') ?? [];
    expect(images).toHaveLength(1);
    expect(images[0].getAttribute('src')).toBe('https://assets.example/explain.svg');
    expect(images[0].getAttribute('alt')).toBe('Explain Change');
    expect(link?.textContent?.trim()).toBe('');
    expect(container.textContent).not.toContain('picture');
    expect(container.textContent).not.toContain('srcset');
  });

  it('paints an authored image size', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: 'An <img src="https://assets.example/icon.png" height="20" alt="icon"> icon.', embeddedHtml: true },
    });
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    const image = container.querySelector('img') as HTMLImageElement;
    expect(image.style.height).toBe('20px');
    expect(image.style.width).toBe('');
  });

  it('renders none of it without the opt-in', async () => {
    const { container } = render(ChatMarkdown, { props: { source: BOT_COMMENT } });
    await waitFor(() => expect(container.textContent).toContain('Currently processing'));
    expect(container.querySelector('details')).toBeNull();
    expect(container.querySelector('img')).toBeNull();
    // Entity references stay as written in agent chat.
    const plain = render(ChatMarkdown, { props: { source: 'A&nbsp;B <ins>x</ins> tail' } });
    await waitFor(() => expect(plain.container.textContent).toContain('tail'));
    expect(plain.container.textContent).toContain('A&nbsp;B');
    expect(plain.container.querySelector('ins')).toBeNull();
  });
});
