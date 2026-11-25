<script lang="ts">
  // Renders a string of ANSI-coloured text into spans. The naive
  // approach — `<pre>{@html html}</pre>` — wholesale-replaces the
  // entire `<pre>`'s children every time `source` updates by even
  // one character, which destroys text selection and forces the
  // browser to redo layout for the entire block on every chunk.
  //
  // We keep the synchronous string build (it's cheap) but apply the
  // result through Idiomorph, which diffs the new HTML against the
  // live DOM and patches only the changed nodes. Stable runs of text
  // and color spans survive the morph, so a user mid-selection
  // doesn't lose their selection on the next chunk and the browser
  // doesn't re-tokenize unchanged spans.
  //
  // Idiomorph is a 3 KB dependency, used in production by Basecamp's
  // Turbo 8 (which switched from morphdom for exactly this pattern).
  // Trade-off: parses each new HTML string into a temp tree to diff;
  // negligible cost for the line counts we render here. What the escapes
  // show is utils/ansiText.ts's, which find counts by.

  import { Idiomorph } from 'idiomorph';
  import {
    ansiHtmlWithState,
    appendAnsiDeltaToDom,
    type AnsiTailState,
  } from '../../utils/ansiText';

  let { source, class: className = '' }: { source: string; class?: string } = $props();

  let root: HTMLPreElement | undefined = $state();
  let renderedRoot: HTMLPreElement | undefined;
  let renderedSource = '';
  let tailState: AnsiTailState = { openClass: '', incompleteTail: false, endsWithCR: false };

  $effect(() => {
    const nextSource = source;
    if (!root) return;
    const sameRoot = root === renderedRoot;
    if (sameRoot && nextSource === renderedSource) return;

    // Fast path 1: clean streaming append — mutates only the tail nodes in O(delta).
    if (
      sameRoot &&
      renderedSource.length > 0 &&
      nextSource.length > renderedSource.length &&
      nextSource.startsWith(renderedSource)
    ) {
      const nextState = appendAnsiDeltaToDom(
        root,
        nextSource.slice(renderedSource.length),
        tailState,
      );
      if (nextState !== null) {
        renderedSource = nextSource;
        tailState = nextState;
        return;
      }
    }

    const { html, state } = ansiHtmlWithState(nextSource);
    // Fast path 2: initial mount into an empty <pre> needs no Idiomorph diff.
    if (!sameRoot || root.firstChild === null) {
      root.innerHTML = html;
      renderedRoot = root;
      renderedSource = nextSource;
      tailState = state;
      return;
    }

    // Fallback: non-append rewrite or completed split escape — morph via Idiomorph.
    const next = document.createElement('pre');
    next.innerHTML = html;
    Idiomorph.morph(root, Array.from(next.childNodes), { morphStyle: 'innerHTML' });
    renderedRoot = root;
    renderedSource = nextSource;
    tailState = state;
  });
</script>

<pre bind:this={root} class={['ansi-body', className].filter(Boolean).join(' ')}></pre>
