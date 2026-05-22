import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { makeItem } from '../../../test/helpers/chat';
import AssistantMessage from './AssistantMessage.svelte';

describe('<AssistantMessage>', () => {
  it('renders an unterminated inline marker as literal text during stream', async () => {
    // `patches/svelte-streamdown@3.0.1.patch` strips Streamdown's
    // inline-emphasis plugins from `IncompleteMarkdownParser.createDefaultPlugins`,
    // so unterminated `**`, `~~`, backtick, `_`, etc. are no longer
    // speculatively closed mid-stream. The user sees literal `**markdown`
    // until the real closer arrives, then one clean transition to a bold
    // span — the alternative (speculative close → unbalanced regrowth on
    // the next chunk) produced visible "spreading" jitter on every
    // assistant response containing inline markdown.
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'streaming',
          summary: 'streaming **markdown',
        }),
      },
    });

    const body = getByTestId('assistant-message-body');
    expect(body.getAttribute('data-render-mode')).toBe('client-markdown');
    await waitFor(() => {
      expect(body.textContent).toContain('streaming **markdown');
      expect(body.querySelector('strong')).toBeNull();
    });
  });

  it('promotes the dangling marker to a strong element once the closer arrives', async () => {
    const { getByTestId, rerender } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'streaming',
          summary: 'streaming **markdown',
        }),
      },
    });

    const originalBody = getByTestId('assistant-message-body');

    await rerender({
      item: makeItem({
        status: 'completed',
        summary: 'streaming **markdown**',
      }),
    });

    const updatedBody = getByTestId('assistant-message-body');
    expect(updatedBody).toBe(originalBody);
    expect(updatedBody.getAttribute('data-render-mode')).toBe('client-markdown');
    await waitFor(() => {
      // Streamdown emits `<strong data-streamdown-strong=... class=...>`
      // — anchor on the wrapping element + its text rather than an
      // exact-string innerHTML match.
      const strong = updatedBody.querySelector('strong');
      expect(strong).not.toBeNull();
      expect(strong?.textContent).toBe('markdown');
    });
  });

  // Regression: svelte-streamdown's `parseIncompleteMarkdown` used to
  // count single italic/code delimiters across the WHOLE line — including
  // contents of inline code spans — and balance them with a trailing
  // copy at end of paragraph (e.g. `` `_CLAIM_SQL` `` made the next
  // sentence end with `there._`). The 2026-05 patch revision removes
  // Streamdown's inline-emphasis plugins entirely
  // (boldItalic / bold / strikethrough / singleAsteriskItalic /
  // inlineCode / singleUnderscoreItalic / subscript / superscript /
  // inlineMath — see `patches/svelte-streamdown@*.patch` `.filter`
  // at the end of `createDefaultPlugins`), so there is nothing left
  // to balance. The two `status` values still cover both gating paths:
  // `'completed'` flips `parseIncompleteMarkdown` off entirely (the
  // `Block.svelte` short-circuit from the original patch), and
  // `'streaming'` runs the remaining (block-level) plugins. Either path
  // regressing — a future Streamdown update putting the inline plugins
  // back, or the filter losing a name — would resurface the stray
  // delimiter.
  const inlineCodeBalanceCases = [
    {
      name: 'underscore inside inline code',
      delimiter: '_',
      summary: 'a `_CLAIM_SQL` partitions by it. The plumbing is 70% there.',
    },
    {
      name: 'asterisk inside inline code',
      delimiter: '*',
      summary: 'use `*ptr = NULL;` to clear it. The plumbing is 70% there.',
    },
  ] as const;

  for (const { name, delimiter, summary } of inlineCodeBalanceCases) {
    it(`does not append a stray '${delimiter}' after ${name} on a settled message`, async () => {
      const { getByTestId } = render(AssistantMessage, {
        props: { item: makeItem({ status: 'completed', summary }) },
      });
      const body = getByTestId('assistant-message-body');
      await waitFor(() => {
        const text = body.textContent?.trimEnd() ?? '';
        expect(text).toContain('70% there.');
        expect(text.endsWith(delimiter)).toBe(false);
      });
    });

    it(`does not append a stray '${delimiter}' after ${name} mid-stream`, async () => {
      const { getByTestId } = render(AssistantMessage, {
        props: { item: makeItem({ status: 'streaming', summary }) },
      });
      const body = getByTestId('assistant-message-body');
      await waitFor(() => {
        const text = body.textContent?.trimEnd() ?? '';
        expect(text).toContain('70% there.');
        expect(text.endsWith(delimiter)).toBe(false);
      });
    });
  }

  it('still renders real italic markdown adjacent to inline code', async () => {
    // Positive control: a future patch that over-skipped (e.g. treated all
    // `_` as inside-code) would silently break italics. Anchor real italic
    // outside the span to keep the inline-code-skip honest.
    const summary = 'see `foo_bar` and the _real_ italic.';
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const em = body.querySelector('em');
      expect(em).not.toBeNull();
      expect(em?.textContent).toBe('real');
    });
  });

  it('still renders ~~double-tilde~~ as legitimate strikethrough', async () => {
    // Positive control: the streamdown filter patch only removes
    // INLINE-EMPHASIS plugins from `parseIncompleteMarkdown`'s
    // speculative-close list — it does NOT modify marked's GFM `del`
    // tokenizer. A real `~~struck~~` pair must still render as `<del>`
    // through Streamdown's normal parse path. If a future patch
    // revision accidentally disables GFM strikethrough at the marked
    // level, this test catches it.
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary: 'this is ~~struck out~~ now.',
        }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const del = body.querySelector('del');
      expect(del).not.toBeNull();
      expect(del?.textContent).toBe('struck out');
    });
  });

  // Regression for the "runaway-spread" streaming jitter: Streamdown's
  // inline-emphasis plugins used to speculatively close unmatched
  // delimiters at end-of-line. On the next chunk the synthesized closer
  // disappeared and the formatted span "grew" to absorb the new text,
  // producing a visible spreading effect for every `**bold`, `` `code` ``,
  // `~~strike`, `_italic`, `*italic` token while the model was still
  // writing — strikethrough across unrelated prose was the most visible
  // symptom (a `~~partial` mid-stream synthesised `~~partial~~` →
  // `<del>partial</del>`, and the closer migrated outward on each chunk).
  // The `IncompleteMarkdownParser.createDefaultPlugins` `.filter` we
  // ship in `patches/svelte-streamdown@3.0.1.patch` drops those plugins,
  // so partial tokens stay literal until the real closer arrives. Each
  // delimiter that was previously in the filtered plugin list gets an
  // independent regression case: a future Streamdown update putting any
  // single plugin back surfaces under exactly one test.
  it.each([
    { name: 'unmatched bold', delimiter: '**', dom: 'strong' as const },
    { name: 'unmatched inline code', delimiter: '`', dom: 'code' as const },
    { name: 'unmatched strikethrough', delimiter: '~~', dom: 'del' as const },
    { name: 'unmatched underscore italic', delimiter: '_', dom: 'em' as const },
    { name: 'unmatched asterisk italic', delimiter: '*', dom: 'em' as const },
  ])('does not synthesise a $name closer mid-stream', async ({ delimiter, dom }) => {
    const partial = `mid-stream ${delimiter}this is a longer phrase that should not auto-close`;
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary: partial }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain(partial);
      expect(body.querySelector(dom)).toBeNull();
    });
  });

  // Progressive-streaming regression: the user-visible "spreading"
  // symptom required watching DOM stability across multiple incremental
  // rerenders, not just a one-shot atomic mount. Without the filter the
  // synthesised closer used to migrate outward on every chunk (e.g.
  // `~~partial` → `<del>partial</del>` → `<del>partial w</del>` →
  // `<del>partial wo</del>`...) producing the visible jitter. With the
  // filter the row contains the raw delimiter as plain text at EVERY
  // intermediate state and only collapses into the styled element on
  // the closing chunk.
  it.each([
    { name: 'bold', delimiter: '**', dom: 'strong' as const },
    { name: 'inline code', delimiter: '`', dom: 'code' as const },
    { name: 'strikethrough', delimiter: '~~', dom: 'del' as const },
    { name: 'underscore italic', delimiter: '_', dom: 'em' as const },
    { name: 'asterisk italic', delimiter: '*', dom: 'em' as const },
  ])(
    'keeps $name partial literal across progressive stream chunks until closer arrives',
    async ({ delimiter, dom }) => {
      const opener = `progressive ${delimiter}`;
      const inner = 'word';
      const closer = delimiter;
      const after = ' tail';
      const closingSummary = `${opener}${inner}${closer}${after}`;

      const { getByTestId, rerender } = render(AssistantMessage, {
        props: { item: makeItem({ status: 'streaming', summary: opener }) },
      });
      const body = getByTestId('assistant-message-body');
      await waitFor(() => {
        expect(body.textContent).toContain(opener);
        expect(body.querySelector(dom)).toBeNull();
      });

      // Stream the inner text one character at a time; before the
      // closer arrives the unmatched delimiter must NEVER produce
      // the styled element.
      for (let i = 1; i <= inner.length; i++) {
        const partial = `${opener}${inner.slice(0, i)}`;
        await rerender({ item: makeItem({ status: 'streaming', summary: partial }) });
        await waitFor(() => {
          expect(body.textContent).toContain(partial);
          expect(body.querySelector(dom)).toBeNull();
        });
      }

      // Closer arrives — the styled element should now exist with the
      // correct inner text only, NOT some grown-out runaway span.
      await rerender({ item: makeItem({ status: 'streaming', summary: closingSummary }) });
      await waitFor(() => {
        const el = body.querySelector(dom);
        expect(el).not.toBeNull();
        expect(el?.textContent).toBe(inner);
      });
    },
  );

  it('keeps inline command flags visually atomic', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary: 'run `--validate` after bootstrap',
        }),
      },
    });

    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const code = body.querySelector('code[data-streamdown-codespan]');
      expect(code).not.toBeNull();
      expect(code?.textContent).toBe('--validate');
      expect(code).toHaveClass(
        'inline-block',
        'max-w-full',
        'overflow-x-auto',
        'whitespace-nowrap',
        'align-middle',
        'leading-[1.35]',
      );
    });
  });

  it.each([
    {
      name: 'ordered',
      summary: '1. First predicate\n2. Second predicate',
      selector: 'ol[data-streamdown-ol]',
      expectedClasses: ['ml-0', 'pl-5', 'list-outside', 'whitespace-normal'],
    },
    {
      name: 'unordered',
      summary: '- First predicate\n- Second predicate',
      selector: 'ul[data-streamdown-ul]',
      expectedClasses: ['ml-0', 'pl-5', 'list-outside', 'list-disc', 'whitespace-normal'],
    },
  ])('keeps $name list markers inside the markdown clipping box', async ({ summary, selector, expectedClasses }) => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary,
        }),
      },
    });

    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const list = body.querySelector(selector);
      expect(list).not.toBeNull();
      expect(list).toHaveClass(...expectedClasses);
      expect(list).not.toHaveClass('ml-4');
    });
  });

  it('renders blank-line markdown as adjacent paragraph elements', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary: 'first paragraph\n\nsecond paragraph',
        }),
      },
    });

    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      // svelte-streamdown emits each markdown block via marked tokens
      // → a stable `[data-streamdown-paragraph]` element. We anchor on
      // that attribute instead of a positional `.markdown-body > p`
      // selector because Streamdown wraps its output in its own div.
      const paragraphs = [...body.querySelectorAll('p[data-streamdown-paragraph]')];
      expect(paragraphs.map((node) => node.textContent?.trim())).toEqual([
        'first paragraph',
        'second paragraph',
      ]);
    });
  });

  it('shows its timestamp without requiring row hover', () => {
    const createdAt = Date.UTC(2026, 0, 2, 15, 4);
    const { container } = render(AssistantMessage, {
      props: {
        item: makeItem({
          createdAt,
          summary: 'done',
        }),
      },
    });

    const time = container.querySelector('time');
    expect(time).not.toBeNull();
    expect(time?.getAttribute('datetime')).toBe(new Date(createdAt).toISOString());
    expect(time?.className).not.toContain('opacity-0');
    expect(time?.className).not.toContain('group-hover:opacity-100');
  });

  it('renders a copy button on a settled message with text', () => {
    const { getByLabelText } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary: 'done' }) },
    });
    expect(getByLabelText('Copy message')).toBeInTheDocument();
  });

  it('hides the copy button while the message is streaming', () => {
    const { container } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary: 'streaming text' }) },
    });
    expect(container.querySelector('[aria-label="Copy message"]')).toBeNull();
  });

  it('hides the copy button when summary is whitespace-only', () => {
    const { container } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary: '   \n  ' }) },
    });
    expect(container.querySelector('[aria-label="Copy message"]')).toBeNull();
  });

  it('writes the raw summary to the clipboard on click', async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });
    const summary = '## Heading\n\n```ts\nconst x = 1;\n```';
    const { getByLabelText } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary }) },
    });
    await fireEvent.click(getByLabelText('Copy message'));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(summary));
  });
});
