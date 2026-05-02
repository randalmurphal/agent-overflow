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
    const stripped = input.replace(/\x1b\][\s\S]*?(?:\x07|\x1b\\)/g, '').replace(/\x1b_[\s\S]*?\x1b\\/g, '');
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
    // Build the next tree in a detached host with the same tag so
    // Idiomorph can morph attributes safely (it normally morphs the
    // root too; we only want innerHTML morphing here).
    const next = document.createElement('pre');
    next.innerHTML = html;
    Idiomorph.morph(root, next, { morphStyle: 'innerHTML' });
  });
</script>

<pre bind:this={root} class={['ansi-body', className].filter(Boolean).join(' ')}></pre>
