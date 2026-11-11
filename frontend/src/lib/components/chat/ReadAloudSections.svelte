<script lang="ts">
  // The reply being read: a rule in the dead gutter left of the section
  // being read, and a "read from here" button in the right gutter beside the
  // first line of every other section. A block split for length is several
  // sections with one place on screen, so it has one button, at its start,
  // for its first part; the rule spans the block while any part is read.
  // AssistantMessage mounts it only while its reply is being read or paused,
  // so an idle reply has no layer, no listener and no observer.
  //
  // Both are drawn in a zero-height layer after the body, positioned from the
  // geometry of the reply's top-level blocks (the children of the Markdown
  // wrapper's committed blocks, which the sections index). The renderer's
  // DOM is only read, so the row's height never changes. One ResizeObserver
  // on the body re-measures; while the reply still renders a volatile tail,
  // the wrapper's children are watched until it settles. When the rendered
  // blocks do not match the reading, nothing is drawn and a settled reply
  // reports it once.
  //
  // The rule sits in the 8px dead gutter between the nav rail's hit strip
  // and the text; the buttons sit in the row's 32px right gutter, clear of
  // the overlay scrollbar's 6px strip at its edge.
  //
  // With a mouse a section's button shows while the pointer is over that
  // section's blocks (found from the measured block map, no layout read) or
  // over the button, or while it has focus; on the compact layout all of
  // them show.

  import Play from '@lucide/svelte/icons/play';
  import Icon from '../primitives/Icon.svelte';
  import { lex } from '../../markdown';
  import { renderedBlocks } from '../../utils/speakableText';
  import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
  import {
    readAloudFromSection,
    readAloudPhase,
    readAloudPosition,
    readAloudSectionBlocks,
  } from '../../stores/readAloud.svelte';

  let { body, readingKey, markdown }: { body: HTMLElement; readingKey: string; markdown: string } = $props();

  interface Band {
    /** The section's first block top and last block bottom, from the layer's top. */
    top: number;
    bottom: number;
    /** The first line of its first block. */
    lineTop: number;
    lineHeight: number;
    /** Whether it starts at the start of a block: false for the later
     *  parts of a block split for length. */
    opens: boolean;
  }

  const position = $derived(readAloudPosition(readingKey));
  const paused = $derived(readAloudPhase(readingKey) === 'paused');
  const sections = $derived(readAloudSectionBlocks(readingKey));
  const expectedBlocks = $derived(renderedBlocks(lex(markdown)).length);

  let layer: HTMLElement;
  let bands = $state.raw<Band[] | null>(null);
  let hovered = $state<number | null>(null);
  // Each measured block element and the section whose button it reveals.
  let sectionOfBlock = new Map<Element, number>();
  let reported = false;
  let tailWatch: MutationObserver | null = null;

  // The top-level block elements, or null while a volatile tail renders.
  function blockElements(): Element[] | null {
    const root = body.querySelector('.markdown-body');
    if (!root) return null;
    let committed: Element | null = null;
    for (const child of root.children) {
      if (child.classList.contains('md-volatile')) return null;
      if (child.classList.contains('md-committed')) committed = child;
    }
    return committed ? Array.from(committed.children) : [];
  }

  function watchTail(root: Element | null): void {
    if (root === null) {
      tailWatch?.disconnect();
      tailWatch = null;
      return;
    }
    if (tailWatch) return;
    tailWatch = new MutationObserver(measure);
    tailWatch.observe(root, { childList: true });
  }

  // The first line box of `block`: the box of its first character, so a
  // long block's other lines are not laid out into rectangles.
  function firstLine(block: Element, fallbackTop: number): { top: number; height: number } {
    const walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const text = node.textContent ?? '';
      const at = text.search(/\S/);
      if (at < 0) continue;
      const range = document.createRange();
      range.setStart(node, at);
      range.setEnd(node, at + (text.codePointAt(at)! > 0xffff ? 2 : 1));
      const rect = typeof range.getClientRects === 'function' ? range.getClientRects()[0] : undefined;
      if (rect && rect.height > 0) return { top: rect.top, height: rect.height };
      break;
    }
    return { top: fallbackTop, height: 24 };
  }

  function measure(): void {
    const blocks = blockElements();
    watchTail(blocks === null ? body.querySelector('.markdown-body') : null);
    const fits = blocks !== null && blocks.length === expectedBlocks && sections.every((s) => s.last < blocks.length);
    if (!fits || !layer) {
      bands = null;
      sectionOfBlock = new Map();
      if (blocks !== null && !reported) {
        reported = true;
        reportFrontendDiagnostic(
          'Read aloud: the reply renders other blocks than it is read in',
          `rendered ${blocks.length}, expected ${expectedBlocks}`,
        );
      }
      return;
    }
    const origin = layer.getBoundingClientRect().top;
    const map = new Map<Element, number>();
    // Each block is read once, however many sections share it.
    const rects = new Map<number, DOMRect>();
    const rectOf = (block: number): DOMRect => {
      let rect = rects.get(block);
      if (!rect) {
        rect = blocks[block].getBoundingClientRect();
        rects.set(block, rect);
      }
      return rect;
    };
    // The first line of the block the section starts in; the later parts
    // of a split block keep the first part's.
    let line = { top: 0, height: 0 };
    bands = sections.map((section, index) => {
      const opens = index === 0 || sections[index - 1].first !== section.first;
      const first = rectOf(section.first);
      if (opens) {
        for (let i = section.first; i <= section.last; i += 1) map.set(blocks[i], index);
        line = firstLine(blocks[section.first], first.top);
      }
      return {
        top: first.top - origin,
        bottom: rectOf(section.last).bottom - origin,
        lineTop: line.top - origin,
        lineHeight: line.height,
        opens,
      };
    });
    sectionOfBlock = map;
  }

  // Re-measure when the reading moves to another section or its layout or
  // the reply changes.
  $effect(() => {
    void position?.section;
    void sections;
    void expectedBlocks;
    measure();
  });

  $effect(() => {
    const observer = new ResizeObserver(() => measure());
    observer.observe(body);
    const over = (event: PointerEvent): void => {
      for (let el = event.target as Element | null; el && el !== body; el = el.parentElement) {
        const section = sectionOfBlock.get(el);
        if (section !== undefined) {
          hovered = section;
          return;
        }
      }
    };
    const leave = (): void => {
      hovered = null;
    };
    body.addEventListener('pointerover', over);
    body.addEventListener('pointerleave', leave);
    return () => {
      observer.disconnect();
      body.removeEventListener('pointerover', over);
      body.removeEventListener('pointerleave', leave);
      watchTail(null);
    };
  });

  const current = $derived(bands && position ? bands[position.section] : undefined);
  // The sections with a button: each block's first, except the current one.
  const offers = $derived.by(() => {
    const out: Array<{ band: Band; section: number }> = [];
    if (!bands || !position) return out;
    bands.forEach((band, section) => {
      if (band.opens && section !== position.section) out.push({ band, section });
    });
    return out;
  });
