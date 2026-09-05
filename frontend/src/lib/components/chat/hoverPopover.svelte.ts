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
  pointerDown(event: PointerEvent): void;
  scheduleClose(): void;
}

// Call during component init (uses onDestroy for timer cleanup).
// `onOpen` runs on every open — including re-entry while already open,
// so RateLimitMeter can recompute its countdown text there and the
// displayed value is fresh on each hover.
export function useHoverPopover(onOpen?: () => void): HoverPopover {
  let show = $state(false);
  let closeTimer: number | null = null;
  let pinned = false;

  onDestroy(() => {
    if (closeTimer !== null) window.clearTimeout(closeTimer);
  });

  return {
    get show() {
      return show;
    },
    set show(value: boolean) {
      show = value;
      if (!value) pinned = false;
    },
    pointerDown(event: PointerEvent): void {
      // Touch/pen synthesize hover and blur as the sheet covers the trigger.
      // Keep a tapped meter open until its Popover receives an actual dismiss.
      pinned = event.pointerType === 'touch' || event.pointerType === 'pen';
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
      if (pinned) return;
      if (closeTimer !== null) window.clearTimeout(closeTimer);
      closeTimer = window.setTimeout(() => {
        show = false;
        closeTimer = null;
      }, CLOSE_DELAY_MS);
    },
  };
}
