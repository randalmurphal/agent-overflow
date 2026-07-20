import { onDestroy } from 'svelte';

// Hover-intent open/close state shared by the header meter popovers
// (RateLimitMeter, ContextWindowMeter). The meters sit side by side,
// so their open/close feel must stay identical — one timer shape, one
// delay. The close delay bridges the pointer gap between the button
// and the floating popover so moving onto the popover doesn't dismiss
// it.
const CLOSE_DELAY_MS = 140;

export interface HoverPopover {
  /** Reactive open state; assign false to close immediately. */
  show: boolean;
  open(): void;
  scheduleClose(): void;
}

// Call during component init (uses onDestroy for timer cleanup).
// `onOpen` runs on every open — including re-entry while already open,
// so RateLimitMeter can recompute its countdown text there and the
// displayed value is fresh on each hover.
export function useHoverPopover(onOpen?: () => void): HoverPopover {
  let show = $state(false);
  let closeTimer: number | null = null;

  onDestroy(() => {
    if (closeTimer !== null) window.clearTimeout(closeTimer);
  });

  return {
    get show() {
      return show;
    },
    set show(value: boolean) {
      show = value;
    },
    open(): void {
      if (closeTimer !== null) {
        window.clearTimeout(closeTimer);
        closeTimer = null;
      }
      onOpen?.();
      show = true;
    },
    scheduleClose(): void {
      if (closeTimer !== null) window.clearTimeout(closeTimer);
      closeTimer = window.setTimeout(() => {
        show = false;
        closeTimer = null;
      }, CLOSE_DELAY_MS);
    },
  };
}
