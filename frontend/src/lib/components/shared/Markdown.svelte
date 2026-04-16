<script lang="ts">
  import CodeBlock from './CodeBlock.svelte';

  let { content }: { content: string } = $props();

  // --- Block types ---

  type FencedCodeBlock = { kind: 'code'; lang: string; content: string };
  type HeadingBlock = { kind: 'heading'; level: 1 | 2 | 3; text: string };
  type ListBlock = { kind: 'list'; ordered: boolean; items: string[] };
  type ParagraphBlock = { kind: 'paragraph'; text: string };
  type Block = FencedCodeBlock | HeadingBlock | ListBlock | ParagraphBlock;

  // --- Inline formatting ---

  type InlineSegment =
    | { kind: 'text'; text: string }
    | { kind: 'code'; text: string }
    | { kind: 'bold'; text: string }
    | { kind: 'italic'; text: string }
    | { kind: 'link'; text: string; href: string };

  /**
   * Parse inline formatting within a text string.
   * Handles: inline code, bold, italic, links.
   * Returns segments in order for rendering.
   */
  function parseInline(text: string): InlineSegment[] {
    const segments: InlineSegment[] = [];
    // Pattern matches inline code, links, bold (**/__), italic (*/_) in priority order.
    // Inline code first (highest priority, prevents inner parsing).
    // Bold before italic so ** is matched before *.
    const pattern = /`([^`]+)`|\[([^\]]+)\]\(([^)]+)\)|\*\*(.+?)\*\*|__(.+?)__|\*(.+?)\*|_(.+?)_/g;
    let lastIndex = 0;
    let match: RegExpExecArray | null;

    while ((match = pattern.exec(text)) !== null) {
      // Push any plain text before this match
      if (match.index > lastIndex) {
        segments.push({ kind: 'text', text: text.slice(lastIndex, match.index) });
      }

      if (match[1] !== undefined) {
        segments.push({ kind: 'code', text: match[1] });
      } else if (match[2] !== undefined) {
        segments.push({ kind: 'link', text: match[2], href: match[3] });
      } else if (match[4] !== undefined) {
        segments.push({ kind: 'bold', text: match[4] });
      } else if (match[5] !== undefined) {
        segments.push({ kind: 'bold', text: match[5] });
      } else if (match[6] !== undefined) {
        segments.push({ kind: 'italic', text: match[6] });
      } else if (match[7] !== undefined) {
        segments.push({ kind: 'italic', text: match[7] });
      }

      lastIndex = match.index + match[0].length;
    }

    // Trailing plain text
    if (lastIndex < text.length) {
      segments.push({ kind: 'text', text: text.slice(lastIndex) });
    }

    return segments;
  }

  /**
   * Parse markdown content into a flat list of blocks.
   *
   * Strategy: extract fenced code blocks first (they're unambiguous),
   * then parse remaining text line-by-line into headings, lists, and paragraphs.
   */
  function parseBlocks(raw: string): Block[] {
    const blocks: Block[] = [];
    // Step 1: Split on fenced code blocks, preserving them.
    const codeBlockPattern = /^```(\w*)\n([\s\S]*?)^```$/gm;
    const textChunks: Array<{ type: 'text' | 'code'; content: string; lang?: string }> = [];
    let lastIndex = 0;
    let match: RegExpExecArray | null;

    while ((match = codeBlockPattern.exec(raw)) !== null) {
      if (match.index > lastIndex) {
        textChunks.push({ type: 'text', content: raw.slice(lastIndex, match.index) });
      }
      textChunks.push({ type: 'code', content: match[2], lang: match[1] || '' });
      lastIndex = match.index + match[0].length;
    }
    if (lastIndex < raw.length) {
      textChunks.push({ type: 'text', content: raw.slice(lastIndex) });
    }
    if (textChunks.length === 0) {
      textChunks.push({ type: 'text', content: raw });
    }

    // Step 2: For each chunk, either emit a code block or parse lines.
    for (const chunk of textChunks) {
      if (chunk.type === 'code') {
        blocks.push({ kind: 'code', lang: chunk.lang ?? '', content: chunk.content });
        continue;
      }

      parseTextIntoBlocks(chunk.content, blocks);
    }

    return blocks;
  }

  /**
   * Parse a text chunk (no fenced code blocks) into headings, lists, and paragraphs.
   * Pushes results onto the provided blocks array.
   */
  function parseTextIntoBlocks(text: string, blocks: Block[]): void {
    const lines = text.split('\n');
    let i = 0;

    while (i < lines.length) {
      const line = lines[i];

      // Skip blank lines
      if (line.trim() === '') {
        i++;
        continue;
      }

      // Heading: # through ###
      const headingMatch = line.match(/^(#{1,3})\s+(.+)$/);
      if (headingMatch) {
        blocks.push({
          kind: 'heading',
          level: headingMatch[1].length as 1 | 2 | 3,
          text: headingMatch[2],
        });
        i++;
        continue;
      }

      // Unordered list: lines starting with - or *
      if (/^[\-\*]\s+/.test(line)) {
        const items: string[] = [];
        while (i < lines.length && /^[\-\*]\s+/.test(lines[i])) {
          items.push(lines[i].replace(/^[\-\*]\s+/, ''));
          i++;
        }
        blocks.push({ kind: 'list', ordered: false, items });
        continue;
      }

      // Ordered list: lines starting with a digit followed by . or )
      if (/^\d+[.)]\s+/.test(line)) {
        const items: string[] = [];
        while (i < lines.length && /^\d+[.)]\s+/.test(lines[i])) {
          items.push(lines[i].replace(/^\d+[.)]\s+/, ''));
          i++;
        }
        blocks.push({ kind: 'list', ordered: true, items });
        continue;
      }

      // Paragraph: collect contiguous non-blank, non-special lines
      const paraLines: string[] = [];
      while (i < lines.length) {
        const l = lines[i];
        if (l.trim() === '') break;
        if (/^#{1,3}\s+/.test(l)) break;
        if (/^[\-\*]\s+/.test(l)) break;
        if (/^\d+[.)]\s+/.test(l)) break;
        paraLines.push(l);
        i++;
      }
      if (paraLines.length > 0) {
        blocks.push({ kind: 'paragraph', text: paraLines.join('\n') });
      }
    }
  }

  let parsed = $derived(parseBlocks(content));
</script>

{#each parsed as block}
  {#if block.kind === 'code'}
    <CodeBlock code={block.content} lang={block.lang || 'text'} />

  {:else if block.kind === 'heading'}
    {#if block.level === 1}
      <h1 class="text-lg font-semibold text-text-primary mt-3 mb-1">{block.text}</h1>
    {:else if block.level === 2}
      <h2 class="text-base font-semibold text-text-primary mt-2.5 mb-1">{block.text}</h2>
    {:else}
      <h3 class="text-sm font-semibold text-text-primary mt-2 mb-0.5">{block.text}</h3>
    {/if}

  {:else if block.kind === 'list'}
    {#if block.ordered}
      <ol class="list-decimal list-inside text-sm leading-relaxed my-1 space-y-0.5">
        {#each block.items as item}
          <li>
            {#each parseInline(item) as seg}
              {#if seg.kind === 'code'}
                <code class="bg-surface-0 px-1 rounded text-sm font-mono">{seg.text}</code>
              {:else if seg.kind === 'bold'}
                <strong>{seg.text}</strong>
              {:else if seg.kind === 'italic'}
                <em>{seg.text}</em>
              {:else if seg.kind === 'link'}
                <a href={seg.href} class="text-accent hover:underline" target="_blank" rel="noopener">{seg.text}</a>
              {:else}
                {seg.text}
              {/if}
            {/each}
          </li>
        {/each}
      </ol>
    {:else}
      <ul class="list-disc list-inside text-sm leading-relaxed my-1 space-y-0.5">
        {#each block.items as item}
          <li>
            {#each parseInline(item) as seg}
              {#if seg.kind === 'code'}
                <code class="bg-surface-0 px-1 rounded text-sm font-mono">{seg.text}</code>
              {:else if seg.kind === 'bold'}
                <strong>{seg.text}</strong>
              {:else if seg.kind === 'italic'}
                <em>{seg.text}</em>
              {:else if seg.kind === 'link'}
                <a href={seg.href} class="text-accent hover:underline" target="_blank" rel="noopener">{seg.text}</a>
              {:else}
                {seg.text}
              {/if}
            {/each}
          </li>
        {/each}
      </ul>
    {/if}

  {:else if block.kind === 'paragraph'}
    <p class="whitespace-pre-wrap text-sm leading-relaxed my-1">
      {#each parseInline(block.text) as seg}
        {#if seg.kind === 'code'}
          <code class="bg-surface-0 px-1 rounded text-sm font-mono">{seg.text}</code>
        {:else if seg.kind === 'bold'}
          <strong>{seg.text}</strong>
        {:else if seg.kind === 'italic'}
          <em>{seg.text}</em>
        {:else if seg.kind === 'link'}
          <a href={seg.href} class="text-accent hover:underline" target="_blank" rel="noopener">{seg.text}</a>
        {:else}
          {seg.text}
        {/if}
      {/each}
    </p>
  {/if}
{/each}
