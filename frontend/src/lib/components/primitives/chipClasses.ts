// The filter chip: a pill-shaped toggle or picker trigger in a filter
// row, on (pressed, or a picker holding a choice) or off. Sized to the
// compact hit target `Button` uses (36 px). A native button takes these
// classes rather than `Button` because a picker chip is a popover anchor.

const CHIP_BASE =
  'inline-flex h-6 shrink-0 cursor-pointer items-center gap-1.5 rounded-full border px-2.5 text-[0.6875rem] font-medium transition-colors ' +
  'compact:h-9 compact:px-3 compact:select-none focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';
const CHIP_ON = 'border-accent/45 bg-accent/12 text-fg';
const CHIP_OFF = 'border-border-subtle text-fg-muted hover:border-border hover:text-fg';

export function chipClasses(on: boolean): string {
  return `${CHIP_BASE} ${on ? CHIP_ON : CHIP_OFF}`;
}
