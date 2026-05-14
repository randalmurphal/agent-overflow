// Theme override for `<Streamdown>` in ChatMarkdown.
//
// `svelte-streamdown`'s built-in `tailwind` baseTheme hardcodes light
// colors (`bg-gray-100`, `border-gray-200`, etc.) and chunky cards
// (rounded-xl + visible borders + heavy padding) that fight our
// surface tokens and read as dated callouts inside the chat. We pass
// this override via the `theme` prop; with `mergeTheme=true` the
// library's `mergeTheme` helper runs `tailwind-merge` (`cn`) on each
// class string, so our values supersede the conflicting bases — i.e.
// `border-0` cancels `border border-gray-200`, our `bg-surface-1`
// replaces `bg-gray-100`, etc.
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

// `DeepPartialTheme` is declared inside `svelte-streamdown/dist/theme`
// but not re-exported at the package root (only `Theme` is). Inline a
// structural type so we don't depend on the internal path.
type ThemeOverride = Record<string, Record<string, string>>;

// `svelte-streamdown` bundles 28 shiki grammars by default. `diff`
// is not one of them — without this addition, fenced ` ```diff `
// blocks emitted by agents fall through to plaintext and the
// `+`/`-` prefixes render unhighlighted. Adding a single
// `LanguageInfo` entry here registers the grammar with the
// library's HighlighterManager, lazy-loaded on first use.
//
// Keep this list short — every entry pays a per-thread shiki
// dynamic import the first time the language is seen. Add one
// only when an agent regularly emits a fenced block in that
// language and the fallback (plaintext) is genuinely worse.
export const extraShikiLanguages = [
  {
    id: 'diff',
    aliases: ['patch'],
    import: () => import('@shikijs/langs/diff'),
  },
];

export const chatMarkdownTheme: ThemeOverride = {
  code: {
    // Drop the border + rounded-xl card; use a single rounded-md
    // wrapper with no border. Background contrast (surface-1 vs the
    // page's surface-0) is what now defines the block.
    base: 'my-3 w-full overflow-hidden rounded-md border-0 flex flex-col bg-surface-1',
    // Same background as base — the inner container is mostly there
    // for the relative positioning the library expects.
    container: 'relative overflow-visible bg-transparent p-0 font-mono text-[13px]',
    // The whole header bar (language label + copy/download buttons)
    // is hidden — chat surfaces favour zero-chrome code blocks. A
    // hover-revealed copy button mounted by `StreamdownCodeHost` is
    // the only chrome we keep. To bring the language label back as
    // a small hover badge, swap this to a tiny absolute-positioned
    // pill — see the discussion in chat 2026-05-02.
    header: 'hidden',
    buttons: 'hidden',
    language: 'hidden',
    skeleton: 'block rounded-md font-mono text-transparent bg-surface-2/60 scale-y-90 w-fit animate-pulse whitespace-nowrap',
    // The pre/code body. Transparent bg so it inherits the wrapper's
    // surface-1; tightened horizontal padding.
    pre: 'overflow-x-auto font-mono p-3 bg-transparent',
    line: 'block',
  },
  // Inline code. `app.css` already styles `.markdown-body code` via
  // `--code-inline-bg`, so we just clear Streamdown's hardcoded gray
  // bg and let the cascade win. Layout stays here because it is
  // specific to Streamdown codespans: keep flags like `--validate`
  // atomic, but cap long inline code at the message width.
  codespan: {
    base: [
      'inline-block max-w-full overflow-x-auto whitespace-nowrap align-baseline',
      'rounded px-1.5 py-0.5 font-mono text-[0.9em] leading-[1.35] bg-transparent',
    ].join(' '),
  },
  table: {
    // No outer border / rounded shell — definition comes from the
    // header bg + per-row separators below.
    base: 'overflow-x-auto max-w-full my-3 border-0 rounded-none',
    table: 'w-full border-collapse min-w-full',
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
    base: 'px-3 py-1.5 text-[13px] min-w-0 max-w-none break-words text-fg',
  },
  th: {
    base: 'px-3 py-1.5 text-[13px] font-medium min-w-0 max-w-none break-words text-fg-muted text-left',
  },
  hr: {
    base: 'border-border-subtle my-5',
  },
  blockquote: {
    base: 'border-border-subtle text-fg-muted my-3 border-l-2 pl-3 italic',
  },
  // Mermaid: strip the white-card shell so diagrams sit on the chat
  // surface like everything else. The Streamdown component handles
  // its own panzoom buttons; the wrapper just needs to be a frame.
  mermaid: {
    base: 'group relative my-3 h-auto rounded-md border-0 bg-surface-1 overflow-hidden items-center min-h-[300px]',
    icon: 'size-4',
    buttons: 'absolute right-1 top-1 flex h-fit w-fit items-center gap-1',
  },
  // Header buttons (copy / download / panzoom). Streamdown's default
  // uses `text-gray-600 hover:bg-gray-100` which doesn't exist in
  // our theme; remap to our text + surface-2 hover.
  components: {
    button: 'disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer p-1 text-fg-hint transition-colors hover:text-fg hover:bg-surface-2/60 rounded w-6 h-6 flex items-center justify-center',
    popover: 'min-w-[250px] max-w-md fixed z-[1000] max-h-md overflow-y-auto rounded-md bg-surface-1 border border-border-subtle p-2 shadow-menu',
  },
  // Links: drop the hardcoded `text-blue-600`, use our accent token
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
  ul: { base: 'ml-4 list-outside list-disc whitespace-normal' },
  ol: { base: 'ml-4 list-outside whitespace-normal' },
  li: { base: 'py-0.5', checkbox: 'mr-2' },
};
