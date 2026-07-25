import { describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { getPathRefsFromMeta } from '../../utils/pathLinkify';

// Regression guard for the end-of-turn stutter (2026-07-21): the settle
// patch swaps `item.meta` on a still-draining assistant_text row
// (codeSpans blob + final pathRefs merged into one meta string). The
// parsed pathRefs must resolve to the SAME canonical array instance
// (utils/pathLinkify.metaIdentity.test.ts pins that layer) so
// ChatMarkdown does NOT rebuild its marked extension — a changed
// `extensions` identity re-lexes EVERY mounted block in both Streamdown
// instances (Block.svelte `tokens` $derived): O(message) parse work
// landing mid-drain, right when the response pill appears.
//
// This file proves the component half with a work counter: the
// extension's `start(src)` hook is invoked by marked's inline lexer
// while blocks tokenize, so its call count is a direct measure of
// lexing work. A settle-shaped meta change must cost ZERO re-lexing; a
// genuine allowlist change must still re-lex (links have to appear).

const counters = vi.hoisted(() => ({ builds: 0, startCalls: 0 }));

vi.mock('../../utils/pathLinkExtension', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../utils/pathLinkExtension')>();
  return {
    ...mod,
    buildPathLinkExtension: (
      ...args: Parameters<typeof mod.buildPathLinkExtension>
    ) => {
      counters.builds += 1;
      const ext = mod.buildPathLinkExtension(...args);
      if (!ext) return ext;
      return {
        ...ext,
        start(src: string) {
          counters.startCalls += 1;
          return ext.start(src);
        },
      };
    },
  };
});

import ChatMarkdownRelexHarness from './ChatMarkdownRelexHarness.svelte';

const PATH = 'src/lib/stores/thread.svelte.ts';
const EXTRA_PATH = 'docs/architecture/frontend-scroll.md';

// Several prose blocks, each mentioning the allowlisted path so every
// block's inline lex traverses the extension at least once. Rendered
// non-streaming: a settled row (and the committed prefix of a streaming
// row) is a single Streamdown instance, which is exactly the instance
// the settle-time re-lex would hit hardest.
const SOURCE = Array.from(
  { length: 6 },
  (_, i) => `Paragraph ${i} touches ${PATH} while explaining the change in detail.`,
).join('\n\n');

describe('<ChatMarkdown> lexing across pathRefs meta enrichment', () => {
  it('a settle-shaped meta change costs zero re-lexing; a genuine allowlist change still re-lexes', async () => {
    // Derive the arrays exactly the way AssistantMessage does — from
    // the streaming-time meta and the settle-time meta (same pathRefs,
    // codeSpans blob added). Content-equal ⇒ same canonical instance.
    const streamingRefs = getPathRefsFromMeta(
      JSON.stringify({ pathRefs: [{ path: PATH }] }),
    )!;
    const settleRefs = getPathRefsFromMeta(
      JSON.stringify({
        pathRefs: [{ path: PATH }],
        codeSpans: { hv: 1, spans: 'persisted-span-blob' },
      }),
    )!;
    const changedRefs = getPathRefsFromMeta(
      JSON.stringify({ pathRefs: [{ path: PATH }, { path: EXTRA_PATH }] }),
    )!;

    counters.builds = 0;
    counters.startCalls = 0;
    const r = render(ChatMarkdownRelexHarness, {
      props: { source: SOURCE, initialRefs: streamingRefs },
    });
    await waitFor(() => {
      expect(
        r.container.querySelectorAll('a[href^="agent-overflow:open"]').length,
      ).toBe(6);
    });
    const mountBuilds = counters.builds;
    const mountStartCalls = counters.startCalls;
    expect(mountBuilds).toBeGreaterThanOrEqual(1);
    // Every block traversed the extension during the mount lex.
    expect(mountStartCalls).toBeGreaterThanOrEqual(6);

    // THE regression: the settle patch's meta enrichment resolves to
    // the same canonical refs instance, so the $derived chain stops and
    // no extension rebuild / re-lex happens mid-drain.
    flushSync(() => r.component.setRefs(settleRefs));
    expect(counters.builds).toBe(mountBuilds);
    expect(counters.startCalls).toBe(mountStartCalls);

    // Sanity: a genuine allowlist change must still rebuild and re-lex
    // (the new path has to linkify), proving the counter would have
    // caught a settle-shape regression above.
    flushSync(() => r.component.setRefs(changedRefs));
    expect(counters.builds).toBe(mountBuilds + 1);
    expect(counters.startCalls - mountStartCalls).toBeGreaterThanOrEqual(6);

    r.unmount();
  });
});
