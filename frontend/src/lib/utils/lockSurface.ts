/** Cover app portals as well as the root, and stop global shortcuts while locked. */
export function createLockSurface(overlay: HTMLElement) {
  let locked = false;
  const previous = new Map<HTMLElement, boolean>();
  const events = ['keydown', 'keyup', 'click', 'pointerdown', 'contextmenu', 'paste', 'drop'] as const;
  overlay.style.position = 'relative';
  overlay.style.zIndex = '2147483647';

  const cover = () => {
    for (const child of document.body.children) {
      if (!(child instanceof HTMLElement) || child === overlay || previous.has(child)) continue;
      previous.set(child, child.inert);
      child.inert = true;
    }
    for (const child of previous.keys()) if (!child.isConnected) previous.delete(child);
  };
  const observer = new MutationObserver(cover);
  const blockOutside = (event: Event) => {
    if (locked && !event.composedPath().includes(overlay)) {
      // Inert content leaves only the cover in the tab order. Let a keyboard
      // user return to it if focus moved to the body during authentication.
      if (!(event instanceof KeyboardEvent && event.key === 'Tab')) event.preventDefault();
      event.stopImmediatePropagation();
    }
  };
  const stopAtCover = (event: Event) => {
    if (locked) event.stopPropagation();
  };
  for (const event of events) {
    window.addEventListener(event, blockOutside, true);
    overlay.addEventListener(event, stopAtCover);
  }

  function setLocked(value: boolean): void {
    if (locked === value) return;
    locked = value;
    if (locked) {
      cover();
      observer.observe(document.body, { childList: true });
    } else {
      observer.disconnect();
      for (const [child, inert] of previous) child.inert = inert;
      previous.clear();
    }
  }
  return {
    setLocked,
    dispose(): void {
      setLocked(false);
      observer.disconnect();
      for (const event of events) {
        window.removeEventListener(event, blockOutside, true);
        overlay.removeEventListener(event, stopAtCover);
      }
    },
  };
}
