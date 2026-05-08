// Shared class string for flat composer trigger buttons. The model
// picker (toolbar/ModelProviderTrigger), branch picker, and env picker
// all sit on the composer card and share the same flat-toolbar shape;
// keeping the visual contract in one place means a hover/focus/disabled
// tweak only has to land in one file to stay consistent across all
// three triggers.
export const composerTriggerClasses = [
  'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
  'px-1.5 py-1 text-[11px] text-fg-muted',
  'transition-colors cursor-pointer',
  'hover:text-fg hover:bg-surface-2/30',
  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
  'disabled:opacity-60 disabled:cursor-not-allowed',
].join(' ');
