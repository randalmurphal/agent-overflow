// The phone owns its shortcut choices. No remote host (or pairing) is needed
// to read, edit, or reset them. Only chord overrides are persisted: commands,
// contexts and newly shipped shortcuts always come from the current bundle.
import { KEYBINDING_DEFAULTS } from '../generated/keybindingDefaults';
import { readFrontendValue, writeFrontendValue } from '../stores/frontendStorage';
import { setKeybindingPersistence, type KeybindingPersistence } from '../stores/keybindings.svelte';
import { tryParseChord } from '../stores/keybindingParser';

const KEY = 'keybindings';
const defaultsById = new Map(KEYBINDING_DEFAULTS.map((rule) => [rule.defaultId, rule]));

function validChord(value: unknown): value is string {
  return typeof value === 'string' && value.length <= 256 && (value === '' || tryParseChord(value) !== null);
}

function persist(overrides: Record<string, string>): void {
  if (!writeFrontendValue(KEY, overrides)) throw new Error('This device could not save its shortcuts.');
}

const persistence: KeybindingPersistence = {
  async read() {
    const saved = readFrontendValue(KEY);
    const overrides = saved && typeof saved === 'object' && !Array.isArray(saved)
      ? saved as Record<string, unknown> : {};
    return { bindings: KEYBINDING_DEFAULTS.map((rule) => {
      const key = rule.defaultId && Object.hasOwn(overrides, rule.defaultId) ? overrides[rule.defaultId] : undefined;
      return { ...rule, key: validChord(key) ? key : rule.key };
    }) };
  },
  async write(rules) {
    const overrides: Record<string, string> = {};
    for (const rule of rules) {
      const shipped = defaultsById.get(rule.defaultId);
      if (!shipped || !rule.defaultId || !validChord(rule.key)) throw new Error('Invalid shortcut.');
      if (rule.key !== shipped.key) overrides[rule.defaultId] = rule.key;
    }
    persist(overrides);
  },
  async reset() { persist({}); },
};

export function installNativeKeybindings(): void {
  setKeybindingPersistence(persistence);
}
