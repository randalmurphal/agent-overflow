<script lang="ts">
  // Paints the draft's leading command word in the accent colour (D31).
  //
  // The composer's input is a real <textarea>, which cannot colour part of its
  // own value, and the usual workaround — a full mirror behind a transparent
  // textarea — is not acceptable here: transparent text hides the IME preedit
  // string and the selection highlight, in the one field where people compose
  // long prose in every language.
  //
  // So this paints ONE word instead of mirroring the value. A command word is
  // by definition the first thing in the draft, so its position is not a
  // measurement: it sits at the textarea's content origin (its padding box,
  // minus scroll), on the first line, in the same font. An opaque `bg-card`
  // span drawn there covers the glyphs underneath with identical glyphs in the
  // accent colour, and the clip container hides it once the first line
  // scrolls away.
  //
  // Two states deliberately hide it rather than risk lying:
  //   - during IME composition, where the browser draws preedit text at the
  //     caret and the value the overlay was built from is momentarily stale;
  //   - while a selection is active, where the textarea's own selection
  //     highlight is painted under the opaque word and the reader would see a
  //     one-word hole in their selection.

  interface Props {
    /** The literal word to paint, e.g. `/workflow`. Empty paints nothing. */
    word: string;
    /** The textarea's current vertical scroll offset, in pixels. */
    scrollTop: number;
  }

  let { word, scrollTop }: Props = $props();
</script>

{#if word}
  <div
    class="pointer-events-none absolute inset-0 overflow-hidden"
    aria-hidden="true"
    data-testid="composer-command-highlight"
  >
    <!--
      Geometry twin of the textarea in Composer.svelte: same `px-1 py-1`
      padding as left/top offsets, same `text-[0.8125rem] leading-[1.55]`,
      same inherited font family. `whitespace-pre` keeps the word on one line
      the way the textarea's soft wrapping does for a word with no break
      opportunity.
    -->
    <span
      class="absolute left-1 top-1 whitespace-pre bg-card text-[0.8125rem] leading-[1.55] text-accent"
      style="transform: translateY({-scrollTop}px)"
      data-command-word={word}
    >{word}</span>
  </div>
{/if}
