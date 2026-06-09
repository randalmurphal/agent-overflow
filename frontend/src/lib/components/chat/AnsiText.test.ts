import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import AnsiText from './AnsiText.svelte';

describe('<AnsiText>', () => {
  // Regression guard for the "expanded subagent/collab output does not
  // word-wrap" bug. The defect was that AnsiText handed Idiomorph a parentless
  // <pre> element:
  //
  //   const next = document.createElement('pre');
  //   next.innerHTML = html;
  //   Idiomorph.morph(root, next, { morphStyle: 'innerHTML' });
  //
  // idiomorph@0.7.4's normalizeParent wraps a parentless single node in a dummy
  // <div>, making `next` itself a CHILD — so the innerHTML morph nested a
  // SECOND, class-less <pre> inside AnsiText's root <pre>:
  //
  //   <pre class="ansi-body whitespace-pre-wrap break-all"><pre>TEXT</pre></pre>
  //
  // The inner <pre> carried none of the wrap classes and computed the UA default
  // `white-space: pre`, so the text never wrapped — verified in chromium
  // (/tmp/wrapspike/spike3.html): the nested <pre> overflowed its 623px container
  // by 19px and was clipped by ExpandablePayloadBody's `overflow-x-hidden`. The
  // fix hands idiomorph `next`'s childNodes, so root's only <pre> is the classed
  // root and the wrap classes apply to the text (chromium: height 17px → 33px,
  // wraps).
  //
  // happy-dom reports zero geometry, so this asserts the STRUCTURE the fix
  // produces (exactly one <pre>, classed, holding the text directly) rather than
  // the wrap pixels — those stay covered by the chromium spike.
  it(
    'renders one classed <pre> holding the text directly — no nested class-less <pre>',
    async () => {
      const longPath =
        'Recommended | src/ai_foundations/db/migrations/versions/platform/0069_graph_outbox_claim_index.py';
      const { container } = render(AnsiText, {
        props: { source: longPath, class: 'whitespace-pre-wrap break-all' },
      });

      await waitFor(() => {
        expect(container.textContent).toContain('0069_graph_outbox_claim_index.py');
      });

      const pres = container.querySelectorAll('pre');
      // Exactly one <pre>: the classed root. No nested wrapper <pre>.
      expect(pres.length).toBe(1);
      const pre = pres[0];
      // It carries the wrap classes…
      expect(pre.className).toContain('whitespace-pre-wrap');
      expect(pre.className).toContain('break-all');
      // …AND it is the element that directly holds the text, so the wrap intent
      // applies to the laid-out text.
      expect(pre.querySelector('pre')).toBeNull();
      expect(pre.textContent).toContain('0069_graph_outbox_claim_index.py');
    },
  );

  // The whole reason AnsiText routes through Idiomorph: a streaming update must
  // patch the existing text node IN PLACE, not replace root's children
  // wholesale — wholesale replacement is what drops a user's mid-stream text
  // selection. This pins that behavior across a source update, and re-checks
  // that a second morph does not reintroduce a nested <pre> (the wrap bug).
  it('morphs in place across source updates so a mid-stream selection survives', async () => {
    const { container, rerender } = render(AnsiText, {
      props: { source: 'abcdef', class: 'whitespace-pre-wrap break-all' },
    });

    await waitFor(() => {
      expect(container.textContent).toContain('abcdef');
    });

    const preBefore = container.querySelector('pre');
    expect(preBefore).not.toBeNull();
    const textNodeBefore = preBefore!.firstChild;
    expect(textNodeBefore?.nodeType).toBe(Node.TEXT_NODE);

    // Superset update — the streaming-append case.
    await rerender({ source: 'abcdefghi', class: 'whitespace-pre-wrap break-all' });
    await waitFor(() => {
      expect(container.textContent).toContain('abcdefghi');
    });

    // Still exactly one <pre>: no nested host reintroduced on the later morph.
    expect(container.querySelectorAll('pre')).toHaveLength(1);
    // The SAME text node object was reused (Idiomorph patched its content in
    // place) — which is what preserves an active selection across a chunk.
    const textNodeAfter = container.querySelector('pre')!.firstChild;
    expect(textNodeAfter).toBe(textNodeBefore);
    expect(textNodeAfter?.textContent).toBe('abcdefghi');
  });

  // renderAnsi strips OSC (`ESC ]` … BEL/ST) and APC (`ESC _` … ST) control
  // strings before rendering. These pin that the visible tail survives and the
  // control payload is removed, across both terminators.
  it.each([
    ['OSC title (BEL-terminated)', '\x1b]0;window title\x07visible', 'visible', 'window title'],
    [
      'OSC-8 hyperlink (open + close)',
      '\x1b]8;;https://example.com\x07link text\x1b]8;;\x07',
      'link text',
      'example.com',
    ],
    ['APC (ST-terminated)', '\x1b_Gpayload-bytes\x1b\\shown', 'shown', 'payload-bytes'],
  ])('strips %s, keeping the visible text', async (_label, source, kept, stripped) => {
    const { container } = render(AnsiText, { props: { source } });
    await waitFor(() => {
      expect(container.textContent).toContain(kept);
    });
    expect(container.textContent).not.toContain(stripped);
  });

  // ReDoS regression guard. The old strip regexes used a lazy `[\s\S]*?` against
  // an alternation terminator; on many unterminated `ESC ]` starts that
  // backtracks O(n²) — each start rescans to EOF. Measured: the old form takes
  // ~7.7s at 140k starts (30k=0.3s, 60k=1.4s, 120k=5.7s — clean quadratic), so
  // it blows the 4s timeout below; the negated-class form is linear and renders
  // the trailing sentinel in well under a second. The explicit timeout is the
  // fail-without signal — a reversion to lazy matching times out here.
  it(
    'handles many unterminated OSC starts without catastrophic backtracking',
    async () => {
      const source = '\x1b]'.repeat(140_000) + 'SENTINEL_TAIL';
      const { container } = render(AnsiText, { props: { source } });
      await waitFor(() => {
        expect(container.textContent).toContain('SENTINEL_TAIL');
      });
    },
    4000,
  );
});
