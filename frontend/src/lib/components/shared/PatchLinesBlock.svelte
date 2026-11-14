<script lang="ts">
  import { tick } from 'svelte';
  import { lineTypeOf, ownText, PatchTextLost, type ReviewFile } from '../../utils/patchStore';
  import { lineTintClass } from '../../utils/diffLineTint';
  import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
  import { PATCH_LINES_LINE_PX } from './PatchLines.svelte';

  // Lines [start, end) of a file's patch as preformatted text. The text
  // is read when the block mounts, again from the diff if it was evicted;
  // until then the block keeps its height with empty lines. A read that
  // fails marks the diff lost, and its owner (WorkflowGateDiff) reads the
  // diff again. Once the text renders, the block reports how wide it is,
  // so the box can scroll sideways to its longest line. With `tint`, each
  // line carries the review surface's add/del/meta tint.

  interface Props {
    file: ReviewFile;
    start: number;
    end: number;
    padTop: number;
    padBottom: number;
    onWidth: (px: number) => void;
    tint?: boolean;
  }
  let { file, start, end, padTop, padBottom, onWidth, tint = false }: Props = $props();

  let text = $state('');
  let lines = $derived(tint && text ? text.split('\n') : []);
  let pre: HTMLPreElement | undefined = $state();

  $effect(() => {
    const body = file.body;
    const from = start;
    const to = end;
    let live = true;
    text = '';
    body.whenResident(() => {
      const lines: string[] = [];
      for (let index = from; index < to; index += 1) lines.push(body.text(index));
      // A copy: a kept line must not keep its whole chunk.
      return ownText(lines.join('\n'));
    }, from, to).then(
      (read) => {
        if (!live) return;
        text = read;
        void tick().then(() => {
          if (live && pre) onWidth(pre.scrollWidth);
        });
      },
      (error: unknown) => {
        if (!(error instanceof PatchTextLost)) {
          reportFrontendDiagnostic('patch lines: block text could not be read', String(error));
        }
      },
    );
    return () => {
      live = false;
    };
  });
</script>

<pre
  bind:this={pre}
  class="m-0 px-2 font-mono"
  style:height="{(end - start) * PATCH_LINES_LINE_PX + padTop + padBottom}px"
  style:padding-top="{padTop}px"
  style:padding-bottom="{padBottom}px"
  style:box-sizing="border-box"
>{#if tint}{#each lines as line, offset (offset)}<span class="block {lineTintClass(lineTypeOf(file.body.kind(start + offset)))}" style:height="{PATCH_LINES_LINE_PX}px">{line}</span>{/each}{:else}{text}{/if}</pre>
