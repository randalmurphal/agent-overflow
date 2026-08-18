function isPlainNavigation(event: KeyboardEvent): boolean {
  return !event.metaKey && !event.ctrlKey && !event.altKey && !event.shiftKey;
}

function isForwardKey(event: KeyboardEvent): boolean {
  return event.key === 'ArrowDown' || event.key === 'ArrowRight' || event.key === 'j' || event.key === 'l';
}

function isBackwardKey(event: KeyboardEvent): boolean {
  return event.key === 'ArrowUp' || event.key === 'ArrowLeft' || event.key === 'k' || event.key === 'h';
}

function actionButtons(container: HTMLElement | undefined): HTMLButtonElement[] {
  if (!container) return [];
  return Array.from(container.querySelectorAll<HTMLButtonElement>('button:not(:disabled)'));
}

// preventScroll everywhere here: the approval panels live in the composer,
// inside the horizontally-scrolled pane strip. The container focus fires on
// PROMPT ARRIVAL (a backend event), so a bare focus() would yank the strip
// to a pane the user had scrolled away from, mid-interaction. DOM focus
// must never scroll (see panes/paneComposerFocus.ts).

export function focusApprovalActionContainer(container: HTMLElement | undefined): void {
  container?.focus({ preventScroll: true });
}

export function focusApprovalActionFromKey(event: KeyboardEvent, container: HTMLElement | undefined): void {
  if (!isPlainNavigation(event)) return;
  const buttons = actionButtons(container);
  if (buttons.length === 0) return;

  const activeIndex = document.activeElement instanceof HTMLButtonElement
    ? buttons.indexOf(document.activeElement)
    : -1;

  if (isForwardKey(event)) {
    event.preventDefault();
    event.stopPropagation();
    const nextIndex = activeIndex < 0 ? 0 : Math.min(activeIndex + 1, buttons.length - 1);
    buttons[nextIndex]?.focus({ preventScroll: true });
    return;
  }

  if (isBackwardKey(event)) {
    event.preventDefault();
    event.stopPropagation();
    const nextIndex = activeIndex < 0 ? buttons.length - 1 : Math.max(activeIndex - 1, 0);
    buttons[nextIndex]?.focus({ preventScroll: true });
  }
}
