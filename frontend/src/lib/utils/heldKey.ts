// A held Enter repeats its keydown, and a native button clicks on each
// one: a control that writes takes this on its keydown (or an ancestor's)
// so only the first press acts. Space clicks on keyup, once.
export function blockHeldActivation(event: KeyboardEvent): void {
  if (event.repeat && (event.key === 'Enter' || event.key === ' ')) event.preventDefault();
}
