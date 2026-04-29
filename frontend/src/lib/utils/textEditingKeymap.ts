// Cross-platform word-op keybindings for plain text inputs.
//
// Linux / Windows Chromium binds Ctrl+Backspace / Ctrl+Arrow for word ops
// but leaves the Alt+ variants unbound. macOS WebKit binds the Alt+
// variants but not the Ctrl+ ones. This dispatcher fills in both gaps so
// every <textarea> and text-style <input> in the app responds the same way
// regardless of OS or modifier muscle memory.
//
// Word semantics follow VS Code rather than browser-native textarea:
// each non-word, non-whitespace character (`;`, `'`, `=`, `.`, …) is its
// own step. Strictly better for the kind of text users enter into a chat
// box for a coding agent (paths, flags, identifiers) and matches the
// muscle memory of users coming from a code editor.
//
// Mutations route through `document.execCommand('delete')` so the native
// undo stack records each word-op as one step. Selection-only moves use
// `setSelectionRange` directly. The dispatcher is target-aware and bails
// out for non-text inputs, read-only / disabled fields, IME composition,
// and Cmd-modifier chords (which carry OS-specific line semantics we
// deliberately leave alone).

const WORD_CHAR = /[\p{L}\p{N}_]/u;
const WHITESPACE = /\s/;

function isWordChar(ch: string | undefined): boolean {
  return ch !== undefined && WORD_CHAR.test(ch);
}

function isWhitespace(ch: string | undefined): boolean {
  return ch !== undefined && WHITESPACE.test(ch);
}

/**
 * Index a single word-op step would land at when moving backward from
 * `pos`. The step removes exactly one of: a run of word chars (letters,
 * digits, underscore — Unicode-aware via `\p{L}\p{N}`), a single
 * non-word non-whitespace character, or a pure-whitespace tail. Any
 * leading whitespace adjacent to the cursor is folded into the step that
 * crosses it.
 */
export function wordBoundaryBackward(text: string, pos: number): number {
  if (pos <= 0) return 0;
  let i = pos;
  while (i > 0 && isWhitespace(text[i - 1])) i--;
  if (i === 0) return 0;
  if (isWordChar(text[i - 1])) {
    while (i > 0 && isWordChar(text[i - 1])) i--;
    return i;
  }
  // Single punctuation/symbol char — each is its own step.
  return i - 1;
}

/**
 * Mirror of {@link wordBoundaryBackward}: index a single word-op step
 * would land at when moving forward from `pos`.
 */
export function wordBoundaryForward(text: string, pos: number): number {
  if (pos >= text.length) return text.length;
  let i = pos;
  while (i < text.length && isWhitespace(text[i])) i++;
  if (i >= text.length) return text.length;
  if (isWordChar(text[i])) {
    while (i < text.length && isWordChar(text[i])) i++;
    return i;
  }
  return i + 1;
}

// `<input>` types whose value semantics are the same plain-text we expect
// in `<textarea>`. `number`, `date`, `range`, `checkbox`, etc. have their
// own native key handling and shouldn't be touched. Empty string is the
// HTML default for `<input>` with no `type` attribute.
const TEXT_INPUT_TYPES = new Set([
  '',
  'email',
  'password',
  'search',
  'tel',
  'text',
  'url',
]);

type EditableTarget = HTMLTextAreaElement | HTMLInputElement;

function editableTarget(target: EventTarget | null): EditableTarget | null {
  if (target instanceof HTMLTextAreaElement) {
    if (target.disabled || target.readOnly) return null;
    return target;
  }
  if (target instanceof HTMLInputElement) {
    if (target.disabled || target.readOnly) return null;
    if (!TEXT_INPUT_TYPES.has(target.type)) return null;
    return target;
  }
  return null;
}

function deleteRange(el: EditableTarget, start: number, end: number): void {
  if (start === end) return;
  el.setSelectionRange(start, end);
  document.execCommand('delete');
}

function moveCaret(
  el: EditableTarget,
  target: number,
  shift: boolean,
): void {
  const start = el.selectionStart ?? 0;
  const end = el.selectionEnd ?? start;
  if (!shift) {
    el.setSelectionRange(target, target);
    return;
  }
  // Shift-extend: keep the anchor (the side opposite the active end) and
  // move the active end to `target`. selectionDirection tells us which
  // side is active; default to 'forward' when the textarea has no
  // selection yet.
  const direction = el.selectionDirection ?? 'forward';
  const anchor = direction === 'backward' ? end : start;
  const lo = Math.min(anchor, target);
  const hi = Math.max(anchor, target);
  el.setSelectionRange(lo, hi, target < anchor ? 'backward' : 'forward');
}

/**
 * Try to handle a keydown as a word-op. Returns `true` if the event was
 * consumed (caller should `preventDefault` and stop further dispatch);
 * `false` to let the browser / app handle it natively.
 */
export function dispatchTextEditing(event: KeyboardEvent): boolean {
  if (event.isComposing) return false;
  if (event.metaKey) return false; // leave Cmd+ chords (macOS line ops) native
  const altOnly = event.altKey && !event.ctrlKey;
  const ctrlOnly = event.ctrlKey && !event.altKey;
  if (!altOnly && !ctrlOnly) return false;

  const el = editableTarget(event.target);
  if (!el) return false;

  const value = el.value;
  const start = el.selectionStart ?? value.length;
  const end = el.selectionEnd ?? start;

  switch (event.key) {
    case 'Backspace': {
      if (start !== end) {
        document.execCommand('delete');
        return true;
      }
      const target = wordBoundaryBackward(value, start);
      deleteRange(el, target, start);
      return true;
    }
    case 'Delete': {
      if (start !== end) {
        document.execCommand('delete');
        return true;
      }
      const target = wordBoundaryForward(value, end);
      deleteRange(el, end, target);
      return true;
    }
    case 'ArrowLeft': {
      const from = event.shiftKey
        ? (el.selectionDirection === 'backward' ? start : end)
        : start;
      const target = wordBoundaryBackward(value, from);
      moveCaret(el, target, event.shiftKey);
      return true;
    }
    case 'ArrowRight': {
      const from = event.shiftKey
        ? (el.selectionDirection === 'backward' ? start : end)
        : end;
      const target = wordBoundaryForward(value, from);
      moveCaret(el, target, event.shiftKey);
      return true;
    }
    default:
      return false;
  }
}
