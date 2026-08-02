// IME composition guard.
//
// While a CJK / Japanese / Korean IME is composing, Enter means "confirm the
// candidate the IME is showing" — it is not a submit or select gesture, and
// the composed text is not in the input's value yet. Every handler that turns
// Enter into an action (send a message, insert the highlighted completion,
// open the highlighted row, commit a rename) has to yield while this holds,
// or the action fires on half-typed text and eats the keystroke the IME
// needed.
//
// The mirror rule is just as important: this must NOT be used to swallow keys
// the IME does not own. Escape and the arrows still belong to the surface —
// only the activation gestures are at issue.

/**
 * Structural shape rather than `KeyboardEvent` so callers can pass a plain
 * object (and tests a synthesized one) without constructing a DOM event.
 * Every real `KeyboardEvent` satisfies it.
 */
export type ImeComposableEvent = Pick<KeyboardEvent, 'isComposing' | 'keyCode'>;

/**
 * True when the keystroke belongs to an in-flight IME composition.
 *
 * `isComposing` is the spec signal. The legacy `keyCode === 229` fallback is
 * kept because WebKit (and some Android IMEs) deliver the composition's final
 * keydown AFTER `compositionend`, with `isComposing` already false but the
 * key code still the IME sentinel — the exact keystroke that would otherwise
 * fire an action on text the user was still composing. No real key press
 * reports 229, so the fallback cannot produce a false positive.
 */
export function isImeComposingEvent(event: ImeComposableEvent): boolean {
  return event.isComposing || event.keyCode === 229;
}
