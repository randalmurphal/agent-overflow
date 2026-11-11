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
  import { ansiHtml } from '../../utils/ansiText';

  let { source, class: className = '' }: { source: string; class?: string } = $props();

  let root: HTMLPreElement | undefined = $state();

  $effect(() => {
    const html = ansiHtml(source);
    if (!root) return;
    // Parse the new HTML inside a detached <pre> so the browser tokenizes
    // it in the same context our root <pre> uses, then hand Idiomorph the
    // parsed CHILD NODES — not the <pre> host itself. Passing a parentless
    // element makes Idiomorph wrap it in a dummy <div> and nest it as a
    // child, yielding `<pre class="…wrap…"><pre>TEXT</pre></pre>`; that inner
    // class-less <pre> computes the UA default `white-space: pre` and never
    // wraps (the wrap classes only live on the outer <pre>). Morphing the
    // child nodes diffs them straight into root's children, so the single
    // classed root <pre> is the only <pre> and the wrap classes apply to the
    // text.
    const next = document.createElement('pre');
    next.innerHTML = html;
    Idiomorph.morph(root, Array.from(next.childNodes), { morphStyle: 'innerHTML' });
  });
</script>

<pre bind:this={root} class={['ansi-body', className].filter(Boolean).join(' ')}></pre>
