<script lang="ts">
  import type { Snippet } from 'svelte';
  import ReviewSuggestionDiff from './ReviewSuggestionDiff.svelte';
  import { readNewSideLines } from '../../utils/suggestionLines';
  import type { ReviewFile } from '../../utils/patchStore';

  // One suggestion block with the lines it replaces: the forge's own when
  // it reports them (`removedKnown`, GitLab's from_content), else the
  // new-side lines `from`..`to` of `file` read from the diff the pane
  // holds (GitHub sends none; an editor's preview has its anchor), else
  // the additions alone. A read that answers after its lines changed (the
  // anchor moved, another diff loaded, the block went) is dropped. Text
  // the diff can no longer read shows the additions alone too.

  interface Props {
    added: readonly string[];
    removedKnown?: readonly string[] | null;
    file?: ReviewFile | null;
    from?: number;
    to?: number;
    footer?: Snippet;
  }

  let { added, removedKnown = null, file = null, from = 0, to = 0, footer }: Props = $props();

  let read = $state.raw<{ file: ReviewFile; from: number; to: number; lines: string[] | null } | null>(null);

  $effect(() => {
    const source = file;
    const first = from;
    const last = to;
    if (removedKnown !== null || !source || first <= 0 || last < first) return;
    let live = true;
    readNewSideLines(source, first, last).then(
      (lines) => { if (live) read = { file: source, from: first, to: last, lines }; },
      // PatchTextLost: the diff can no longer give the text.
      () => { if (live) read = { file: source, from: first, to: last, lines: null }; },
    );
    return () => { live = false; };
  });

  const removed = $derived.by<readonly string[] | null>(() => {
    if (removedKnown !== null) return removedKnown;
    const done = read;
    if (!done || done.file !== file || done.from !== from || done.to !== to) return null;
    return done.lines;
  });
</script>

<ReviewSuggestionDiff {removed} {added} {footer} />
