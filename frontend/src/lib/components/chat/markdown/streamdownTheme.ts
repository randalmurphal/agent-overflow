// THE markdown theme. One flat table, handed to `<Streamdown>` whole.
//
// It used to be an override layer: the library shipped a Tailwind base theme
// (hardcoded light colors, chunky rounded-xl cards) and merged a host's
// partial table over it per read with `tailwind-merge`, so an override only
// cancelled a vendor value when it collided in the same (modifier, utility
// category) key. Both halves are gone — the base table, the merge helper,
// `clsx` and `tailwind-merge`. What is written here IS what renders, and the
// entries below are the merge's settled output, so no class had to change.
//
// NOTE: do not write a Tailwind palette class VERBATIM in these comments.
// Tailwind scans comments too, so quoting one compiles a real (dead) rule plus
// its `--color-*` entry into the bundle. Name the scale in prose instead.
//
// Design intent — "sleek but defined":
//   - No outer borders on code blocks, tables, or mermaid wrappers.
//     Definition comes from a single elevation step (surface-0 page bg →
//     surface-1 block bg) and a thin internal divider for header / row
//     separation.
//   - Tighter padding — code header is one-line chrome, table cells drop the
//     200-400px width clamps.
//   - Tokens come from `app.css` `@theme` (`bg-surface-1`, `text-fg-muted`,
//     `border-border-subtle`, etc.) so light/dark theme switching keeps
//     working without a second copy of values.
//
// The roster is enforced, not documented: `streamdownTheme.test.ts` derives
// every `streamdown.theme.<group>.<slot>` read out of the render path's source
// and fails in both directions — a slot the renderer reads and this table
// omits renders `class={undefined}`, and an entry nothing reads is a dead
// Tailwind rule in the bundle.

import type { Theme } from '../../../markdown';

// `md-blk` marks every element that can be a DIRECT child of the
// .md-committed / .md-volatile wrapper — the app.css edge-margin resets
// (`.markdown-body > … > .md-blk:first-child`) key on it. The class is
// not decorative: with a bare `:first-child` in that position, Blink
// files the rule in its UNIVERSAL invalidation sets, and every
// sibling-list change anywhere in the document paid a whole-subtree
// style recalc (measured 2026-08-25: 131 subtree invalidations/15s,
// 1,091-element passes during two-pane streaming). A block-level theme
// entry missing the marker keeps its own edge margin when it lands
// first/last in a message — streamdownTheme.test.ts pins the roster.
export const MD_BLOCK_MARKER = 'md-blk';

