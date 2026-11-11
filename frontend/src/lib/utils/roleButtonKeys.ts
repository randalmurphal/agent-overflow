// Keyboard activation for a `role="button"` element, as a native button
// does it: Enter activates on keydown and not on auto-repeat; Space stops
// the page scroll on keydown and activates once on keyup; the element's
// blur disarms it, so moving focus away while Space is held cancels. Space
// aimed at a control inside the element is left to it.

export interface RoleButtonKeys {
  /** Returns whether the key was the button's own. */
  keydown(event: KeyboardEvent): boolean;
  keyup(event: KeyboardEvent): void;
  blur(): void;
}

export function roleButtonKeys(activate: (event: KeyboardEvent) => void): RoleButtonKeys {
  let spaceArmed = false;
  return {
    keydown(event) {
      if (event.key === 'Enter') {
        event.preventDefault();
        if (!event.repeat) activate(event);
        return true;
      }
      if (event.key === ' ' && event.target === event.currentTarget) {
        event.preventDefault();
        spaceArmed = true;
        return true;
      }
      return false;
    },
    keyup(event) {
      if (event.key !== ' ' || !spaceArmed) return;
      spaceArmed = false;
      event.preventDefault();
      activate(event);
    },
    blur() {
      spaceArmed = false;
    },
  };
}
