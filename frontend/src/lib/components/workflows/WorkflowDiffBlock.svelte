<script lang="ts">
  import { tick } from 'svelte';
  import { ownText, PatchTextLost, type ReviewFile } from '../../utils/patchStore';
  import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
  import { WORKFLOW_DIFF_LINE_PX } from './WorkflowDiffLines.svelte';

  // Lines [start, end) of a gate-diff file as preformatted text. The text
  // is read when the block mounts, again from the diff if it was evicted;
  // until then the block keeps its height with empty lines. A read that
  // fails marks the diff lost, and its owner (WorkflowGateDiff) reads the
  // diff again. Once the text renders, the block reports how wide it is,
  // so the box can scroll sideways to its longest line.

  interface Props {
    file: ReviewFile;
    start: number;
    end: number;
    padTop: number;
    padBottom: number;
    onWidth: (px: number) => void;
  }
  let { file, start, end, padTop, padBottom, onWidth }: Props = $props();

  let text = $state('');
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
          reportFrontendDiagnostic('workflow diff: block text could not be read', String(error));
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
  style:height="{(end - start) * WORKFLOW_DIFF_LINE_PX + padTop + padBottom}px"
  style:padding-top="{padTop}px"
  style:padding-bottom="{padBottom}px"
  style:box-sizing="border-box"
>{text}</pre>
