// The `Button` primitive's look: variant, size (with the compact hit
// target) and pressed treatment. `Button.svelte` renders it on a native
// button; a control that cannot be a native button, such as an HTML drag
// source (a `<button>` does not start drags in Firefox), takes these
// classes on a `role="button"` element instead of copying them.

export type ButtonVariant =
  | 'primary'
  | 'secondary'
  | 'ghost'
  | 'tinted'
  | 'danger'
  | 'danger-outline'
  | 'danger-ghost';
export type ButtonSize = 'xs' | 'sm' | 'md';

const BASE = [
  'inline-flex items-center justify-center rounded-[var(--radius-control)]',
  'font-medium cursor-pointer select-none whitespace-nowrap',
  'transition-[color,background-color,border-color,opacity,transform] duration-150',
  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0',
  'disabled:cursor-not-allowed disabled:opacity-50',
  'active:scale-[0.98]',
].join(' ');

const SIZE: Record<ButtonSize, string> = {
  xs: 'h-6 compact:h-9 px-2 compact:px-2.5 text-[0.6875rem] gap-1',
  sm: 'h-7 compact:h-9 px-2.5 text-xs gap-1.5',
  md: 'h-8 compact:h-10 px-3 text-sm gap-2',
};

const VARIANT: Record<ButtonVariant, string> = {
  primary:
    'bg-accent text-surface-0 hover:opacity-90 active:opacity-80 ' +
    'focus-visible:ring-accent/40',
  secondary:
    'border border-border-subtle bg-transparent text-fg-muted ' +
    'hover:text-fg hover:border-border hover:bg-surface-2/40 ' +
    'focus-visible:ring-accent/40',
  ghost:
    'bg-transparent text-fg-muted hover:text-fg hover:bg-surface-2/40 ' +
    'focus-visible:ring-accent/30',
  tinted:
    'bg-accent/15 text-accent hover:bg-accent/25 active:bg-accent/30 ' +
    'focus-visible:ring-accent/40',
  danger:
    'bg-error text-surface-0 hover:opacity-90 active:opacity-80 ' +
    'focus-visible:ring-error/40',
  'danger-outline':
    'border border-error/40 bg-transparent text-error/90 ' +
    'hover:text-error hover:border-error/70 hover:bg-error/10 ' +
    'focus-visible:ring-error/40',
  'danger-ghost':
    'bg-transparent text-error/80 hover:text-error hover:bg-error/10 ' +
    'focus-visible:ring-error/40',
};

// Pressed-state styling is shared across every variant. The subtle
// inset ring + surface tint reads as "currently engaged" without
// overriding the variant's rest color identity.
const PRESSED = 'bg-surface-2/60 text-fg ring-1 ring-inset ring-border-subtle';

export interface ButtonClassOptions {
  variant?: ButtonVariant;
  size?: ButtonSize;
  pressed?: boolean;
  class?: string;
}

export function buttonClasses({ variant = 'secondary', size = 'sm', pressed = false, class: extra = '' }: ButtonClassOptions = {}): string {
  // Pressed takes precedence so the toggle-state visual isn't fighting
  // the variant's rest palette.
  return [BASE, SIZE[size], pressed ? PRESSED : VARIANT[variant], extra].join(' ');
}
