// Helper: when a keybinding (pane.focusLeft, pane.focusRight, pane.close,
// thread.newPane) shifts focus from one pane to another while the user was
// typing in a composer, move DOM focus to the new pane's composer textarea so
// subsequent keystrokes land where the user expects. Without this, pressing
// alt+h from inside the right pane updates focusedPaneId but leaves the
// caret in the right pane's textarea — feeling like alt+h "did nothing"
// because the only feedback is the subtle pane background tint.

function isActiveElementEditable(): boolean {
  const active = typeof document !== 'undefined' ? document.activeElement : null;
  if (!active) return false;
  const tag = active.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return (active as HTMLElement).isContentEditable === true;
}

export function focusPaneComposerIfEditableActive(paneId: string): void {
  if (typeof document === 'undefined') return;
  if (!isActiveElementEditable()) return;
  focusPaneComposer(paneId);
}

/**
 * Move DOM focus to the named pane's composer textarea. Returns false
 * when the textarea is missing or disabled, so callers can fall back:
 * composer-toolbar pickers refocus their trigger button, and the
 * terminal-close path uses it to keep the caret off <body> after the
 * drawer unmounts.
 */
export function focusPaneComposer(paneId: string): boolean {
  const textarea = findPaneComposer(paneId);
  if (!textarea) return false;
  textarea.focus();
  return true;
}

function findPaneComposer(paneId: string): HTMLTextAreaElement | null {
  if (typeof document === 'undefined') return null;
  const escape = (window as unknown as { CSS?: { escape?: (s: string) => string } }).CSS?.escape;
  const selector = escape ? `[data-pane-id="${escape(paneId)}"]` : `[data-pane-id="${paneId}"]`;
  const root = document.querySelector(selector);
  if (!root) return null;
  const textarea = root.querySelector<HTMLTextAreaElement>(
    'textarea[aria-label="Message Input"]',
  );
  if (!textarea || textarea.disabled) return null;
  return textarea;
}
