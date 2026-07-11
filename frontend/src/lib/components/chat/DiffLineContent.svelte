<script lang="ts" module>
  /*
   * Shared diff-line content renderer. Inline and review diff rows
   * consume this so tokenized line rendering is defined once. Pure:
   * renders one PatchLine given the resolved tokens (caller does the
   * cache lookup).
   *
   * The first character of `+` / `-` lines is the diff prefix and is
   * surfaced separately from the tokenized body — Shiki tokenizes the
   * source-only text (`stripPatchLinePrefix`), so the prefix lives in
   * its own tinted span while tokens fill the body.
   *
   * `meta` lines render plain (no tokenization is attempted upstream;
   * tokens will be null). `context` lines render with leading space
   * preserved; `add`/`del` lines render the prefix + tokenized body.
   *
   * `intraline` (review rows) marks the changed slice of a paired
   * add/del line: tokens are split at the range boundaries and the
   * inside segments get a stronger background wash.
   */
  import type { PatchLine } from '../../utils/patchFiles';
  import type { LineToken } from '../../utils/tokenCache';
  import { fontStyleClass, prefixTintClass } from '../../utils/diffLineTint';
  import { segmentLine, type IntralineRange } from '../../utils/intralineDiff';

  export type { PatchLine, LineToken };
</script>

<script lang="ts">
  let {
    line,
    tokens,
    intraline = null,
  }: {
    line: PatchLine;
    tokens: LineToken[] | null;
    intraline?: IntralineRange | null;
  } = $props();

  let prefix = $derived(line.type === 'add' || line.type === 'del' ? line.content.charAt(0) : '');
  let plain = $derived(line.type === 'add' || line.type === 'del' ? line.content.slice(1) : line.content);
  let segments = $derived(
    intraline && line.type !== 'meta' ? segmentLine(tokens, plain, intraline) : null,
  );
  let emphClass = $derived(
    line.type === 'add' ? 'rounded-[2px] bg-success/35' : 'rounded-[2px] bg-error/35',
  );
</script>

{#if prefix}<span class={prefixTintClass(line.type)}>{prefix}</span>{/if}{#if segments}{#each segments as seg, si (si)}<span style:color={seg.color} class="{fontStyleClass(seg.fontStyle)} {seg.emph ? emphClass : ''}">{seg.text}</span>{/each}{:else if tokens && tokens.length > 0 && line.type !== 'meta'}{#each tokens as token, ti (ti)}<span style:color={token.color} class={fontStyleClass(token.fontStyle)}>{token.content}</span>{/each}{:else}{plain}{/if}
