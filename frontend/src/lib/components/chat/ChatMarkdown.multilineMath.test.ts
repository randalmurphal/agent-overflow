import { describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';

// Regression coverage for the patched marked-math block rule.
//
// Upstream svelte-streamdown ships a block-math regex that recognises
// only two shapes:
//   1. `$$\nCONTENT\n$$`  — canonical newline-delimited multiline.
//   2. `$$CONTENT$$`       — single-line, no newlines or `$` in CONTENT.
//
// LLMs frequently emit multi-line matrices like:
//   $$ \begin{pmatrix} a & b \\ c & d \end{pmatrix} \begin{pmatrix} x \\ y
//   \end{pmatrix} = \begin{pmatrix}
//   ax + by \\
//   cx + dy
//   \end{pmatrix}
//   $$
// which open with a space after the `$$` (not a newline) AND contain
// internal newlines — neither legacy alternative matches, and the
// whole block fell through to plain paragraph rendering (backslashes
// and `\begin` literal, no KaTeX typesetting). The user-visible symptom
// during streaming was "started to render right, then turned back into
// raw markdown text" — pre-newline the source matched the *inline*
// `$$X$$` rule, post-newline that rule failed and nothing took over.
//
// Our patch adds a third block-rule alternative whose content allows
// internal newlines and `$`, with a strict `(?=\n|$)` lookahead after
// the closing `$$` so adjacent inline-style `$$X$$` blocks on the same
// line continue to match the original single-line alternative first.

const katexCalls: string[] = [];

vi.mock('katex', () => ({
  default: {
    renderToString: vi.fn((code: string) => {
      katexCalls.push(code);
      return `<span class="katex" data-rendered="${encodeURIComponent(code)}"></span>`;
    }),
  },
}));

import ChatMarkdown from './ChatMarkdown.svelte';

describe('<ChatMarkdown> multi-line display math (space-opened)', () => {
  it('tokenises and typesets `$$ \\begin{pmatrix}...\\end{pmatrix}\n$$` instead of falling back to paragraph text', async () => {
    katexCalls.length = 0;
    // The exact shape an LLM emits for matrix multiplication. Opens with
    // `$$ ` (space, not newline), spans multiple lines, closes with `$$`
    // on its own line.
    const source =
      '$$ \\begin{pmatrix} a & b \\\\ c & d \\end{pmatrix} \\begin{pmatrix} x \\\\ y\n' +
      '\\end{pmatrix} = \\begin{pmatrix}\n' +
      'ax + by \\\\\n' +
      'cx + dy\n' +
      '\\end{pmatrix}\n' +
      '$$';

    const r = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => {
      const rendered = r.container.querySelectorAll('span.katex');
      expect(rendered.length).toBe(1);
    });
    expect(katexCalls.length).toBe(1);
    // Content is the math body minus surrounding `$$` and the trim
    // applied by the marked-math tokenizer.
    expect(katexCalls[0]).toContain('\\begin{pmatrix}');
    expect(katexCalls[0]).toContain('cx + dy');
    expect(katexCalls[0]).not.toContain('$$');
    r.unmount();
  });

  it('tokenises matrix-style content that spans multiple lines without leading-character collisions', async () => {
    // Variant of the matrix case but smaller. Content uses `\\` line
    // continuations so the second line does not start with a marked
    // block-prefix character (`+`, `-`, `*`, `#`, `>`), which would
    // otherwise reach marked's list / heading / blockquote lexers
    // before the math tokenizer sees the paragraph.
    katexCalls.length = 0;
    const source = '$$f(x) = ax^2 \\\\\nbx + c\n$$';

    const r = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => {
      expect(r.container.querySelectorAll('span.katex').length).toBe(1);
    });
    expect(katexCalls.length).toBe(1);
    expect(katexCalls[0]).toContain('f(x) = ax^2');
    expect(katexCalls[0]).toContain('bx + c');
    r.unmount();
  });

  it('keeps canonical `$$\\nE = mc^2\\n$$` matching the original newline form', async () => {
    katexCalls.length = 0;
    const source = '$$\nE = mc^{single-canonical}\n$$\n';

    const r = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => {
      expect(r.container.querySelectorAll('span.katex').length).toBe(1);
    });
    expect(katexCalls.length).toBe(1);
    expect(katexCalls[0]).toBe('E = mc^{single-canonical}');
    r.unmount();
  });

  it('keeps single-line `$$X$$` followed by inline text matching the no-newline alternative (no over-match)', async () => {
    katexCalls.length = 0;
    // Source where the single-line `$$x^2$$` should not slurp up the
    // trailing prose. Markdown collapses the inline text into the same
    // paragraph as the math; we just need exactly one katex call with
    // content `x^2` and the trailing prose preserved as DOM text.
    const source = '$$x_{nooverlap}^2$$ then prose continues here.';

    const r = render(ChatMarkdown, { props: { source, pathRefs: [] } });
    await waitFor(() => {
      expect(r.container.querySelectorAll('span.katex').length).toBe(1);
    });
    expect(katexCalls.length).toBe(1);
    expect(katexCalls[0]).toBe('x_{nooverlap}^2');
    expect(r.container.textContent).toContain('then prose continues here.');
    r.unmount();
  });
});
