// Suppresses browser-default behaviors that don't fit an app shell:
// history navigation (Alt+arrows, mouse buttons 3/4) and the native
// context menu on surfaces where it would show browser chrome
// (Back / Forward / Reload / Open Frame…). The native menu stays
// available where it's genuinely useful — editable fields
// (cut/copy/paste/spellcheck) and right-clicks landing on selected
// text (copy) — because reimplementing those menus in JS is strictly
// worse: clipboard read is permission-gated in WebKitGTK and
// spellcheck suggestions are native-only.
//
// This guard is the single cross-platform source of truth for
// context-menu policy. Wails' DefaultContextMenuDisabled is
// deliberately NOT set on any window: it is Windows-only and would
// hard-disable the editable-field menus below this layer.

import { isMacPlatform } from './platform';

let installCount = 0;
let release: (() => void) | null = null;

export function installBrowserHistoryGuard(): () => void {
  if (typeof document === 'undefined') return () => {};
  if (installCount === 0) {
    release = install();
  }
  installCount += 1;
  return () => {
    installCount = Math.max(0, installCount - 1);
    if (installCount === 0) {
      release?.();
      release = null;
    }
  };
}

function install(): () => void {
  const options = { capture: true };
  document.addEventListener('keydown', preventHistoryKey, options);
  document.addEventListener('mousedown', preventHistoryMouseButton, options);
  document.addEventListener('mouseup', preventHistoryMouseButton, options);
  document.addEventListener('auxclick', preventHistoryMouseButton, options);
  document.addEventListener('contextmenu', preventBrowserContextMenu, options);
  return () => {
    document.removeEventListener('keydown', preventHistoryKey, options);
    document.removeEventListener('mousedown', preventHistoryMouseButton, options);
    document.removeEventListener('mouseup', preventHistoryMouseButton, options);
    document.removeEventListener('auxclick', preventHistoryMouseButton, options);
    document.removeEventListener('contextmenu', preventBrowserContextMenu, options);
  };
}

function preventHistoryKey(event: KeyboardEvent): void {
  if (event.defaultPrevented) return;
  if (!event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
  if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
  // On macOS, Alt(Option)+Arrow is never a history gesture — WebKit
  // binds history to Cmd-based keys — it is the native word-caret
  // movement (filled in for our fields by textEditingKeymap), so
  // consuming it would kill word navigation in every text field. On
  // Windows/Linux webviews Alt+Arrow IS history navigation, even with
  // a text caret focused, so it stays suppressed there (word ops use
  // Ctrl+Arrow on those platforms).
  if (isMacPlatform()) return;
  consume(event);
}

function preventHistoryMouseButton(event: MouseEvent): void {
  if (event.defaultPrevented) return;
  if (event.button !== 3 && event.button !== 4) return;
  consume(event);
}

function preventBrowserContextMenu(event: MouseEvent): void {
  if (allowsNativeContextMenu(event)) return;
  // preventDefault only — app-level oncontextmenu handlers (sidebar
  // rows, pane titles, …) still receive the event and open their own
  // custom menus.
  event.preventDefault();
}

// Input types with a text caret. The rest (checkbox, radio, range,
// color, file, button…) hit-test to the page menu, which we suppress.
const TEXT_INPUT_TYPES = new Set(['text', 'search', 'url', 'tel', 'email', 'password', 'number']);

function allowsNativeContextMenu(event: MouseEvent): boolean {
  const target = event.target;
  if (!(target instanceof Element)) return false;
  if (target instanceof HTMLTextAreaElement) return editableFieldAllows(target);
  if (target instanceof HTMLInputElement) {
    return TEXT_INPUT_TYPES.has(target.type) && editableFieldAllows(target);
  }
  if (target instanceof HTMLElement && target.isContentEditable) return true;
  return clickIsOnSelection(event);
}

// Writable fields always get the editing menu; read-only ones only
// when text is selected (Copy is the sole useful item there).
function editableFieldAllows(field: HTMLInputElement | HTMLTextAreaElement): boolean {
  if (field.disabled) return false;
  if (!field.readOnly) return true;
  return field.selectionStart != null && field.selectionStart !== field.selectionEnd;
}

// Allow the selection menu only when the click lands on the selected
// text itself. A right-click elsewhere in a partially-selected element
// would hit-test to the page menu and leak browser chrome.
function clickIsOnSelection(event: MouseEvent): boolean {
  const selection = window.getSelection();
  if (!selection || selection.isCollapsed) return false;
  for (let i = 0; i < selection.rangeCount; i++) {
    for (const rect of selection.getRangeAt(i).getClientRects()) {
      if (
        event.clientX >= rect.left &&
        event.clientX <= rect.right &&
        event.clientY >= rect.top &&
        event.clientY <= rect.bottom
      ) {
        return true;
      }
    }
  }
  return false;
}

function consume(event: Event): void {
  event.preventDefault();
  event.stopImmediatePropagation();
}
