import { describe, expect, it } from 'vitest';
import { getPathRefsFromMeta } from './pathLinkify';

// Identity-stability contract for the parsed pathRefs allowlist
// (end-of-turn stutter fix, 2026-07-21).
//
// `getPathRefsFromMeta` memoizes on the exact meta string for the
// per-frame streaming steady state, and CANONICALIZES the parsed array
// by content underneath it. The second layer is what the settle path
// depends on: triage merges the persisted `codeSpans` blob (and the
// final pathRefs sweep) into the SAME meta string
// (`persistItemFieldsAndPatch`), so a draining row's meta changes at
// settle while its path list is byte-identical. Without content
// canonicalization that string-keyed miss returned a fresh array
// identity, AssistantMessage's `pathRefs` $derived propagated it, and
// ChatMarkdown rebuilt its marked extension — re-lexing every mounted
// markdown block mid-drain (the pill-time stutter). The component-level
// half of this regression lives in
// components/chat/ChatMarkdown.settleRelex.test.ts.
describe('getPathRefsFromMeta identity across meta enrichment', () => {
  const pathRefs = [
    { path: 'src/lib/stores/thread.svelte.ts' },
    { path: 'src/lib/components/chat/MessageTimeline.svelte', line: 42 },
  ];

  it('returns the same array identity for repeated calls with an identical meta string', () => {
    const meta = JSON.stringify({ pathRefs });
    const first = getPathRefsFromMeta(meta);
    const second = getPathRefsFromMeta(meta);
    expect(first).toBeDefined();
    expect(second).toBe(first);
  });

  it('keeps identity across a settle-shaped meta change (codeSpans added, pathRefs byte-identical)', () => {
    // Streaming-time meta: pathRefs only (what applyItemMeta pushed on
    // the last text flush).
    const streamingMeta = JSON.stringify({ pathRefs });
    // Settle-time meta: same pathRefs plus the persisted highlight-span
    // blob (shape per utils/persistedSpans.ts — content irrelevant here,
    // only that it changes the meta STRING).
    const settleMeta = JSON.stringify({
      pathRefs,
      codeSpans: { hv: 1, spans: 'AAAA…persisted-span-blob…AAAA' },
    });

    const streamingRefs = getPathRefsFromMeta(streamingMeta);
    const settleRefs = getPathRefsFromMeta(settleMeta);

    expect(settleRefs).toEqual(streamingRefs);
    // Content-equal ⇒ the SAME canonical instance, so ChatMarkdown's
    // extension chain sees no change and nothing re-lexes at settle.
    expect(settleRefs).toBe(streamingRefs);
  });

  it('distinguishes genuinely different pathRefs content', () => {
    const a = getPathRefsFromMeta(JSON.stringify({ pathRefs }));
    const b = getPathRefsFromMeta(
      JSON.stringify({ pathRefs: [...pathRefs, { path: 'docs/extra.md' }] }),
    );
    expect(b).not.toBe(a);
    expect(b).toHaveLength(3);
  });

  it('keeps canonical identity for a row whose meta is HIT per frame while other panes churn the caches', () => {
    // The eviction hole the review caught: a draining row's per-frame
    // calls hit the meta-string memo and returned early, so its
    // CONTENT entry kept the recency stamp from the last meta change.
    // 128+ distinct allowlists parsed elsewhere (multi-pane session,
    // scrolling history) before settle would evict it, and the settle
    // meta minted a fresh identity — the re-lex regression, back for
    // exactly the row that needs the guarantee. Memo hits now
    // re-canonicalize, refreshing (or reinstalling) the content entry.
    const rowRefs = [{ path: 'src/lib/components/chat/ChatMarkdown.svelte' }];
    const streamingMeta = JSON.stringify({ pathRefs: rowRefs });
    const streamingRefs = getPathRefsFromMeta(streamingMeta);
    expect(streamingRefs).toBeDefined();

    // Interleave churn well past both LRU caps (128) with per-frame
    // hits on the streaming meta — the production access pattern.
    for (let i = 0; i < 200; i++) {
      getPathRefsFromMeta(JSON.stringify({ pathRefs: [{ path: `src/churn/${i}.ts` }] }));
      expect(getPathRefsFromMeta(streamingMeta)).toBe(streamingRefs);
    }

    const settleRefs = getPathRefsFromMeta(
      JSON.stringify({ pathRefs: rowRefs, codeSpans: { hv: 1, spans: 'blob' } }),
    );
    expect(settleRefs).toBe(streamingRefs);
  });

  it('does not collide refs whose fields concatenate ambiguously', () => {
    // Same joined text, different field structure: one ref with a line
    // vs the line digits living in a second ref's path. The canonical
    // key must keep these distinct.
    const a = getPathRefsFromMeta(
      JSON.stringify({ pathRefs: [{ path: 'a.ts', line: 2 }] }),
    );
    const b = getPathRefsFromMeta(
      JSON.stringify({ pathRefs: [{ path: 'a.ts' }, { path: '2.ts' }] }),
    );
    expect(a).toBeDefined();
    expect(b).toBeDefined();
    expect(b).not.toBe(a);
    expect(b).not.toEqual(a);
  });
});
