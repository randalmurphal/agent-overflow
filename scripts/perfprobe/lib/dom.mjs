// Stable production selectors shared by probes that census or drive scroll surfaces.
export const CHAT_SCROLLER_SELECTOR = '[data-testid="message-timeline-scroll"]';
export const SCROLL_SURFACE_SELECTOR = [
  CHAT_SCROLLER_SELECTOR,
  '[data-testid="channel-message-list"]',
  '[data-testid="activity-run-clip"]',
].join(',');
