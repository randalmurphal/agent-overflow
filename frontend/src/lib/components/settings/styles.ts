// Shared Tailwind class constants for the settings panes. Centralized so
// every section pulls the same controls and the visual rhythm doesn't drift
// between sections. Values come from the design tokens declared in
// styles/tokens.css and app.css's @theme block.

export const CONTROL_BASE =
  'rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 ' +
  'text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent ' +
  'focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors';

export const SELECT_CLASS =
  CONTROL_BASE +
  ' min-w-[8rem] text-[0.75rem] px-2.5 py-1 cursor-pointer';

export const INPUT_CLASS =
  CONTROL_BASE + ' w-full text-[0.75rem] px-2.5 py-1.5';

export const NUMBER_CLASS =
  CONTROL_BASE + ' w-14 text-[0.75rem] px-2 py-1 text-right tabular-nums';

export const PRIMARY_BUTTON_CLASS =
  'rounded-[var(--radius-field)] bg-accent px-3 py-1.5 text-[0.75rem] font-medium ' +
  'text-accent-fg hover:brightness-105 disabled:opacity-50 ' +
  'disabled:cursor-not-allowed cursor-pointer transition focus:outline-none ' +
  'focus-visible:ring-2 focus-visible:ring-accent/50';

export const SECONDARY_BUTTON_CLASS =
  'rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 ' +
  'px-3 py-1.5 text-[0.75rem] font-medium text-fg hover:border-accent/40 ' +
  'disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer transition-colors ' +
  'focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';

export const DANGER_BUTTON_CLASS =
  'rounded-[var(--radius-field)] border border-error/40 bg-error/10 ' +
  'px-3 py-1.5 text-[0.75rem] font-medium text-error hover:bg-error/15 ' +
  'disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer transition-colors ' +
  'focus:outline-none focus-visible:ring-2 focus-visible:ring-error/40';

// Explanatory prose under a section title. SettingsHeader renders its
// `description` with it; sections whose explanation needs inline markup
// (a <code> span) render the same voice through the header's `details`
// snippet, so one class defines what "section prose" looks like.
export const SECTION_PROSE_CLASS =
  'max-w-2xl text-[0.71875rem] leading-snug text-fg-muted';

// Model chips. Two sections render them with opposite polarity — Providers
// as a hide-list, Prompts & Tools as a selection — so the vocabulary lives
// here rather than being copied between them. It was copied once, and the
// two had already drifted (the empty-state prose) by the time the second
// section shipped.
export const CHIP_BASE_CLASS =
  'rounded-[var(--radius-field)] border px-2 py-0.5 text-[0.6875rem] ' +
  'transition-colors cursor-pointer focus:outline-none focus-visible:ring-2 ' +
  'focus-visible:ring-accent/40';

/** Resting chip: on, visible, unselected — whatever the section's neutral is. */
export const CHIP_RESTING_CLASS =
  'border-border-subtle bg-surface-0 text-fg-muted hover:border-border';

/** Chip the user has picked. */
export const CHIP_SELECTED_CLASS = 'border-accent/50 bg-accent/15 text-fg';

/** Chip switched off — struck through rather than merely dimmed. */
export const CHIP_EXCLUDED_CLASS =
  'border-border-subtle/60 bg-surface-0/50 text-fg-hint line-through';

/** Prose shown in place of the chip row when there is nothing to render. */
export const CHIP_EMPTY_PROSE_CLASS = 'text-[0.75rem] text-fg-muted';

export const GHOST_BUTTON_CLASS =
  'rounded-[var(--radius-field)] px-2 py-1 text-[0.71875rem] text-fg-hint ' +
  'hover:text-fg-muted hover:bg-surface-2/40 cursor-pointer transition-colors ' +
  'focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';
