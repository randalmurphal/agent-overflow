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
  consume(event);
}

function preventHistoryMouseButton(event: MouseEvent): void {
  if (event.defaultPrevented) return;
  if (event.button !== 3 && event.button !== 4) return;
  consume(event);
}

function preventBrowserContextMenu(event: MouseEvent): void {
  event.preventDefault();
}

function consume(event: Event): void {
  event.preventDefault();
  event.stopImmediatePropagation();
}
