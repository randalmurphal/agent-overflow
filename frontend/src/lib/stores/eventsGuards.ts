// Shared wire-payload validation guards for the events* domain modules.
// Every push handler admits data from the shared transport (the same
// WebSocket serves the embedded webview, `agent-overflow --connect`
// clients, and LAN browsers), so string fields are length-bounded and
// numeric fields finite-checked before a payload touches reactive
// state. Extracted from eventsItemStream.ts so eventsDiscussion.ts
// applies the identical guards to its channel payloads.

/**
 * Cap for unbounded text fields (item summaries/deltas/meta and
 * discussion message content). 2M chars is comfortably above the
 * largest single payload the item stream legitimately carries while
 * still bounding a hostile frame.
 */
export const ITEM_EVENT_TEXT_FIELD_MAX_CHARS = 2_000_000;

export function isFiniteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

export function isBoundedString(
  value: unknown,
  maxChars = ITEM_EVENT_TEXT_FIELD_MAX_CHARS,
): value is string {
  return typeof value === 'string' && value.length <= maxChars;
}
