import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';

// Structural coverage for the streaming committed-prefix / volatile-tail
// spacing seam. ChatMarkdown renders the committed prefix and the volatile
// tail as two separate Streamdown containers. Block margins collapse across
// the seam for every transition EXCEPT paragraph→paragraph — a paragraph is
// `margin: 0` and its gap comes entirely from `.markdown-body p + p`, which
// can't match across the container boundary. So app.css restores the gap
// ONLY for the true p→p seam. Streamdown publishes the committed root's final
// rendered type; the volatile root exposes its actual first child. The selector
// therefore does not need `:has()`.
// Non-paragraph seams get NO seam margin — the first tail block's intrinsic
// margin collapses to the correct gap on its own. (A blanket seam margin —
// the prior approach — over-spaced those by +9–17px while split, snapping
// down on commit: the visible "## / bullets jump" the fix removes.)
//
// happy-dom does not compute layout/margins, so these tests guard the CSS
// HOOK — that the marker classes, committed block-type attribute, and per-block
// element shape are present (committed immediately followed by volatile;
// p-vs-non-p first/last children). The actual pixel gap is
// verified in the running app (and was measured per-transition in a
// headless-Chromium spike against this exact DOM + these app.css rules).
describe('<ChatMarkdown> streaming boundary spacing seam', () => {
  it('marks the committed prefix and the volatile tail as adjacent siblings', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'Paragraph one.\n\nParagraph two starting…',
        streaming: true,
      },
    });
    const body = container.querySelector('.markdown-body');
    expect(body).not.toBeNull();

    await waitFor(() => {
      expect(body!.querySelector('.md-committed')).not.toBeNull();
      expect(body!.querySelector('.md-volatile')).not.toBeNull();
    });

    const committedEl = body!.querySelector('.md-committed')!;
    const volatileEl = body!.querySelector('.md-volatile')!;
    // The seam rule needs the volatile tail to be the immediately-following
    // sibling of the committed prefix...
    expect(committedEl.nextElementSibling).toBe(volatileEl);
    expect(committedEl.textContent).toContain('Paragraph one');
    expect(volatileEl.textContent).toContain('Paragraph two');
    // The parser metadata and rendered DOM must agree on both sides of the
    // p→p seam. This is the one transition whose gap the seam rule restores.
    expect(committedEl.getAttribute('data-streamdown-last-block')).toBe('paragraph');
    expect(committedEl.lastElementChild?.tagName).toBe('P');
    expect(volatileEl.firstElementChild?.tagName).toBe('P');
    expect(committedEl.firstElementChild).toHaveClass('sd-first-block');
    expect(volatileEl.firstElementChild).toHaveClass('sd-first-block');
    expect(committedEl.lastElementChild).not.toHaveClass('sd-volatile-paragraph');
    expect(volatileEl.firstElementChild).toHaveClass('sd-volatile-paragraph');
    expect(volatileEl.firstElementChild).not.toHaveClass('sd-paragraph-gap');
    expect(committedEl.firstElementChild).toHaveClass('sd-trim-first-block');
    expect(committedEl.lastElementChild).not.toHaveClass('sd-trim-last-block');
    expect(volatileEl.firstElementChild).not.toHaveClass('sd-trim-first-block');
    expect(volatileEl.lastElementChild).toHaveClass('sd-trim-last-block');
  });

  it('leaves a heading-led tail without a <p> first child (no seam margin stacked on a heading)', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: 'Paragraph one.\n\n## A heading being typed', streaming: true },
    });
    const body = container.querySelector('.markdown-body')!;

    await waitFor(() => {
      expect(body.querySelector('.md-committed')).not.toBeNull();
      expect(body.querySelector('.md-volatile')).not.toBeNull();
    });

    const volatileEl = body.querySelector('.md-volatile')!;
    // The tail's first block is a heading, so the paragraph attribute selector
    // does NOT match. The seam adds nothing and the heading's own top margin
    // sets the gap (collapsing across the plain wrapper to the settled value).
    expect(volatileEl.firstElementChild?.tagName).toBe('H2');
    expect(volatileEl.firstElementChild?.tagName).not.toBe('P');
    expect(volatileEl.firstElementChild).toHaveClass('sd-first-block');
  });

  it('leaves a list-led tail without a <p> first child (no seam margin stacked on a list)', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: 'Paragraph one.\n\n- bullet one\n- bullet tw', streaming: true },
    });
    const body = container.querySelector('.markdown-body')!;

    await waitFor(() => {
      expect(body.querySelector('.md-committed')).not.toBeNull();
      expect(body.querySelector('.md-volatile')).not.toBeNull();
    });

    const volatileEl = body.querySelector('.md-volatile')!;
    // Same as the heading case: the list type fails the p→p attribute gate,
    // so the list's intrinsic margin owns the gap rather than the p+p seam.
    expect(volatileEl.firstElementChild?.tagName).toBe('UL');
    expect(volatileEl.firstElementChild?.tagName).not.toBe('P');
    expect(volatileEl.firstElementChild).toHaveClass('sd-first-block');
  });

  it('publishes the rendered extension type rather than the boundary token type', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '$$ x + y $$\n\nA paragraph still growing',
        streaming: true,
      },
    });
    const body = container.querySelector('.markdown-body')!;

    await waitFor(() => {
      expect(body.querySelector('.md-committed')).not.toBeNull();
      expect(body.querySelector('.md-volatile')).not.toBeNull();
    });

    const committed = body.querySelector('.md-committed')!;
    expect(committed.getAttribute('data-streamdown-last-block')).toBe('math');
    expect(committed.lastElementChild?.tagName).not.toBe('P');
    expect(body.querySelector('.md-volatile')?.firstElementChild?.tagName).toBe('P');
  });

  it('moves a closed list out of the volatile tail before the next block grows', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '- first item\n- second item\n\n| Partial table | still growing',
        streaming: true,
      },
    });
    const body = container.querySelector('.markdown-body')!;

    await waitFor(() => {
      expect(body.querySelector('.md-committed')).not.toBeNull();
      expect(body.querySelector('.md-volatile')).not.toBeNull();
    });

    const committed = body.querySelector('.md-committed')!;
    const volatile = body.querySelector('.md-volatile')!;
    expect(volatile.firstElementChild).toHaveClass('sd-first-block');
    expect(committed.querySelector('ul')?.textContent).toContain('first item');
    expect(committed.textContent).toContain('second item');
    expect(committed.textContent).not.toContain('Partial table');
    expect(volatile.textContent).toContain('Partial table');
    expect(volatile.textContent).not.toContain('first item');
  });

  it('renders a single committed container with no volatile seam once settled', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: 'Paragraph one.\n\nParagraph two.', streaming: false },
    });
    const body = container.querySelector('.markdown-body')!;

    await waitFor(() => {
      expect(body.querySelector('.md-committed')).not.toBeNull();
    });
    // Settled: one container, no volatile tail → the seam rule can't match,
    // so settled messages keep the plain `p + p` spacing.
    expect(body.querySelector('.md-volatile')).toBeNull();
    const committed = body.querySelector('.md-committed')!;
    expect(committed.firstElementChild).toHaveClass('sd-trim-first-block');
    expect(committed.lastElementChild).toHaveClass('sd-trim-last-block');
    const paragraphs = committed.querySelectorAll('p');
    expect(paragraphs).toHaveLength(2);
    expect(paragraphs[0]).not.toHaveClass('sd-paragraph-gap');
    expect(paragraphs[1]).toHaveClass('sd-paragraph-gap');
  });

  it('does not mark a committed prefix before the first block commits', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: 'Just one unfinished paragraph', streaming: true },
    });
    const body = container.querySelector('.markdown-body')!;

    await waitFor(() => {
      expect(body.querySelector('.md-volatile')).not.toBeNull();
    });
    // No committed prefix yet → only the volatile tail. The seam rule
    // requires a preceding .md-committed, so it can't add a stray top gap to
    // the very first streamed block.
    expect(body.querySelector('.md-committed')).toBeNull();
    const volatile = body.querySelector('.md-volatile')!;
    expect(volatile.firstElementChild).toHaveClass('sd-trim-first-block');
    expect(volatile.lastElementChild).toHaveClass('sd-trim-last-block');
  });
});
