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
   * the parent element while tokens fill the body.
   *
   * `meta` lines render plain (no tokenization is attempted upstream;
   * tokens will be null). `context` lines render with leading space
   * preserved; `add`/`del` lines render the prefix + tokenized body.
   */
  import type { PatchLine } from '../../utils/patchFiles';
  import type { LineToken } from '../../utils/tokenCache';
  import { fontStyleClass } from '../../utils/diffLineTint';

  export type { PatchLine, LineToken };
</script>

<script lang="ts">
  let {
    line,
    tokens,
  }: {
    line: PatchLine;
    tokens: LineToken[] | null;
  } = $props();

  let prefix = $derived(line.type === 'add' || line.type === 'del' ? line.content.charAt(0) : '');
  let plain = $derived(line.type === 'add' || line.type === 'del' ? line.content.slice(1) : line.content);
</script>

{#if prefix}{prefix}{/if}{#if tokens && tokens.length > 0 && line.type !== 'meta'}{#each tokens as token, ti (ti)}<span style:color={token.color} class={fontStyleClass(token.fontStyle)}>{token.content}</span>{/each}{:else}{plain}{/if}
