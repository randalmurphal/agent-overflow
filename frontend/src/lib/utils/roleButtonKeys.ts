// Keyboard activation for a `role="button"` element, as a native button
// does it: Enter activates on keydown and not on auto-repeat; Space stops
// the page scroll on keydown and activates once on keyup; the element's
// blur disarms it, so moving focus away while Space is held cancels. A key
// aimed at a control inside the element is that control's: the helper acts
// only on events targeted at the element itself.

export interface RoleButtonKeys {
  /** Returns whether the key was the button's own. */
  keydown(event: KeyboardEvent): boolean;
  keyup(event: KeyboardEvent): void;
  blur(): void;
}

function ownKey(event: KeyboardEvent): boolean {
  return event.target === event.currentTarget;
}

export function roleButtonKeys(activate: (event: KeyboardEvent) => void): RoleButtonKeys {
  let spaceArmed = false;
  return {
    keydown(event) {
      if (!ownKey(event)) return false;
      if (event.key === 'Enter') {
        event.preventDefault();
        if (!event.repeat) activate(event);
        return true;
      }
      if (event.key === ' ') {
        event.preventDefault();
        spaceArmed = true;
        return true;
      }
      return false;
    },
    keyup(event) {
      if (!ownKey(event) || event.key !== ' ' || !spaceArmed) return;
      spaceArmed = false;
      event.preventDefault();
      activate(event);
    },
    blur() {
      spaceArmed = false;
    },
  };
}
