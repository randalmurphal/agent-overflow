import { describe, expect, it } from 'vitest';
import { renderMarkdown, sanitizeRenderedSvg } from './markdownRender';

describe('renderMarkdown', () => {
  it('renders one-character inline math markers', () => {
    const html = renderMarkdown('Solve $x$ now.');

    expect(html).toContain('class="math-inline"');
    expect(html).toContain('x');
  });

  it('does not rewrite math-looking text inside inline code', () => {
    const html = renderMarkdown('Use `$x$` literally.');

    expect(html).toContain('<code>$x$</code>');
    expect(html).not.toContain('math-inline');
  });

  it('does not rewrite math-looking text inside fenced code', () => {
    const html = renderMarkdown('```ts\nconst value = "$x$";\n```');

    expect(html).toContain('const value');
    expect(html).toContain('$x$');
    expect(html).not.toContain('math-inline');
  });

  it('renders display math markers outside code blocks', () => {
    const html = renderMarkdown('$$\nx + y\n$$');

    expect(html).toContain('class="math-display"');
    expect(html).toContain('x + y');
  });

  it('strips SVG data URI markdown images', () => {
    const html = renderMarkdown('![bad](data:image/svg+xml;base64,PHN2ZyBvbmxvYWQ9YWxlcnQoMSk+)');

    expect(html).toContain('<img');
    expect(html).not.toContain('data:image/svg+xml');
  });

  it('strips SVG data URI raw image attributes', () => {
    const html = renderMarkdown('<img src="data:image/svg+xml,%3Csvg%20onload=alert(1)%3E">');

    expect(html).toContain('<img');
    expect(html).not.toContain('data:image/svg+xml');
  });

  it('removes external image tags from sanitized Mermaid SVG', () => {
    const svg = sanitizeRenderedSvg(
      '<svg><image href="data:image/svg+xml,%3Csvg%20onload=alert(1)%3E"></image><text>ok</text></svg>',
    );

    expect(svg).toContain('<text>ok</text>');
    expect(svg).not.toContain('<image');
    expect(svg).not.toContain('data:image/svg+xml');
  });
});
