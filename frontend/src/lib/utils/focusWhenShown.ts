/**
 * Focuses the element `find` returns once it can take focus, trying again
 * each frame: a virtualized row stays hidden until it is measured, and a
 * hidden element takes no focus. Stops after `frames` tries, or as soon as
 * focus goes to any other element, so it never takes focus back from
 * where the reader moved it. `onFocused` runs once the element has focus.
 * Returns the cancel.
 */
export function focusWhenShown(
  find: () => HTMLElement | null | undefined,
  onFocused?: (element: HTMLElement) => void,
  frames = 10,
): () => void {
  const owner = document.activeElement;
  let done = false;
  let tries = 0;
  let handle = 0;
  const stop = (): void => {
    if (done) return;
    done = true;
    cancelAnimationFrame(handle);
    document.removeEventListener('focusin', onFocusIn, true);
  };
  const onFocusIn = (event: FocusEvent): void => {
    if (event.target !== find()) stop();
  };
  const attempt = (): void => {
    if (done) return;
    // Focus is on another element than when this was asked: the reader's.
    const active = document.activeElement;
    if (active !== owner && active !== null && active !== document.body && active !== find()) {
      stop();
      return;
    }
    const element = find();
    if (element?.isConnected) {
      element.focus({ preventScroll: true });
      if (document.activeElement === element) {
        stop();
        onFocused?.(element);
        return;
      }
    }
    if (tries >= frames) {
      stop();
      return;
    }
    tries += 1;
    handle = requestAnimationFrame(attempt);
  };
  document.addEventListener('focusin', onFocusIn, true);
  attempt();
  return stop;
}
