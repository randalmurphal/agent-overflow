import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';

// Structural coverage for the streaming committed-prefix / volatile-tail
// spacing seam. ChatMarkdown renders the committed prefix and the volatile
// tail as two separate Streamdown containers. Block margins collapse across
// the seam for every transition EXCEPT paragraph→paragraph — a paragraph is
// `margin: 0` and its gap comes entirely from `.markdown-body p + p`, which
// can't match across the container boundary. So app.css restores the gap
// ONLY for the true p→p seam, via
// `.md-committed:has(> p:last-child) + .md-volatile:has(> p:first-child)`.
// Non-paragraph seams get NO seam margin — the first tail block's intrinsic
// margin collapses to the correct gap on its own. (A blanket seam margin —
// the prior approach — over-spaced those by +9–17px while split, snapping
// down on commit: the visible "## / bullets jump" the fix removes.)
//
// happy-dom does not compute layout/margins, so these tests guard the CSS
// HOOK — that the marker classes AND the per-block element shape the
// `:has()` selector keys on are present (committed immediately followed by
// volatile; p-vs-non-p first/last children). The actual pixel gap is
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
    // ...AND both `:has()` legs satisfied for a p→p seam: committed ends with
    // a <p>, volatile starts with a <p>. This is the one transition whose gap
    // the seam rule must restore (the others collapse correctly on their own).
    expect(committedEl.lastElementChild?.tagName).toBe('P');
    expect(volatileEl.firstElementChild?.tagName).toBe('P');
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
    // The tail's first block is a heading, so `.md-volatile:has(> p:first-child)`
    // does NOT match — the seam adds nothing and the heading's own top margin
    // sets the gap (collapsing across the plain wrapper to the settled value).
    expect(volatileEl.firstElementChild?.tagName).toBe('H2');
    expect(volatileEl.firstElementChild?.tagName).not.toBe('P');
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
    // Same as the heading case: a <ul> first child fails the p→p `:has()` gate,
    // so the list's intrinsic margin owns the gap rather than the p+p seam.
    expect(volatileEl.firstElementChild?.tagName).toBe('UL');
    expect(volatileEl.firstElementChild?.tagName).not.toBe('P');
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
  });
});
