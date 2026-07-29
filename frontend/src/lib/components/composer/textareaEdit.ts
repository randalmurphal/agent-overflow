/**
 * Replace `[start, end)` of a textarea with `text`, as if the user typed it.
 *
 * `document.execCommand('insertText')` is the only way to put text into a
 * textarea and keep it in the browser's native undo stack, which is why every
 * composer completion (mentions, slash commands) goes through it: a completion
 * the user cannot Ctrl+Z is a completion they have to delete by hand.
 *
 * It is also deprecated and absent in some environments (test DOMs, and any
 * future engine that drops it), so this falls back to a direct value write
 * plus a synthetic `input` event — the same event the composer's own handler
 * listens for, so the draft store updates either way. Only the undo stack is
 * lost on the fallback path; the text always lands.
 */
export function replaceTextareaRange(
  textarea: HTMLTextAreaElement,
  start: number,
  end: number,
  text: string,
): void {
  // preventScroll: DOM focus must never scroll the pane strip (see
  // paneComposerFocus.ts) — mouse-selecting a completion holds focus on the
  // clicked button, and reclaiming it must not nudge a partially-visible pane.
  textarea.focus({ preventScroll: true });
  textarea.setSelectionRange(start, end);
  if (typeof document.execCommand === 'function' && document.execCommand('insertText', false, text)) {
    return;
  }
  const value = textarea.value;
  textarea.value = value.slice(0, start) + text + value.slice(end);
  const caret = start + text.length;
  textarea.setSelectionRange(caret, caret);
  textarea.dispatchEvent(new Event('input', { bubbles: true }));
}
