import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';

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
      expect(summary?.querySelector('strong')).not.toBeNull();
      // Markdown inside the dropdown still parses as markdown.
      expect(details?.querySelector('p strong')?.textContent).toBe('markdown');
      expect(details?.querySelector('sub')?.textContent).toBe('inline html');
      // The raw table went through the sanitizer.
      expect(container.querySelector('td')?.textContent).toBe('raw html cell');
      // Unknown html is escaped literal text, never dropped, never an element.
      expect(container.querySelector('blink')).toBeNull();
      expect(container.textContent).toContain('<blink>');
      expect(container.textContent).toContain('mystery');
    });
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
      // Visible as literal text instead of vanishing.
      expect(container.textContent).toContain('<script>');
    });
  });
});
