// Which actions a menu shows at once, the rest sitting behind Show more
// (`components/shared/ActionMenu.svelte`). A caller names its menu (one
// name per kind of menu, such as `issues-card`), gives each action a
// stable ID and says which show at once by default; the person changes
// that from the menu itself, and the choice is kept here per menu name,
// on this device, until they change it again. Another window's change is
// heard. An ID the caller no longer gives is ignored, so a removed action
// needs no migration.

import { SvelteMap } from 'svelte/reactivity';
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';

const KEY = 'menu-shown-actions';

function read(): Map<string, readonly string[]> {
  const v = readFrontendValue(KEY);
  const out = new Map<string, readonly string[]>();
  if (!v || typeof v !== 'object') return out;
  for (const [menu, ids] of Object.entries(v as Record<string, unknown>)) {
    if (Array.isArray(ids)) out.set(menu, ids.filter((id): id is string => typeof id === 'string'));
  }
  return out;
}

const chosen = new SvelteMap<string, readonly string[]>(read());

if (typeof window !== 'undefined') {
  onFrontendValueChanged(KEY, () => {
    const next = read();
    for (const menu of chosen.keys()) if (!next.has(menu)) chosen.delete(menu);
    for (const [menu, ids] of next) chosen.set(menu, ids);
  });
}

/** The actions `menu` shows at once: the person's choice, else
 * `defaults`; reactive. */
export function shownActions(menu: string, defaults: readonly string[]): ReadonlySet<string> {
  return new Set(chosen.get(menu) ?? defaults);
}

/** Shows `id` at once in `menu`, or puts it behind Show more. */
export function setActionShown(menu: string, defaults: readonly string[], id: string, shown: boolean): void {
  const now = chosen.get(menu) ?? defaults;
  const next = shown ? (now.includes(id) ? now : [...now, id]) : now.filter((x) => x !== id);
  chosen.set(menu, next);
  // A failed save says so (`writeFrontendValue`); the choice holds until
  // the app closes.
  writeFrontendValue(KEY, Object.fromEntries(chosen));
}

export function resetMenuShownActionsForTest(): void {
  chosen.clear();
}
