<script lang="ts">
  // Paints every command word in the draft in the accent colour (D31).
  //
  // The composer's input is a real <textarea>, which cannot colour part of its
  // own value, and the usual workaround — a full mirror behind a TRANSPARENT
  // textarea — is not acceptable here: transparent text hides the IME preedit
  // string and the selection highlight, in the one field where people compose
  // long prose in every language.
  //
  // So the textarea stays fully opaque and this mirror is invisible except for
  // the command words. It renders the same value in the same font, at the same
  // width, with the same wrapping, and every character transparent; the only
  // ink it puts down is each command word, drawn with an opaque `bg-card`
  // background so it covers the identical glyphs underneath. Layout, not
  // measurement, is what puts each word in the right place — which is the only
  // approach that stays correct when a word wraps, sits on the fifth line, or
  // moves because the line above it grew.
  //
  // The mirror's width is the textarea's `clientWidth`: the padding box minus
  // any scrollbar, which is exactly the width the textarea wraps its text to.
  // A ResizeObserver keeps it current; 100% is the pre-measurement fallback,
  // correct whenever no scrollbar is showing.
  //
  // Two states deliberately paint nothing rather than risk lying (the caller
  // enforces them by passing no ranges):
  //   - during IME composition, where the browser draws preedit text at the
  //     caret and the value the overlay was built from is momentarily stale;
  //   - while a selection is active, where the textarea's own selection
  //     highlight is painted under the opaque words and the reader would see
  //     holes in their selection.

  import { commandSegments, type CommandWordRange } from '../../utils/commandWords';

  interface Props {
    /** The exact value the textarea is rendering. */
    value: string;
    /** Word ranges to accent, sorted and non-overlapping. Empty paints nothing. */
    ranges: readonly Pick<CommandWordRange, 'start' | 'end'>[];
    /** The textarea's current vertical scroll offset, in pixels. */
    scrollTop: number;
    /** The textarea itself — measured for width, never written to. */
    textarea: HTMLTextAreaElement | undefined;
  }

  let { value, ranges, scrollTop, textarea }: Props = $props();

  const segments = $derived(ranges.length > 0 ? commandSegments(value, ranges) : []);

  let clientWidth = $state(0);
  $effect(() => {
    const node = textarea;
    if (!node) {
      clientWidth = 0;
      return;
    }
    // Seed synchronously: a ResizeObserver's first delivery is a frame away,
    // and in a jsdom test it never comes at all.
    clientWidth = node.clientWidth;
    const observer = new ResizeObserver(() => {
      clientWidth = node.clientWidth;
    });
    observer.observe(node);
    return () => observer.disconnect();
  });

  const width = $derived(clientWidth > 0 ? `${clientWidth}px` : '100%');
</script>

{#if segments.length > 0}
  <div
    class="pointer-events-none absolute inset-0 overflow-hidden"
    aria-hidden="true"
    data-testid="composer-command-highlight"
  >
    <!--
      Geometry twin of the textarea in Composer.svelte: same `px-1 py-1`
      padding, same `text-[0.8125rem] leading-[1.55]`, same inherited font
      family, and the wrapping a textarea does by default
      (`white-space: pre-wrap; overflow-wrap: break-word`).
    -->
    <div
      class="absolute left-0 top-0 whitespace-pre-wrap px-1 py-1 text-[0.8125rem] leading-[1.55] text-transparent [overflow-wrap:break-word]"
      style="width: {width}; transform: translateY({-scrollTop}px)"
    >{#each segments as segment, index (index)}{#if segment.command}<span
          class="bg-card text-accent"
          data-command-word={segment.text}
        >{segment.text}</span>{:else}{segment.text}{/if}{/each}</div>
  </div>
{/if}
