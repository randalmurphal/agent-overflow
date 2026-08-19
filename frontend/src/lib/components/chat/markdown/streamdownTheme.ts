// Theme override for `<Streamdown>` in ChatMarkdown.
//
// `svelte-streamdown`'s built-in `tailwind` baseTheme hardcodes light
// colors (gray-100 backgrounds, gray-200 borders, etc.) and chunky
// cards (rounded-xl + visible borders + heavy padding) that fight our
// surface tokens and read as dated callouts inside the chat. We pass
// this override via the `theme` prop; with `mergeTheme=true` the
// library's `mergeTheme` helper runs `tailwind-merge` (`cn`) on each
// class string, so our values supersede the conflicting bases — our
// `bg-surface-1` replaces the vendor's gray-100 background, and so on.
//
// NOTE: do not write a vendor class VERBATIM in these comments.
// Tailwind scans comments too, so quoting one compiles a real (dead)
// rule plus its `--color-*` entry into the bundle — the exact palette
// leak this file exists to close. Name the scale in prose instead.
//
// Design intent — "sleek but defined":
//   - No outer borders on code blocks, tables, or mermaid wrappers.
//     Definition comes from a single elevation step (surface-0 page
//     bg → surface-1 block bg) and a thin internal divider for
//     header / row separation.
//   - Tighter padding — code header is one-line chrome, table cells
//     drop the 200-400px width clamps.
//   - Tokens come from `app.css` `@theme` (`bg-surface-1`,
//     `text-fg-muted`, `border-border-subtle`, etc.) so light/dark
//     theme switching keeps working without a second copy of values.

// Cancelling a vendor color requires COLLIDING with it, not merely
// naming the same element. `tailwind-merge` groups by (modifier,
// utility category): the vendor's blue-600 text class and `text-info`
// collide, but the SAME class carried under an
// `[&>[data-alert-title]]:` modifier and a bare `text-info` do NOT —
// the modifier is part of the key, so both survive and the vendor
// palette still paints. Every override below is written in the same
// shape the vendor key uses (see `vendor/svelte-streamdown/dist/
// theme.js`); the alert entries in particular keep that modifier on
// their text color for exactly this reason.

// `DeepPartialTheme` is declared inside `svelte-streamdown/dist/theme`
// but not re-exported at the package root (only `Theme` is). Inline a
// structural type so we don't depend on the internal path.
type ThemeOverride = Record<string, Record<string, string>>;

