import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { makeItem } from '../../../test/helpers/chat';
import { getSettings, resetSettingsForTest } from '../../stores/settings.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import AssistantMessage from './AssistantMessage.svelte';

describe('<AssistantMessage>', () => {
  it('renders an unterminated inline marker as literal text during stream', async () => {
    // `patches/svelte-streamdown@3.1.2.patch` strips Streamdown's
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

  // The fixture leads each section with `**bold**`, matching real
  // assistant prose (and the reported bug): the boundary detector must
  // commit a `**…**`-led paragraph so the committed prefix advances. If
  // isListItemStart regresses to treating a leading `*` as a bullet, the
  // prefix stays empty and the "committed section" assertion below fails.
  const SPLIT_FIXTURE =
    '**Committed section:** done body text.\n\n**Volatile section:** still in progress';

  it('withholds the volatile tail while streaming when streaming is disabled', async () => {
    // "Streaming enabled" = off: the in-progress (uncommitted) block is
    // held back and only committed markdown blocks render until the row
    // settles. This is the user-visible half of the setting; orthogonal
    // to low power, which only strips the reveal animation.
    getSettings().streamingEnabled = false;
    try {
      const { getByTestId } = render(AssistantMessage, {
        props: {
          item: makeItem({ status: 'streaming', summary: SPLIT_FIXTURE }),
        },
      });
      const body = getByTestId('assistant-message-body');
      await waitFor(() => {
        expect(body.textContent).toContain('done body text.');
      });
      expect(body.textContent).not.toContain('still in progress');
    } finally {
      resetSettingsForTest();
    }
  });

  it('streams the volatile tail while streaming when streaming is enabled', async () => {
    // Default (setting on): both the committed prefix and the live
    // in-progress block are visible as text arrives.
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({ status: 'streaming', summary: SPLIT_FIXTURE }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain('done body text.');
      expect(body.textContent).toContain('still in progress');
    });
  });

  it('holds streaming rendering through the post-completion drain (pane still smoothing)', async () => {
    // The wire settles status to 'completed' while the per-item smoother
    // is still draining the reveal. Rendered streaming mode must key off
    // `status === 'streaming' || pane.isItemSmoothing(id)` — flipping to
    // settled mode mid-drain drops the volatile-tail split (and its
    // incomplete-markdown guards) while text is still growing.
    const smoothingPane = {
      isItemSmoothing: () => true,
    } as unknown as ThreadPane;
    const { getByTestId } = render(AssistantMessage, {
      props: {
        pane: smoothingPane,
        item: makeItem({ status: 'completed', summary: SPLIT_FIXTURE }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain('done body text.');
    });
    // Still in streaming mode: the two-instance split is live.
    expect(body.querySelector('.md-volatile')).not.toBeNull();
  });

  it('renders settled once the drain finishes (pane no longer smoothing)', async () => {
    const settledPane = {
      isItemSmoothing: () => false,
    } as unknown as ThreadPane;
    const { getByTestId } = render(AssistantMessage, {
      props: {
        pane: settledPane,
        item: makeItem({ status: 'completed', summary: SPLIT_FIXTURE }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain('still in progress');
    });
    // Settled mode: single committed instance, no volatile tail.
    expect(body.querySelector('.md-volatile')).toBeNull();
    expect(body.querySelector('.md-committed')).not.toBeNull();
  });

  it('hides the timestamp/meta row until the first block commits (streaming disabled)', async () => {
    // Bug follow-up: with streaming disabled the body is withheld until a
    // block commits, but the meta row (timestamp + copy slot) used to
    // render unconditionally — so a lone timestamp floated over an empty
    // body from the instant streaming started. A single unterminated
    // paragraph has no committed prefix, so the whole row must be empty.
    getSettings().streamingEnabled = false;
    try {
      const { getByTestId, queryByTestId } = render(AssistantMessage, {
        props: {
          item: makeItem({
            status: 'streaming',
            summary: '**On the ports:** still typing this first section',
          }),
        },
      });
      // Nothing committed → no visible body text and no meta row.
      expect(getByTestId('assistant-message-body').textContent?.trim()).toBe('');
      expect(queryByTestId('assistant-message-meta')).toBeNull();
    } finally {
      resetSettingsForTest();
    }
  });

  it('shows the meta row once a block commits (streaming disabled)', async () => {
    getSettings().streamingEnabled = false;
    try {
      const { getByTestId } = render(AssistantMessage, {
        props: {
          item: makeItem({ status: 'streaming', summary: SPLIT_FIXTURE }),
        },
      });
      // A committed block is on screen → the meta row appears with it.
      await waitFor(() => {
        expect(getByTestId('assistant-message-meta')).toBeInTheDocument();
      });
    } finally {
      resetSettingsForTest();
    }
  });

  it('reveals the meta row when the first block commits mid-stream (streaming disabled)', async () => {
    getSettings().streamingEnabled = false;
    try {
      const item = makeItem({
        status: 'streaming',
        summary: '**On the ports:** still typing this first section',
      });
      const { queryByTestId, rerender } = render(AssistantMessage, {
        props: { item },
      });
      expect(queryByTestId('assistant-message-meta')).toBeNull();

      // The section terminates (blank line) and a second one begins — the
      // first block commits, so the meta row latches on.
      await rerender({
        item: {
          ...item,
          summary:
            '**On the ports:** first section done.\n\n**On the fans:** next section',
        },
      });
      await waitFor(() => {
        expect(queryByTestId('assistant-message-meta')).toBeInTheDocument();
      });
    } finally {
      resetSettingsForTest();
    }
  });

  it('shows the meta row immediately for normal streaming (setting on)', () => {
    // Default streaming still reserves the meta row (and copy slot) as
    // soon as any text arrives — this fix must not regress that.
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({ status: 'streaming', summary: 'live text arriving' }),
      },
    });
    expect(getByTestId('assistant-message-meta')).toBeInTheDocument();
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
    expect(updatedBody.getAttribute('data-render-mode')).toBe(
      'client-markdown',
    );
    await waitFor(() => {
      // Streamdown emits `<strong data-streamdown-strong=... class=...>`
      // — anchor on the wrapping element + its text rather than an
      // exact-string innerHTML match.
      const strong = updatedBody.querySelector('strong');
      expect(strong).not.toBeNull();
      expect(strong?.textContent).toBe('markdown');
    });
  });

  it('renders payload-linked assistant text as normal full output', async () => {
    const { container, getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary: 'normal assistant body',
          payloadId: 'assistant-text:thread-1:text:0:1',
          payloadKind: 'assistant_text',
        }),
      },
    });

    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain('normal assistant body');
    });
    expect(container.textContent).not.toContain('Show full message');
    expect(container.textContent).not.toContain('Show preview');
  });

  it('renders bracketed numeric references as literal text', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary: "So entry [32]'s main resume does mis-route it.",
        }),
      },
    });

    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain("[32]'s main resume");
      expect(body.querySelector('[data-streamdown-citation-preview]')).toBeNull();
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

  // Regression (Tier 1): marked's GFM inline `text` rule begins with the
  // alternation `[`~]+|[^`~]`, whose `[`~]+` branch is a COMBINED run of
  // backticks AND tildes — so a `~` fused to a backtick (no space) was
  // swallowed into a literal text run and the code span never opened:
  // `` ~`x` `` rendered as plain text. The patch
  // (`patches/svelte-streamdown@*.patch`, `_fixText` next to `_fixedDel`)
  // splits that leading class into homogeneous runs so a backtick is never
  // eaten as part of a tilde run. A `~` glued to a code span must now leave
  // a literal tilde and a real `<code>`. These Tier-1 cases also guard
  // against a future marked upgrade silently no-op'ing `_fixText`'s
  // source-`.replace` — if the override stops applying, they fail.
  it('renders a code span when a tilde is glued to the opening backtick', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary: 'see ~`config.value` here',
        }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const code = body.querySelector('code[data-streamdown-codespan]');
      expect(code).not.toBeNull();
      // The tilde is literal text OUTSIDE the span, not consumed into it:
      // the code holds exactly the value and the `~` still shows in the body.
      expect(code?.textContent).toBe('config.value');
      expect(code?.textContent).not.toContain('~');
      expect(body.textContent).toContain('~');
    });
  });

  // Regression for the exact reported symptom: `` ~`a` ... ~`b` `` produced a
  // STRAY code pill over the middle text and left the real content unstyled,
  // because the first `~`-glued backtick was eaten as literal text and the
  // remaining backticks mis-paired. Both code spans must render with their
  // correct contents and nothing spurious between them.
  it('renders both code spans (no stray mid-line pill) for two glued tilde-code pairs', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary: 'set ~`alpha` then ~`beta`',
        }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const codes = [
        ...body.querySelectorAll('code[data-streamdown-codespan]'),
      ];
      expect(codes.map((c) => c.textContent)).toEqual(['alpha', 'beta']);
    });
  });

  // Positive control: a tilde INSIDE a code span (home-dir paths are the
  // common case) must survive verbatim — the text-rule split must not
  // disturb code-span tokenization that starts at a backtick.
  it('preserves a tilde inside an inline code span', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({ status: 'completed', summary: 'run `cd ~/proj` now' }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const code = body.querySelector('code[data-streamdown-codespan]');
      expect(code).not.toBeNull();
      expect(code?.textContent).toBe('cd ~/proj');
    });
  });

  // Regression (Tier 2): the `markedSub` extension's `subRule` treated a
  // backtick as ordinary subscript content and ran BEFORE the code-span
  // tokenizer, so `` ~`~/etc` `` became a `<sub>` of a lone backtick
  // (garbage) instead of a code span. The patch excludes backticks from
  // sub/sup content so code spans win (CommonMark precedence).
  it('lets a code span win over subscript when a tilde precedes it', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({ status: 'completed', summary: '~`~/etc` config' }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const code = body.querySelector('code[data-streamdown-codespan]');
      expect(code).not.toBeNull();
      expect(code?.textContent).toBe('~/etc');
      // No subscript should have been synthesised around the code span.
      expect(body.querySelector('sub')).toBeNull();
    });
  });

  // Positive control for the Tier-2 narrowing: excluding backticks from
  // sub/sup content must NOT disable subscript on ordinary text. `H~2~O`
  // must still render `<sub>2</sub>`.
  it('still renders subscript on plain text after the sub-rule narrowing', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({ status: 'completed', summary: 'formula H~2~O here' }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const sub = body.querySelector('sub');
      expect(sub).not.toBeNull();
      expect(sub?.textContent).toBe('2');
    });
  });

  // Regression (Tier 3): the `subRule` digit-lookahead `~(?!\d)`. Agents write
  // approximate ranges like `~5~10` / `~50~100` (≈5–10); without the lookahead
  // the first `~N~` pair tokenized as `<sub>5</sub>`, eating the low bound into
  // a subscript and dropping the tildes. A closing `~` immediately before a
  // digit now blocks the match, so the range stays literal text. Legitimate
  // subscript (`H~2~O` — closing `~` before a letter) is unaffected, as the
  // positive case above asserts.
  it('does not subscript an approximate range like ~5~10', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({ status: 'completed', summary: 'the range ~5~10 is typical' }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      // Literal tildes survive (no sub ate them) AND no <sub> was synthesised.
      // Without the lookahead, `~5~10` → `<sub>5</sub>10`, so textContent would
      // read `…range 510 is typical` and this contains-check would fail.
      expect(body.textContent).toContain('~5~10');
      expect(body.querySelector('sub')).toBeNull();
    });
  });

  // Regression (Tier 2, superscript half): `supRule` had the same defect as
  // `subRule` — a backtick counted as superscript content and the rule ran
  // before the code-span tokenizer, so `` ^`x`^ `` became a `<sup>` wrapping
  // a raw backtick. The patch excludes backticks from `supRule` too, so the
  // code span wins. Without this case the `supRule` half of the change would
  // be entirely unguarded.
  it('lets a code span win over superscript when a caret wraps it', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({ status: 'completed', summary: 'see ^`config`^ ok' }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const code = body.querySelector('code[data-streamdown-codespan]');
      expect(code).not.toBeNull();
      expect(code?.textContent).toBe('config');
      // No superscript should have been synthesised around the code span.
      expect(body.querySelector('sup')).toBeNull();
    });
  });

  // Positive control mirroring `H~2~O`: the `supRule` narrowing must not
  // disable superscript on ordinary text — `mc^2^` still renders `<sup>2</sup>`.
  it('still renders superscript on plain text after the sup-rule narrowing', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({
          status: 'completed',
          summary: 'energy E=mc^2^ today',
        }),
      },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const sup = body.querySelector('sup');
      expect(sup).not.toBeNull();
      expect(sup?.textContent).toBe('2');
    });
  });

  // Regression: single-`$` inline math vs. agent prose. Agents emit
  // `$`-prefixed identifiers constantly — `$ref` (JSON Schema), `$PATH`
  // / `$HOME` (shell), jQuery `$el` — and inline code spans like
  // `` `$` `` / `` `$ref` ``. A bare `$` used to open a KaTeX inline-math
  // span that closed on the `$` of a *later* token, swallowing the prose
  // between them and rendering it in math mode (serif font, collapsed
  // whitespace — visibly corrupt; the reported `` `$ref` … `$ref` ``
  // paragraph). The `singleDollarLooksLikeProse` guard in
  // `patches/svelte-streamdown@3.1.2.patch` rejects a single-`$` span when
  // EITHER its closing `$` abuts an identifier char (it closed on a bare
  // `$ref`/`$PATH`) OR its captured content holds a backtick — the closing
  // `$` landed inside a `` `$` `` code span, so the content ran up to that
  // span's opening backtick. The backtick arm is load-bearing: in that case
  // the closer's *next* char is a backtick, not an identifier, which the
  // original word-char-only guard missed (the observed `ref cycles" closing
  // on the` serif blob). The load-bearing assertion is `.math-inline` being
  // null — that host span is emitted only when an inline-math token was
  // created. (textContent alone is not a discriminator: KaTeX is not
  // typeset in the test env, so a swallowed span's raw source text would
  // still appear.) The cases cover both guard sites: a closer mid-prose
  // is dropped by the `start()` scanner, while a span that *opens* the
  // text run (leading `$`) is dropped only by the `tokenizer()` site,
  // which marked calls at offset 0 before consulting `start()`.
  it.each([
    {
      name: 'a `$ref` chain with a backtick-flanked closer',
      summary:
        'the `$ref` cycle detector from this branch\'s "reject recursive $ref cycles" work — **doubly recursive**: self-calls on `$ref` targets',
      intact: 'doubly recursive',
    },
    {
      name: 'a span closing on a `$` inside a code span (backtick arm)',
      summary: 'recursive $ref cycles closing on the `$` sigil',
      intact: 'recursive $ref cycles closing on the',
    },
    {
      name: 'bare shell variables mid-sentence',
      summary: 'export $PATH and $HOME before the run',
      intact: 'export $PATH and $HOME before the run',
    },
    {
      name: 'a leading `$` opening the text run',
      summary: '$ref points to $HOME here',
      intact: '$ref points to $HOME here',
    },
  ])('does not swallow $name into an inline math span', async ({ summary, intact }) => {
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain(intact);
    });
    expect(body.querySelector('.math-inline')).toBeNull();
  });

  // Positive control: the guard must not disable real inline math. The
  // closing `$` of genuine math is followed by a space, punctuation, or
  // end-of-line — never an identifier char — so it survives. The
  // end-of-line row also covers the `charAt` boundary: past the string
  // end it returns '', which is not an identifier char. Without this,
  // dropping single-`$` math outright would pass the negative cases above
  // while silently regressing genuine math.
  it.each([
    { name: 'mid-sentence', summary: 'amortized $O(n^2)$ here', source: 'O(n^2)' },
    { name: 'at end of line', summary: 'the upper bound is $x$', source: 'x' },
  ])(
    'still renders real single-$ inline math ($name) as a math span',
    async ({ summary, source }) => {
      const { getByTestId } = render(AssistantMessage, {
        props: { item: makeItem({ status: 'completed', summary }) },
      });
      const body = getByTestId('assistant-message-body');
      await waitFor(() => {
        const math = body.querySelector('.math-inline');
        expect(math).not.toBeNull();
        expect(math?.getAttribute('data-math-source')).toBe(source);
      });
    },
  );

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
  // ship in `patches/svelte-streamdown@3.1.2.patch` drops those plugins,
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
  ])(
    'does not synthesise a $name closer mid-stream',
    async ({ delimiter, dom }) => {
      const partial = `mid-stream ${delimiter}this is a longer phrase that should not auto-close`;
      const { getByTestId } = render(AssistantMessage, {
        props: { item: makeItem({ status: 'streaming', summary: partial }) },
      });
      const body = getByTestId('assistant-message-body');
      await waitFor(() => {
        expect(body.textContent).toContain(partial);
        expect(body.querySelector(dom)).toBeNull();
      });
    },
  );

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
        await rerender({
          item: makeItem({ status: 'streaming', summary: partial }),
        });
        await waitFor(() => {
          expect(body.textContent).toContain(partial);
          expect(body.querySelector(dom)).toBeNull();
        });
      }

      // Closer arrives — the styled element should now exist with the
      // correct inner text only, NOT some grown-out runaway span.
      await rerender({
        item: makeItem({ status: 'streaming', summary: closingSummary }),
      });
      await waitFor(() => {
        const el = body.querySelector(dom);
        expect(el).not.toBeNull();
        expect(el?.textContent).toBe(inner);
      });
    },
  );

  // Regression: transient Setext-heading "balloon" during streaming.
  // CommonMark reads a non-blank line followed by a lone run of `-`/`=` as a
  // Setext heading. Mid-stream, a nested/tight bullet's marker arrives a chunk
  // before its text, so the volatile tail momentarily ends in
  // `…parent text:\n  -`. marked then promotes the parent line to <h2>
  // (larger font + heading margins + a re-wrap), and one chunk later — when the
  // bullet text streams in — it collapses back to a paragraph + nested <ul>.
  // That up-then-down relayout is the visible jank. The
  // `stripDanglingSetextUnderline` hunk in
  // `patches/svelte-streamdown@3.1.2.patch` drops a trailing lone `-`/`=` line
  // (when the line above is non-blank) from `parseIncompleteMarkdown`'s output,
  // so the underline never promotes the line above while it is the last thing
  // streamed. It runs only on the volatile tail; settled messages and the
  // committed prefix pass `parseIncompleteMarkdown === false` and are untouched.
  it('does not transiently promote a streamed line to a heading on a dangling bullet marker', async () => {
    // Volatile tail ends in the first nested marker before its text arrives.
    const summary = [
      '- first point here',
      '- second point splits into two:',
      '  -',
    ].join('\n');
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      // The parent line's text must still be present…
      expect(body.textContent).toContain('second point splits into two:');
      // …but NOT as a heading. Without the fix marked emits an <h2> here.
      expect(body.querySelector('h1, h2')).toBeNull();
    });
  });

  it('does not promote a top-level streamed line on a dangling dash', async () => {
    const summary = 'A sentence that ends with a colon:\n-';
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain('ends with a colon:');
      expect(body.querySelector('h1, h2')).toBeNull();
    });
  });

  // Pins the any-indent guard. The regex is `^[ \t]*[-=]+[ \t]*$` (any leading
  // whitespace), NOT `^ {0,3}…` — a `{0,3}` ceiling would miss a DOUBLE-nested
  // bullet whose marker lands at column 4, which marked still mis-promotes to
  // <h2> mid-stream. (Indented code can't be the false positive: it requires a
  // blank line above, which the blank-above guard rejects.) Reverting the regex
  // to `{0,3}` passes every ≤2-space case above but fails here.
  it('does not promote a double-nested 4-space dangling bullet marker', async () => {
    const summary = ['- a', '  - b', '    -'].join('\n');
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain('b');
      expect(body.querySelector('h1, h2')).toBeNull();
    });
  });

  // Pins the `=` branch. `=` is in the regex for symmetry with `-` (Setext H1),
  // and is the only heading-1 path; without `=` in the alternation a streamed
  // `Title\n===` balloons to <h1> exactly like the dash case.
  it('does not promote a top-level streamed line on a dangling = underline', async () => {
    const summary = 'Big Title in progress\n===';
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.textContent).toContain('Big Title in progress');
      expect(body.querySelector('h1, h2')).toBeNull();
    });
  });

  // Positive control 1: the fix only touches the streaming volatile tail. A
  // SETTLED Setext heading parses with `parseIncompleteMarkdown === false`, so
  // `Title\n---` must still render <h2>. Guards against the strip leaking onto
  // the authoritative (committed/settled) path and eating real headings.
  it('still renders a settled Setext heading as a real heading', async () => {
    const summary = 'Section Title\n---\n\nbody text follows here';
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const heading = body.querySelector('h2');
      expect(heading).not.toBeNull();
      expect(heading?.textContent).toBe('Section Title');
    });
  });

  // Positive control 2: once the nested bullet's text arrives the strip must NOT
  // eat it — a real nested list renders. The regex only matches a LONE `-`/`=`
  // run, so `  - nested child` is never stripped.
  it('renders the nested bullet as a list item once its text streams in', async () => {
    const summary = [
      '- first point here',
      '- second point splits into two:',
      '  - nested child item',
    ].join('\n');
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      // Assert the real structure, not just absence of a heading: the child
      // must land in an <li>. A future over-strip that ate the `- ` marker
      // would still leave the text present and no heading, slipping past a
      // text-only check — this catches it.
      const li = [...body.querySelectorAll('li')].find((el) =>
        el.textContent?.includes('nested child item'),
      );
      expect(li).not.toBeUndefined();
      expect(body.querySelector('h1, h2')).toBeNull();
    });
  });

  // Boundary guard for strip-after-parse ordering. The strip runs on
  // `defaultParser.parse(text)` OUTPUT, not the raw input, specifically so an
  // open code fence is sealed first — a `-` line inside the fence is then no
  // longer the trailing line and survives. If a future re-roll moved the strip
  // BEFORE the parse, the trailing dash of a streamed open fence would be eaten
  // out of the code. Asserts the dash stays in the code source.
  it('keeps a trailing dash inside a streamed open code fence', async () => {
    const summary = '```js\nconst x = 1\n-';
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const code = body.querySelector('[data-code-source]');
      expect(code).not.toBeNull();
      const src = code?.getAttribute('data-code-source') ?? '';
      expect(src).toContain('const x = 1');
      expect(src).toContain('-');
      expect(body.querySelector('h1, h2')).toBeNull();
    });
  });

  // Boundary guard for the single-line guard (`lastNewline < 0`). A lone
  // streamed `---` is a thematic break, not a Setext underline — there is no
  // line above it to promote. Without the guard the strip would slice off the
  // last char (`---` → `--`) and destroy the rule. Asserts the <hr> survives.
  // (The blank-above guard is NOT reachable at the component level — the
  // boundary splitter commits the blank line into the prefix, so a tail like
  // `text\n\n---` arrives as a single-line `---`; that branch is covered by the
  // parser-level battery, not here.)
  it('does not corrupt a lone streamed thematic break into a paragraph', async () => {
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary: '---' }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.querySelector('hr')).not.toBeNull();
      expect(body.querySelector('h1, h2')).toBeNull();
    });
  });

  // Regression battery for the fence-seal fidelity hunk in
  // `patches/svelte-streamdown@3.1.2.patch` (parse-incomplete-markdown.js,
  // contextManager). While a fence streams, the volatile tail is sealed with
  // an auto-appended closer. The original seal was always a flush-left ` ``` `,
  // which is NOT a closer for a fence opened with indentation (list item) or
  // a blockquote prefix — per CommonMark it terminates the enclosing container
  // and OPENS a new top-level fence instead. That phantom fence rendered as a
  // persistent empty code-block container under the streaming one, vanishing
  // with a layout snap when the real closer arrived. The seal now replicates
  // the opener's leading prefix, fence char, and run length, and drops a
  // trailing half-streamed closer (a bare ` or `` line) so the close moment
  // doesn't flicker.
  it('renders exactly one code block for a streaming list-indented fence (no phantom empty block)', async () => {
    const summary = [
      'Two steps:',
      '',
      '1. First install it:',
      '',
      '   ```bash',
      '   npm install foo',
      '   npm run build',
    ].join('\n');
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const hosts = body.querySelectorAll('[data-code-source]');
      // Without the prefix-replicating seal this is 2: the real block plus
      // an empty phantom opened by the flush-left ``` closer.
      expect(hosts.length).toBe(1);
      expect(hosts[0].getAttribute('data-code-source')).toContain('npm install foo');
    });
  });

  it('renders exactly one code block for a streaming blockquote-nested fence', async () => {
    const summary = ['> From the docs:', '>', '> ```js', '> const x = 1'].join('\n');
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const hosts = body.querySelectorAll('[data-code-source]');
      expect(hosts.length).toBe(1);
      expect(hosts[0].getAttribute('data-code-source')).toContain('const x = 1');
      // The block must live INSIDE the blockquote — a flush-left closer
      // would strand a phantom fence outside it.
      expect(hosts[0].closest('blockquote')).not.toBeNull();
    });
  });

  // Pins the fence-char half of the seal: a streaming ~~~ fence must be
  // sealed with ~~~, not ```. A ``` line appended inside an open ~~~ fence is
  // CONTENT — the block stayed unsealed and the stray ``` showed up as a code
  // line for a chunk.
  it('seals a streaming tilde fence without leaking a ``` line into the code', async () => {
    const summary = '~~~python\nprint(1)';
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const host = body.querySelector('[data-code-source]');
      expect(host).not.toBeNull();
      const src = host?.getAttribute('data-code-source') ?? '';
      expect(src).toContain('print(1)');
      expect(src).not.toContain('```');
    });
  });

  // Pins the run-length half of the close toggle: inside a 4-backtick fence a
  // bare ``` line is nested-example content, not a closer. The original
  // any-run toggle treated it as a close, desyncing the seal from marked.
  it('keeps a bare ``` line inside a streaming 4-backtick fence as content', async () => {
    const summary = '````md\nExample fence:\n```js\nconst x = 1';
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const hosts = body.querySelectorAll('[data-code-source]');
      expect(hosts.length).toBe(1);
      expect(hosts[0].getAttribute('data-code-source')).toContain('```js');
    });
  });

  // The half-streamed closer: the final ``` arrives a char at a time, and the
  // lone ` / `` momentarily lexes as a code content line — a one-chunk
  // grow-then-shrink flicker at every fence close. The seal now drops that
  // trailing partial run before appending the real closer.
  it('drops a half-streamed closing fence from the streamed code tail', async () => {
    const summary = '```js\nconst x = 1\n``';
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const host = body.querySelector('[data-code-source]');
      expect(host).not.toBeNull();
      const src = host?.getAttribute('data-code-source') ?? '';
      expect(src).toContain('const x = 1');
      expect(src).not.toContain('``');
    });
  });

  // Positive control: only the TRAILING partial run is dropped. A bare ``
  // line mid-code (not the last line) is real content and must survive.
  it('keeps a bare `` line inside streamed code when it is not the tail', async () => {
    const summary = '```js\nconst a = 1\n``\nconst b = 2';
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'streaming', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const src =
        body.querySelector('[data-code-source]')?.getAttribute('data-code-source') ?? '';
      expect(src).toContain('const a = 1');
      expect(src).toContain('``');
      expect(src).toContain('const b = 2');
    });
  });

  // Positive control: the seal only runs on the streaming volatile tail. A
  // settled list-indented block parses with parseIncompleteMarkdown === false
  // and must render one code block inside the list as before.
  it('renders a settled list-indented code block normally', async () => {
    const summary = [
      '1. First install it:',
      '',
      '   ```bash',
      '   npm install foo',
      '   ```',
      '',
      '2. Then run it.',
    ].join('\n');
    const { getByTestId } = render(AssistantMessage, {
      props: { item: makeItem({ status: 'completed', summary }) },
    });
    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      const hosts = body.querySelectorAll('[data-code-source]');
      expect(hosts.length).toBe(1);
      expect(hosts[0].getAttribute('data-code-source')).toContain('npm install foo');
      expect(hosts[0].closest('li')).not.toBeNull();
    });
  });

  it('wraps inline command flags instead of scrolling horizontally', async () => {
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
        'inline',
        'whitespace-pre-wrap',
        'wrap-anywhere',
        'align-baseline',
        'leading-[1.35]',
      );
      expect(code).not.toHaveClass('inline-block');
      expect(code).not.toHaveClass('overflow-x-auto');
      expect(code).not.toHaveClass('whitespace-nowrap');
      expect(code).not.toHaveClass('align-middle');
    });
  });

  it.each([
    {
      name: 'ordered',
      summary: '1. First predicate\n2. Second predicate',
      selector: 'ol[data-streamdown-ol]',
      expectedClasses: ['ml-0', 'pl-[2em]', 'list-outside', 'whitespace-normal'],
    },
    {
      name: 'unordered',
      summary: '- First predicate\n- Second predicate',
      selector: 'ul[data-streamdown-ul]',
      expectedClasses: [
        'ml-0',
        'pl-[2em]',
        'list-outside',
        'list-disc',
        'whitespace-normal',
      ],
    },
  ])(
    'keeps $name list markers inside the markdown clipping box',
    async ({ summary, selector, expectedClasses }) => {
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
    },
  );

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
      const paragraphs = [
        ...body.querySelectorAll('p[data-streamdown-paragraph]'),
      ];
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
    expect(time?.getAttribute('datetime')).toBe(
      new Date(createdAt).toISOString(),
    );
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
      props: {
        item: makeItem({ status: 'streaming', summary: 'streaming text' }),
      },
    });
    expect(container.querySelector('[aria-label="Copy message"]')).toBeNull();
  });

  it('reserves the copy button slot before completed content can be copied', async () => {
    const streamingItem = makeItem({
      status: 'streaming',
      summary: 'streaming text',
    });
    const completedItem = { ...streamingItem, status: 'completed' as const };
    const { container, getByLabelText, queryByLabelText, rerender } = render(
      AssistantMessage,
      {
        props: { item: streamingItem },
      },
    );

    const streamingSlot = container.querySelector(
      '[data-testid="assistant-message-copy-slot"]',
    );
    expect(streamingSlot?.className).toContain('h-7');
    expect(streamingSlot?.className).toContain('w-7');
    expect(queryByLabelText('Copy message')).toBeNull();

    await rerender({ item: completedItem });

    const completedSlot = container.querySelector(
      '[data-testid="assistant-message-copy-slot"]',
    );
    expect(completedSlot?.className).toBe(streamingSlot?.className);
    expect(getByLabelText('Copy message')).toBeInTheDocument();
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
