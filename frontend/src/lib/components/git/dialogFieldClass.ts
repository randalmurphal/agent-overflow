// Text field chrome shared by the git dialogs (Commit, Create PR/MR) so
// their inputs and textareas read as one family.
export const FIELD_CLASS =
  'w-full text-[0.8125rem] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 ' +
  'text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 ' +
  'transition-colors';

// The small bordered action that sits on a field's label row (Generate,
// Select all).
export const FIELD_ACTION_CLASS =
  'text-[0.625rem] px-2 py-0.5 rounded border border-border-subtle text-fg-muted hover:text-accent hover:border-accent/40 ' +
  'cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed transition-colors ' +
  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50';