export const chatMarkdownTheme: ThemeOverride = {
  code: {
    // Drop the border + rounded-xl card; use a single rounded-md
    // wrapper with no border. Background contrast (the code-block
    // ground vs the page's surface-0) is what now defines the block.
    // These three entries are consumed by `StreamdownCodeHost` (which
    // renders the pre/code DOM itself from backend spans); the
    // library's header / skeleton / line-wrapper classes have no
    // consumer, so chat surfaces stay zero-chrome — the host's
    // hover-revealed copy button is the only affordance.
    //
    // `bg-code-block` rather than `bg-surface-1`: the code-block ground
    // is its own role (it travels with a code theme), even though the
    // token currently aliases the surface-1 elevation tier. Keep in
    // step with `.markdown-body pre`'s `--code-block` in app.css.
    //
    // `border-border-subtle` alongside `border-0`: tailwind-merge treats
    // border WIDTH and border COLOR as different categories, so
    // `border-0` alone leaves the vendor's gray-200 border color
    // standing. Inert at zero width today, but one `border` away from
    // painting a light-mode gray in dark mode.
    base: 'my-3 w-full overflow-hidden rounded-md border-0 border-border-subtle flex flex-col bg-code-block',
    // Same background as base — the inner container is mostly there
    // for the relative positioning the host's copy overlay expects.
    container: 'relative overflow-visible bg-transparent p-0 font-mono text-[0.8125rem]',
    // The pre/code body. Transparent bg so it inherits the wrapper's
    // surface-1; tightened horizontal padding.
    pre: 'whitespace-pre-wrap wrap-anywhere overflow-x-visible font-mono p-3 bg-transparent',
  },
  // Inline code. `app.css` already styles `.markdown-body code` via
  // `--code-inline-bg`, so we just clear Streamdown's hardcoded gray
  // bg and let the cascade win. Layout stays here because it is
  // specific to Streamdown codespans: they participate in normal
  // inline layout, and long paths / hashes may break under constraint.
  codespan: {
    base: [
      'inline whitespace-pre-wrap wrap-anywhere align-baseline',
      'rounded px-1.5 py-0.5 font-mono text-[0.9em] leading-[1.35] bg-transparent',
    ].join(' '),
  },
  table: {
    // No outer border / rounded shell — definition comes from the
    // header bg + per-row separators below.
    //
    // `border-border-subtle` for the same width-vs-color reason as
    // `code.base` and `mermaid.base`: the vendor sets a full border here,
    // and `border-0` cancels only the WIDTH. Without a colliding border
    // COLOR the vendor's light-mode gray survived the merge — inert at
    // zero width, one `border` away from painting.
    base: 'overflow-visible max-w-full my-3 border-0 border-border-subtle rounded-none',
    // table-auto (not fixed): app.css `.markdown-body … table` already forces
    // table-layout:auto via higher specificity, so `table-fixed` here was a
    // silently-overridden no-op. Keep them in agreement — columns size to
    // content (see the table block + overflow-wrap note in app.css).
    table: 'w-full table-auto border-collapse min-w-0',
  },
  thead: {
    base: 'bg-surface-1 text-fg-muted',
  },
  tbody: {
    base: '',
  },
  tfoot: {
    base: 'bg-surface-1 border-t border-border-subtle',
  },
  tr: {
    base: 'border-border-subtle border-b transition-colors hover:bg-surface-1/40',
  },
  td: {
    // Drop the 200-400px width clamp — chat tables are usually
    // narrow, and clamped cells push them past the column bounds.
    base: 'px-3 py-1.5 text-[0.8125rem] min-w-0 max-w-none break-words text-fg',
  },
  th: {
    base: 'px-3 py-1.5 text-[0.8125rem] font-medium min-w-0 max-w-none break-words text-fg-muted text-left',
  },
  hr: {
    base: 'border-border-subtle my-5',
  },
  blockquote: {
    base: 'border-border-subtle text-fg-muted my-3 border-l-2 pl-3 italic',
  },
  // GFM alerts (`> [!NOTE]` … ). Each vendor key carries a full literal
  // palette for its variant — blue-600 for note, green-600 for tip,
  // and so on, as a text class under an `[&>[data-alert-title]]:`
  // modifier plus a bare border and stroke class — landing on the alert
  // container (left border), its `[data-alert-title]` row, and the
  // inline SVG icon, which inherits `stroke` from the container.
  // Map each variant onto the semantic token that already means the
  // same thing everywhere else in the app; the border is toned down to
  // /45 so a 4px rule doesn't out-shout the prose it introduces.
  alert: {
    note: '[&>[data-alert-title]]:text-info stroke-info border-info/45',
    tip: '[&>[data-alert-title]]:text-success stroke-success border-success/45',
    warning: '[&>[data-alert-title]]:text-warning stroke-warning border-warning/45',
    caution: '[&>[data-alert-title]]:text-error stroke-error border-error/45',
    important: '[&>[data-alert-title]]:text-accent stroke-accent border-accent/45',
  },
  // Mermaid: strip the white-card shell so diagrams sit on the chat
  // surface like everything else. The Streamdown component handles
  // its own panzoom buttons; the wrapper just needs to be a frame.
  mermaid: {
    // `border-border-subtle` for the same width-vs-color reason as
    // `code.base` above. Background stays on the elevation tier, NOT on
    // `--code-block`: a diagram is not code and must not move when a
    // code theme changes.
    base: 'group relative my-3 h-auto rounded-md border-0 border-border-subtle bg-surface-1 overflow-hidden items-center min-h-[300px]',
    icon: 'size-4',
    buttons: 'absolute right-1 top-1 flex h-fit w-fit items-center gap-1',
  },
  // Header buttons (copy / download / panzoom). Streamdown's default
  // uses gray-600 text with a gray-100 hover fill, neither of which
  // exists in our theme; remap to our text + surface-2 hover.
  components: {
    button: 'disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer p-1 text-fg-hint transition-colors hover:text-fg hover:bg-surface-2/60 rounded w-6 h-6 flex items-center justify-center',
    popover: 'min-w-[250px] max-w-md fixed z-[1000] max-h-md overflow-y-auto rounded-md bg-surface-1 border border-border-subtle p-2 shadow-menu',
  },
  // Links: drop the hardcoded blue-600 text, use our accent token
  // so light/dark theme stays consistent.
  link: {
    base: 'text-accent font-medium underline wrap-anywhere hover:text-accent/80',
    blocked: 'text-fg-hint',
  },
  // Headings inside markdown lose Streamdown's default `mt-6 mb-2
  // text-3xl` — chat surfaces want compact heading sizing that
  // matches `app.css` `.markdown-body` rules. Clearing each entry
  // hands control back to the cascade.
  h1: { base: 'mt-3 mb-1 text-lg font-semibold' },
  h2: { base: 'mt-3 mb-1 text-base font-semibold' },
  h3: { base: 'mt-2 mb-1 text-[0.9375rem] font-semibold' },
  h4: { base: 'mt-2 mb-1 text-sm font-semibold' },
  h5: { base: 'mt-2 mb-1 text-sm font-semibold' },
  h6: { base: 'mt-2 mb-1 text-sm font-semibold' },
  // pl matches `.markdown-body ul/ol` in app.css (which wins the
  // cascade): 2em of marker room so wide fonts don't clip — see the
  // comment there.
  ul: { base: 'ml-0 pl-[2em] list-outside list-disc whitespace-normal' },
  ol: { base: 'ml-0 pl-[2em] list-outside whitespace-normal' },
  li: { base: 'py-0.5', checkbox: 'mr-2' },
  // `~~strike~~`. Vendor ships gray-600 text; de-emphasis here means
  // the same thing `.markdown-body del` means, so use the same tier.
  del: { base: 'text-fg-subtle' },
  // Footnote reference chip (`[^1]`). Vendor: gray-600 text on a
  // gray-100/80 fill — a light-mode chip that stayed light in dark mode.
  footnoteRef: { base: 'text-fg-muted bg-surface-1/80' },
  // Definition lists. Vendor: gray-900 text with a gray-200 border on
  // the term, gray-700 text on the detail — the term is the focal text
  // and the detail is body copy, exactly the fg/fg-muted split.
  descriptionTerm: { base: 'text-fg border-border-subtle' },
  descriptionDetail: { base: 'text-fg-muted' },
};
