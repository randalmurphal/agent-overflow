/**
 * "Was the modifier held?" for a mouse gesture, in one place.
 *
 * Cmd on macOS, Ctrl everywhere else, and both accepted everywhere: a
 * browser's own "open in a new tab" reads the pair the same way, so a
 * gesture that works on one keyboard layout works on the other. Every
 * surface that widens a click into "somewhere else" asks this — opening a
 * project's new thread in a second pane, and opening an external link in
 * the thread's companion browser — so the two cannot drift into
 * disagreeing about what the gesture is.
 *
 * Not a keybinding: `stores/keybindingParser.ts` owns chords, which are
 * user-rebindable and platform-normalised. This is a MOUSE modifier, has
 * no chord to bind, and is read inside a click handler.
 */
export function isModClick(event: MouseEvent | KeyboardEvent): boolean {
  return event.metaKey || event.ctrlKey;
}
