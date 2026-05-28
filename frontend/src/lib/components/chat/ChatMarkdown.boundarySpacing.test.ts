import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';

// Structural coverage for the streaming committed-prefix / volatile-tail
// spacing seam. ChatMarkdown renders the committed prefix and the volatile
// tail as two separate Streamdown containers, so the adjacent-sibling
// `.markdown-body p + p` rule can't match across them and the inter-paragraph
// gap collapses at the seam until the next block commits (the user-visible
// "spacing drops then reformats" while streaming). The fix re-adds the gap
// via `.markdown-body > .md-committed + .md-volatile` in app.css.
//
// happy-dom does not compute layout/margins, so these tests guard the CSS
// HOOK — that the marker classes are applied in the structure the selector
// targets (committed immediately followed by volatile). The actual visual
// gap is verified in the running app.
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
    // The seam rule is `.md-committed + .md-volatile`, so the volatile tail
    // must be the immediately-following sibling of the committed prefix.
    expect(committedEl.nextElementSibling).toBe(volatileEl);
    expect(committedEl.textContent).toContain('Paragraph one');
    expect(volatileEl.textContent).toContain('Paragraph two');
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
