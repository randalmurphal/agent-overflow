<script lang="ts" module>
  /*
   * Shared diff-line content renderer. Inline and review diff rows
   * consume this so span rendering is defined once. Pure: renders one
   * PatchLine given the resolved spans (caller does the cache lookup).
   *
   * The first character of `+` / `-` lines is the diff prefix and is
   * surfaced separately from the highlighted body — the backend spans
   * cover the prefix-stripped source text, so the prefix lives in its
   * own tinted span while span segments fill the body. Context lines
   * render with their leading space preserved (the backend pads their
   * runs by one plain byte); `meta` lines carry no runs and render
   * plain.
   *
   * `intraline` (review rows) marks the changed slice of a paired
   * add/del line: span segments are split at the range boundaries and
   * the inside segments get a stronger background wash.
   *
   * Inline spans only — no block boxes or vertical metrics. The review
   * pane's exact-height contract (REVIEW_LINE_HEIGHT_PX per visual
   * line) depends on this component adding zero vertical geometry.
   */
  import type { PatchLine } from '../../utils/patchFiles';
  import { prefixTintClass } from '../../utils/diffLineTint';
  import { segmentLine, type IntralineRange } from '../../utils/intralineDiff';
  import { spanSegments, type EncodedLine } from '../../utils/syntaxSpans';

  export type { PatchLine };
</script>

<script lang="ts">
  let {
    line,
    spans,
    intraline = null,
  }: {
    line: PatchLine;
    spans: EncodedLine | null;
    intraline?: IntralineRange | null;
  } = $props();

  let prefix = $derived(line.type === 'add' || line.type === 'del' ? line.content.charAt(0) : '');
  let plain = $derived(line.type === 'add' || line.type === 'del' ? line.content.slice(1) : line.content);
  // Plain lines (no runs) skip the segment walk and render raw text —
  // the common case; meta lines never carry runs by contract.
  let bodySegments = $derived(
    spans?.r && spans.r.length >= 2 ? spanSegments(plain, spans) : null,
  );
  let segments = $derived(
    intraline && line.type !== 'meta' ? segmentLine(bodySegments, plain, intraline) : null,
  );
  let emphClass = $derived(
    line.type === 'add' ? 'rounded-[2px] bg-success/35' : 'rounded-[2px] bg-error/35',
  );
</script>

{#if prefix}<span class={prefixTintClass(line.type)}>{prefix}</span>{/if}{#if segments}{#each segments as seg, si (si)}<span class="{seg.className} {seg.emph ? emphClass : ''}">{seg.text}</span>{/each}{:else if bodySegments}{#each bodySegments as seg, si (si)}<span class={seg.className}>{seg.text}</span>{/each}{:else}{plain}{/if}
