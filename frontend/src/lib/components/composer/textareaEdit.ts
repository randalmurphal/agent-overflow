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

/**
 * Change a textarea to `nextValue` with one native edit over only the range
 * that differs. A whole-value assignment resets a long textarea's internal
 * scroll position in Blink when its controlled value commits back through
 * Svelte. Keeping the unchanged prefix and suffix outside the edit preserves
 * that internal scroll state and records the mutation as one undo step.
 */
export function replaceTextareaValue(
  textarea: HTMLTextAreaElement,
  nextValue: string,
  caret: number,
): void {
  const currentValue = textarea.value;
  if (currentValue === nextValue) {
    textarea.focus({ preventScroll: true });
    textarea.setSelectionRange(caret, caret);
    return;
  }
  let start = 0;
  const sharedLength = Math.min(currentValue.length, nextValue.length);
  while (start < sharedLength && currentValue[start] === nextValue[start]) {
    start += 1;
  }

  let currentEnd = currentValue.length;
  let nextEnd = nextValue.length;
  while (
    currentEnd > start
    && nextEnd > start
    && currentValue[currentEnd - 1] === nextValue[nextEnd - 1]
  ) {
    currentEnd -= 1;
    nextEnd -= 1;
  }

  replaceTextareaRange(textarea, start, currentEnd, nextValue.slice(start, nextEnd));
  textarea.setSelectionRange(caret, caret);
}
