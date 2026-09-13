// Class string for the chat header's workspace segments: the project
// crumb before the title on desktop, and the machine, project, branch and
// worktree segments of the compact header's facts line. One flat text
// button shape so the header reads as one line of facts, sized to the
// title beside it on desktop and to the composer's own chips under
// compact. Locked segments render disabled and read as labels.
//
// The rest color is the host's, because the worktree segment swaps it for
// the accent in a linked worktree and two same-property utilities in one
// string would race. Every other segment, on both platforms, rests at
// `text-text-secondary`: one visible tier under the title in every
// built-in theme. The muted tier is not enough; in Blacklight it is the
// same white as the primary, so the facts read as more title. Hover lifts
// to `text-fg` only while enabled, so a locked segment never changes.
export const headerSegmentClasses = [
  'inline-flex min-w-0 items-center gap-1 rounded-[var(--radius-field)]',
  'px-1 py-0.5 text-[0.8125rem] compact:text-[0.6875rem] whitespace-nowrap',
  'transition-colors cursor-pointer enabled:hover:text-fg enabled:hover:bg-surface-2/40',
  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
  'disabled:cursor-default',
].join(' ');

export const headerSegmentSeparatorClasses = 'shrink-0 select-none text-fg-hint/70';
