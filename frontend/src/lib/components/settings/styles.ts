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
  ' min-w-[8rem] text-[12px] px-2.5 py-1 cursor-pointer';

export const INPUT_CLASS =
  CONTROL_BASE + ' w-full text-[12px] px-2.5 py-1.5';

export const NUMBER_CLASS =
  CONTROL_BASE + ' w-14 text-[12px] px-2 py-1 text-right tabular-nums';

export const PRIMARY_BUTTON_CLASS =
  'rounded-[var(--radius-field)] bg-accent px-3 py-1.5 text-[12px] font-medium ' +
  'text-accent-foreground hover:brightness-105 disabled:opacity-50 ' +
  'disabled:cursor-not-allowed cursor-pointer transition focus:outline-none ' +
  'focus-visible:ring-2 focus-visible:ring-accent/50';

export const SECONDARY_BUTTON_CLASS =
  'rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 ' +
  'px-3 py-1.5 text-[12px] font-medium text-fg hover:border-accent/40 ' +
  'disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer transition-colors ' +
  'focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';

export const GHOST_BUTTON_CLASS =
  'rounded-[var(--radius-field)] px-2 py-1 text-[11.5px] text-fg-hint ' +
  'hover:text-fg-muted hover:bg-surface-2/40 cursor-pointer transition-colors ' +
  'focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';