export const chatMarkdownTheme: Theme = {
  // Links: `text-md-link` names the token the unlayered `.markdown-body a`
  // rule (app.css) actually paints, themeable per palette. The blocked
  // variant is a <span>, so no element rule competes and its class does
  // paint; a code label inside it keeps the de-emphasis via the app.css
  // carve-out.
  link: {
    base: 'text-md-link font-medium underline wrap-anywhere hover:text-md-link',
    blocked: 'text-fg-hint',
  },
  // Compact heading sizing that matches the `.markdown-body` rules in
  // app.css, rather than the library's mt-6/mb-2/text-3xl chat-hostile scale.
  h1: { base: 'md-blk mt-3 mb-1 text-lg font-semibold' },
  h2: { base: 'md-blk mt-3 mb-1 text-base font-semibold' },
  h3: { base: 'md-blk mt-2 mb-1 text-[0.9375rem] font-semibold' },
  h4: { base: 'md-blk mt-2 mb-1 text-sm font-semibold' },
  h5: { base: 'md-blk mt-2 mb-1 text-sm font-semibold' },
  h6: { base: 'md-blk mt-2 mb-1 text-sm font-semibold' },
  // Block-level entries that carry nothing but the md-blk marker (see its
  // doc above); spacing is the cascade's.
  paragraph: { base: 'md-blk' },
  // pl matches `.markdown-body ul/ol` in app.css (which wins the cascade):
  // 2em of marker room so wide fonts don't clip — see the comment there.
  ul: { base: 'md-blk ml-0 pl-[2em] list-outside list-disc whitespace-normal' },
  ol: { base: 'md-blk ml-0 pl-[2em] list-outside whitespace-normal' },
  // `marker:hidden` is load-bearing for task items, whose marker is the
  // rendered checkbox.
  li: { base: 'marker:hidden py-0.5', checkbox: 'mr-2' },
  code: {
    // No border, no rounded-xl card: a single rounded-md wrapper. Background
    // contrast (the code-block ground vs the page's surface-0) is what
    // defines the block.
    //
    // `bg-code-block` rather than `bg-surface-1`: the code-block ground is
    // its own role (it travels with a code theme), even though the token
    // currently aliases the surface-1 elevation tier. Keep in step with
    // `.markdown-body pre`'s `--code-block` in app.css.
    //
    // `border-border-subtle` alongside `border-0`: border WIDTH and border
    // COLOR are separate concerns, and this block is one `border` away from
    // painting. Inert at zero width today.
    base: 'md-blk my-3 w-full overflow-hidden rounded-md border-0 border-border-subtle flex flex-col bg-code-block',
    // Same background as base — the inner container is mostly there for the
    // relative positioning the host's copy overlay expects.
    container: 'relative overflow-visible bg-transparent p-0 font-mono text-[0.8125rem]',
    // The pre/code body. Transparent bg so it inherits the wrapper's ground;
    // tightened horizontal padding.
    //
    // All three entries are consumed by `StreamdownCodeHost` /
    // `staticCodeBlock.ts`, which render the pre/code DOM themselves from
    // backend spans. Chat code blocks are zero-chrome: the host's
    // hover-revealed copy button is the only affordance.
    pre: 'whitespace-pre-wrap wrap-anywhere overflow-x-visible font-mono p-3 bg-transparent',
  },
  // Inline code. `app.css` already styles `.markdown-body code` via
  // `--code-inline-bg`, so the background stays transparent and the cascade
  // wins. Layout stays here because it is specific to codespans: they
  // participate in normal inline layout, and long paths / hashes may break
  // under constraint.
  codespan: {
    base: 'inline whitespace-pre-wrap wrap-anywhere align-baseline rounded px-1.5 py-0.5 font-mono text-[0.9em] leading-[1.35] bg-transparent',
  },
  // Reached only without an `image` snippet: `ChatMarkdown` supplies
  // `StreamdownImageHost`, which owns its own classes. The harnesses that
  // mount `<Streamdown>` bare fall through to these.
  image: {
    base: 'group relative my-4  mx-auto w-fit block',
    image: 'max-w-full rounded-lg',
  },
  // `text-md-blockquote` names the token the unlayered `.markdown-body
  // blockquote` rule in app.css paints (var(--md-blockquote)), so the class
  // cannot disagree with what the cascade renders.
  blockquote: {
    base: 'md-blk border-border-subtle text-md-blockquote my-3 border-l-2 pl-3 italic',
  },
  // GFM alerts (`> [!NOTE]` … ). The variant entry lands on the alert
  // container (left border), its `[data-alert-title]` row (via the descendant
  // modifier — the title is a CHILD of the element carrying the class), and
  // the inline SVG icon, which inherits `stroke` from the container. Each
  // variant maps onto the semantic token that already means the same thing
  // everywhere else in the app; the border is toned to /45 so a 4px rule
  // doesn't out-shout the prose it introduces.
  alert: {
    base: 'relative my-4 border-l-4 p-4 md-blk',
    title: 'text-sm font-semibold flex items-center gap-2 mb-2 capitalize',
    icon: 'size-5',
    note: '[&>[data-alert-title]]:text-info stroke-info border-info/45',
    tip: '[&>[data-alert-title]]:text-success stroke-success border-success/45',
    warning: '[&>[data-alert-title]]:text-warning stroke-warning border-warning/45',
    caution: '[&>[data-alert-title]]:text-error stroke-error border-error/45',
    important: '[&>[data-alert-title]]:text-accent stroke-accent border-accent/45',
  },
  table: {
    // No outer border / rounded shell — definition comes from the header bg
    // plus the per-row separators below. `border-border-subtle` for the same
    // width-vs-color reason as `code.base`.
    base: 'md-blk overflow-visible max-w-full my-3 border-0 border-border-subtle rounded-none',
    // table-auto (not fixed): app.css `.markdown-body … table` already forces
    // table-layout:auto via higher specificity, so `table-fixed` here was a
    // silently-overridden no-op. Keep them in agreement — columns size to
    // content (see the table block + overflow-wrap note in app.css).
    table: 'w-full table-auto border-collapse min-w-0',
  },
  thead: { base: 'bg-surface-1 text-fg-muted' },
  tbody: { base: '' },
  tfoot: { base: 'bg-surface-1 border-t border-border-subtle' },
  tr: {
    base: 'not-last:border-b border-border-subtle border-b transition-colors hover:bg-surface-1/40',
  },
  // No 200-400px width clamp — chat tables are usually narrow, and clamped
  // cells push them past the column bounds.
  td: { base: 'px-3 py-1.5 text-[0.8125rem] min-w-0 max-w-none break-words text-fg' },
  th: {
    base: 'px-3 py-1.5 text-[0.8125rem] font-medium min-w-0 max-w-none break-words text-fg-muted text-left',
  },
  // Stacked text. The size is `em`, not a rem step: `<sup>`/`<sub>` appear in
  // prose (0.875rem), in table cells (0.8125rem) and inside headings, and a
  // fixed rem value renders a superscript LARGER than the text it annotates
  // in the smallest of those. The vertical-align is spelled out even though
  // it matches the UA default, so the pair reads as one decision and neither
  // half can drift. One `em` step only — nested stacking (`x^a^` inside a
  // `<sup>`) compounds, and two levels is already the practical limit.
  sup: { base: 'text-[0.75em] align-super' },
  sub: { base: 'text-[0.75em] align-sub' },
  hr: { base: 'md-blk border-border-subtle my-5' },
  strong: { base: 'font-semibold' },
  em: { base: 'italic' },
  // `~~strike~~`. De-emphasis here means the same thing `.markdown-body del`
  // means, so it uses the same tier.
  del: { base: 'text-fg-subtle' },
  // Mermaid: no white-card shell, so diagrams sit on the chat surface like
  // everything else. Background stays on the elevation tier, NOT on
  // `--code-block`: a diagram is not code and must not move when a code theme
  // changes. `border-border-subtle` for the width-vs-color reason above.
  //
  // The repeated `group` is the merge's literal output (both halves named it)
  // preserved so the flattening changed no class attribute at all. It is a
  // marker class: the duplicate is inert, and dropping one is a free cleanup
  // for whoever next edits this entry.
  mermaid: {
    base: 'group md-blk group relative my-3 h-auto rounded-md border-0 border-border-subtle bg-surface-1 overflow-hidden items-center min-h-[300px]',
    buttons: 'absolute right-1 top-1 flex h-fit w-fit items-center gap-1',
  },
  math: { block: 'md-blk', inline: '' },
  // Footnote reference chip (`[^1]`). The chip is UI chrome rather than
  // prose, so it takes the app's smallest named text step instead of
  // inheriting the body size — which also pins its line box (`text-xs`
  // carries its own line-height) so a chip cannot grow the 1.65 prose line
  // it sits in. Clicking it opens the definition body: the chip publishes
  // its label on `data-footnote-label` and `chat/FootnotePopoverHost.svelte`
  // resolves and shows the definition.
  // A footnote reference presents like one: superscript, link-colored,
  // with a hover affordance — the muted inline pill it replaced read as
  // plain prose and nothing said "clickable" (2026-08-30).
  footnoteRef: {
    base: 'align-super text-[0.7em] leading-none px-0.5 font-medium text-md-link cursor-pointer hover:underline',
  },
  // Definition lists: the term is the focal text and the detail is body copy,
  // exactly the fg/fg-muted split.
  descriptionList: { base: 'my-4 space-y-2 md-blk' },
  descriptionTerm: { base: 'font-semibold border-l-2 pl-4 text-fg border-border-subtle' },
  descriptionDetail: { base: 'ml-4 leading-relaxed text-fg-muted' },
  // The one header button left: mermaid's expand control.
  components: {
    button:
      'disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer p-1 text-fg-hint transition-colors hover:text-fg hover:bg-surface-2/60 rounded w-6 h-6 flex items-center justify-center',
  },
};