</script>

<div
  bind:this={layer}
  class="pointer-events-none relative h-0"
  data-testid="assistant-message-read-aloud-sections"
  data-drawn={bands !== null}
>
  {#if current && position}
    <div
      class="absolute -left-[5px] w-[2px] rounded-full bg-accent {paused ? 'opacity-[0.45]' : ''}"
      style="top: {current.top}px; height: {current.bottom - current.top}px"
      data-testid="assistant-message-read-aloud-marker"
      data-section={position.section}
    ></div>
  {/if}
  {#each offers as { band, section } (section)}
    <div
      class="pointer-events-auto absolute left-full flex w-[26px] items-center justify-center transition-opacity duration-150 hover:opacity-100 focus-within:opacity-100 compact:opacity-100 {hovered === section ? 'opacity-100' : 'opacity-0'}"
      style="top: {band.lineTop}px; height: {band.lineHeight}px"
      data-section={section}
    >
      <!-- Not IconButton: its sizes do not fit the gutter left of the
           overlay scrollbar's strip on either layout. -->
      <button
        type="button"
        aria-label="Read from here"
        title="Read from here"
        class="inline-flex h-6 w-5 shrink-0 cursor-pointer items-center justify-center rounded-md text-text-secondary transition-colors hover:bg-surface-2/60 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 compact:h-9 compact:w-[26px]"
        data-testid="assistant-message-read-aloud-from"
        onclick={() => readAloudFromSection(section)}
      >
        <Icon icon={Play} size={12} />
      </button>
    </div>
  {/each}
</div>
