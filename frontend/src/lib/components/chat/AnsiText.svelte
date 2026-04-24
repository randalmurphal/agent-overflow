<script lang="ts">
  import { escapeHtml } from '../../utils/markdownRender';

  let { source, class: className = '' }: { source: string; class?: string } = $props();

  const html = $derived(renderAnsi(source));

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
</script>

<pre class={['ansi-body', className].filter(Boolean).join(' ')}>{@html html}</pre>

