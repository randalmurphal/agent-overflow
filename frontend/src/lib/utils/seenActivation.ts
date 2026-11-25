import type { Attachment } from 'svelte/attachments';
import { isEnterKey } from './localEnter';

// The keys that activate a button.
function activates(event: KeyboardEvent): boolean {
  return event.key === ' ' || isEnterKey(event);
}

/**
 * Makes a control act on what the reader saw when they began to press it,
 * not on what a reload put under it before the click.
 *
 * An activation starts on the control's primary pointerdown, or on its
 * keydown of Space or Enter, and captures `read()` then. The control's
 * click runs `run` with that capture and ends the activation. One that
 * ends without a click on the control is retired: a pointercancel, a
 * pointer released anywhere or Space or Enter released with no click on
 * it after, or the control losing focus. A click with no activation
 * (assistive technology, a programmatic click) runs with `read()` at the
 * click: nothing was seen before it. `run` also gets the click, for its
 * modifier keys. The capture is the control's own; no state
 * is shared between controls.
 */
export function seenActivation<T>(read: () => T, run: (seen: T, click: MouseEvent) => void): Attachment<HTMLElement> {
  return (node) => {
    let seen: { value: T } | null = null;
    // Which activation a deferred retire belongs to.
    let activation = 0;

    const retire = (): void => {
      seen = null;
      activation += 1;
      window.removeEventListener('pointerup', onRelease, true);
      window.removeEventListener('pointercancel', retire, true);
      window.removeEventListener('keyup', onKeyUp, true);
    };
    // The click a release makes is dispatched in the same task as the
    // release: the activation outlives the release only until then.
    const onRelease = (): void => {
      const ended = activation;
      setTimeout(() => {
        if (activation === ended) retire();
      }, 0);
    };
    const onKeyUp = (event: KeyboardEvent): void => {
      if (activates(event)) onRelease();
    };
    const start = (): void => {
      retire();
      seen = { value: read() };
      window.addEventListener('pointerup', onRelease, true);
      window.addEventListener('pointercancel', retire, true);
      window.addEventListener('keyup', onKeyUp, true);
    };
    const onPointerDown = (event: PointerEvent): void => {
      if (event.button === 0) start();
    };
    const onKeyDown = (event: KeyboardEvent): void => {
      if (!activates(event)) return;
      // A held key repeats: the activation began at the first keydown.
      if (event.repeat && seen) return;
      start();
    };
    const onClick = (event: MouseEvent): void => {
      const value = seen ? seen.value : read();
      retire();
      run(value, event);
    };

    node.addEventListener('pointerdown', onPointerDown);
    node.addEventListener('keydown', onKeyDown);
    node.addEventListener('blur', retire);
    node.addEventListener('click', onClick);
    return () => {
      retire();
      node.removeEventListener('pointerdown', onPointerDown);
      node.removeEventListener('keydown', onKeyDown);
      node.removeEventListener('blur', retire);
      node.removeEventListener('click', onClick);
    };
  };
}

/**
 * Delegated variant of `seenActivation` for lists/blocks with many buttons:
 * attaches one set of listeners on the container instead of 4 per button,
 * resolving the control via `match(target)` and preserving identical press-to-
 * click capture semantics.
 */
export function delegatedSeenActivation<T>(
  match: (target: EventTarget | null) => HTMLElement | null,
  read: (control: HTMLElement) => T | null,
  run: (seen: T, click: MouseEvent) => void,
): Attachment<HTMLElement> {
  return (container) => {
    let seenControl: HTMLElement | null = null;
    let seenValue: T | null = null;
    let hasSeen = false;
    let activation = 0;

    const retire = (): void => {
      seenControl = null;
      seenValue = null;
      hasSeen = false;
      activation += 1;
      window.removeEventListener('pointerup', onRelease, true);
      window.removeEventListener('pointercancel', retire, true);
      window.removeEventListener('keyup', onKeyUp, true);
    };
    const onRelease = (): void => {
      const ended = activation;
      setTimeout(() => {
        if (activation === ended) retire();
      }, 0);
    };
    const onKeyUp = (event: KeyboardEvent): void => {
      if (activates(event)) onRelease();
    };
    const start = (control: HTMLElement): void => {
      retire();
      const val = read(control);
      if (val === null) return;
      seenControl = control;
      seenValue = val;
      hasSeen = true;
      window.addEventListener('pointerup', onRelease, true);
      window.addEventListener('pointercancel', retire, true);
      window.addEventListener('keyup', onKeyUp, true);
    };
    const onPointerDown = (event: PointerEvent): void => {
      if (event.button !== 0) return;
      const control = match(event.target);
      if (control) start(control);
    };
    const onKeyDown = (event: KeyboardEvent): void => {
      if (!activates(event)) return;
      const control = match(event.target);
      if (!control) return;
      if (event.repeat && hasSeen && seenControl === control) return;
      start(control);
    };
    const onFocusOut = (event: FocusEvent): void => {
      if (hasSeen && seenControl && event.target === seenControl) retire();
    };
    const onClick = (event: MouseEvent): void => {
      const control = match(event.target);
      if (!control) return;
      const value = hasSeen && seenControl === control ? seenValue : read(control);
      retire();
      if (value !== null) run(value, event);
    };

    container.addEventListener('pointerdown', onPointerDown);
    container.addEventListener('keydown', onKeyDown);
    container.addEventListener('focusout', onFocusOut);
    container.addEventListener('click', onClick);
    return () => {
      retire();
      container.removeEventListener('pointerdown', onPointerDown);
      container.removeEventListener('keydown', onKeyDown);
      container.removeEventListener('focusout', onFocusOut);
      container.removeEventListener('click', onClick);
    };
  };
}
