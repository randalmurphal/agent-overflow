// A local Enter handler (submit a field, step a find, activate a row) owns
// plain Enter and Shift+Enter only. A chord with Alt, Ctrl or Meta belongs
// to the app's keybindings: App.svelte's global dispatcher declines an event
// a local handler prevented, so taking one swallows a bound command such as
// `alt+shift+enter` (pane.togglePromoted). A handler that means a modifier
// chord (ctrl/meta+Enter to submit a comment) checks for it itself.

export type LocalEnterEvent = Pick<KeyboardEvent, 'key' | 'altKey' | 'ctrlKey' | 'metaKey'>;

export function isLocalEnter(event: LocalEnterEvent): boolean {
  return event.key === 'Enter' && !event.altKey && !event.ctrlKey && !event.metaKey;
}
