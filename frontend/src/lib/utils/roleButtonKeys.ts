// Keyboard activation for a `role="button"` element, as a native button
// does it: Enter activates on keydown and not on auto-repeat; Space stops
// the page scroll on keydown and activates once on keyup; the element's
// blur disarms it, so moving focus away while Space is held cancels. A key
// aimed at a control inside the element is that control's: the helper acts
// only on events targeted at the element itself. Ctrl or Cmd with Enter or
// Space reaches `activate` as the press's ActivationIntent (open in a new
// pane), as a modified click does; Alt with Enter is an app chord
// (alt+shift+enter) and is left to it.

import { isEnterKey } from './localEnter';
import { activationIntent, type ActivationIntent } from './modClick';

export interface RoleButtonKeys {
  /** Returns whether the key was the button's own. */
  keydown(event: KeyboardEvent): boolean;
  keyup(event: KeyboardEvent): void;
  blur(): void;
}

function ownKey(event: KeyboardEvent): boolean {
  return event.target === event.currentTarget;
}

export function roleButtonKeys(activate: (intent: ActivationIntent) => void): RoleButtonKeys {
  let spaceArmed = false;
  return {
    keydown(event) {
      if (!ownKey(event)) return false;
      if (isEnterKey(event) && !event.altKey) {
        event.preventDefault();
        if (!event.repeat) activate(activationIntent(event));
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
      activate(activationIntent(event));
    },
    blur() {
      spaceArmed = false;
    },
  };
}
