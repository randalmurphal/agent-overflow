// Back navigation lets existing Escape handlers dismiss their surface, but
// must never execute a keyboard shortcut (Escape may interrupt a turn).
const dismissalEvents = new WeakSet<Event>();

export function createSurfaceDismissalEvent(): KeyboardEvent {
  const event = new KeyboardEvent('keydown', {
    key: 'Escape', code: 'Escape', bubbles: true, cancelable: true,
  });
  dismissalEvents.add(event);
  return event;
}

export function isSurfaceDismissalEvent(event: Event): boolean {
  return dismissalEvents.has(event);
}
