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
  /**
   * The trigger's click. A mouse click opens (hover already did; this is
   * the keyboard/assistive path). A touch tap toggles: the first tap
   * pinned the card open, and there is no hover-away to close it, so the
   * second tap on the same ring must.
   */
  click(): void;
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
  // Whether the card was already up when the current press began. A touch
  // tap's compatibility mouseenter and focus open the card BEFORE its
  // click arrives, so the click cannot read `show` to tell a first tap
  // from a second one.
  let openAtPress = false;

  onDestroy(() => {
    if (closeTimer !== null) window.clearTimeout(closeTimer);
  });

  // Methods are passed unbound as event handlers, so they close over
  // module state rather than reading `this`.
  function open(): void {
    if (closeTimer !== null) {
      window.clearTimeout(closeTimer);
      closeTimer = null;
    }
    onOpen?.();
    show = true;
  }

  return {
    get show() {
      return show;
    },
    set show(value: boolean) {
      show = value;
      if (!value) pinned = false;
    },
    pointerDown(event: PointerEvent): void {
      // Touch/pen synthesize hover and blur as the card covers the trigger.
      // Keep a tapped meter open until its Popover receives an actual dismiss.
      pinned = event.pointerType === 'touch' || event.pointerType === 'pen';
      openAtPress = show;
    },
    open,
    click(): void {
      if (pinned && openAtPress) {
        show = false;
        pinned = false;
        openAtPress = false;
        return;
      }
      open();
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
