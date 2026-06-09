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
  // negligible cost for the line counts we render here.

  import { Idiomorph } from 'idiomorph';
  import { escapeHtml } from '../../utils/markdownRender';

  let { source, class: className = '' }: { source: string; class?: string } = $props();

  let root: HTMLPreElement | undefined = $state();

  function renderAnsi(input: string): string {
    // Strip OSC (`ESC ]` … BEL or ST) and APC (`ESC _` … ST) sequences. The
    // body uses a negated class that excludes the terminator's lead bytes
    // (ESC / BEL) rather than a lazy `[\s\S]*?`: a lazy match against an
    // alternation terminator backtracks toward O(n²) on pathological input —
    // e.g. thousands of unterminated `ESC ]` starts each rescan to EOF — while
    // the negated class can't backtrack, so each start scans forward once.
    // Real OSC payloads (window titles, OSC-8 hyperlinks) never embed a bare
    // ESC, so stopping at the first ESC/BEL matches terminal behaviour.
    const stripped = input
      .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, '')
      .replace(/\x1b_[^\x1b]*\x1b\\/g, '');
    const parts: string[] = [];
    let open = false;
    let cursor = 0;
    const pattern = /\x1b\[([0-9;]*)m/g;
    let match: RegExpExecArray | null;

    while ((match = pattern.exec(stripped)) !== null) {
      parts.push(escapeHtml(stripped.slice(cursor, match.index)));
      if (open) {
        parts.push('</span>');
        open = false;
      }
      const classNameForCode = ansiClass(match[1] ?? '');
      if (classNameForCode) {
        parts.push(`<span class="${classNameForCode}">`);
        open = true;
      }
      cursor = pattern.lastIndex;
    }

    parts.push(escapeHtml(stripped.slice(cursor)));
    if (open) {
      parts.push('</span>');
    }
    return parts.join('');
  }

  function ansiClass(codeText: string): string {
    const codes = codeText
      .split(';')
      .map((code) => Number(code || 0))
      .filter((code) => Number.isFinite(code));
    if (codes.includes(0)) {
      return '';
    }
    const classes: string[] = [];
    if (codes.includes(1)) classes.push('ansi-bold');
    if (codes.includes(3)) classes.push('ansi-italic');
    if (codes.includes(4)) classes.push('ansi-underline');
    const foreground = codes.find((code) => (code >= 30 && code <= 37) || (code >= 90 && code <= 97));
    if (foreground) classes.push(`ansi-fg-${foreground}`);
    return classes.join(' ');
  }

  $effect(() => {
    const html = renderAnsi(source);
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
