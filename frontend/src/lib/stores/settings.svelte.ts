// Each computer owns a settings snapshot and save queue. This frontend owns
// its presentation preferences, overlaid on every computer's projection.
import { FRONTEND_DEVICE_SETTINGS_KEYS } from '../generated/settingsDefaults';
import type { Settings } from '../types/settings';
import { GetSettings, UpdateSettings } from './bindings';
import { addToast } from './toast.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { defaultSettings, mergeSettingsWithDefaults } from './settingsDefaults';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { isFrontendOnly } from '../transport/runMode';
import { attachedBackends, backendById, onBackendDetached, withBackendTarget } from '../transport/backends';
import {
  frontendPreferences, seedFrontendPreferences, updateFrontendPreferences,
  isFrontendPreference, resetFrontendPreferencesForTest, validatedFrontendPreferences,
} from './frontendPreferences.svelte';

type PendingPatch = { patch: Partial<Settings> };
interface ComputerSettings {
  saved: Settings;
  pending: readonly PendingPatch[];
}
const EMPTY: ComputerSettings = { saved: defaultSettings(), pending: [] };
const computers = createKeyedSignalRegistry<ComputerSettings>(EMPTY);
const queues = new Map<BackendKey, Promise<void>>();
const projections = new Map<BackendKey, {
  source: ComputerSettings; preferences: Partial<Settings>; value: Settings;
}>();
let generation = 0;

const mirroredKeys = new Set<string>(FRONTEND_DEVICE_SETTINGS_KEYS);
interface PreferenceMirror { pending: Partial<Settings>; running: boolean }
const mirrors = new Map<BackendKey, PreferenceMirror>();

/** Only device-tier keys may cross this seam; user-tier defaults never do. */
export function mirrorFrontendPreferences(backend: BackendKey, patch: Partial<Settings> = frontendPreferences()): void {
  if (isFrontendOnly() && backend === HOME_BACKEND) return;
  const target = backendById(backend);
  if (!target) return;
  const values = Object.fromEntries(Object.entries(validatedFrontendPreferences(patch)).filter(([key]) => mirroredKeys.has(key))) as Partial<Settings>;
  if (!Object.keys(values).length) return;
  let mirror = mirrors.get(backend);
  if (!mirror) { mirror = { pending: {}, running: false }; mirrors.set(backend, mirror); }
  Object.assign(mirror.pending, values);
  if (mirror.running || (target.status.status !== 'connected' && (!target.home || target.client.getHello?.()))) return;
  const held = mirror;
  held.running = true;
  void (async () => {
    try {
      while (mirrors.get(backend) === held && backendById(backend) === target && Object.keys(held.pending).length) {
        const next = held.pending;
        held.pending = {};
        try { await withBackendTarget(backend, () => UpdateSettings(next)); }
        catch {
          // Preserve the newest value for a reconnect or the next change.
          held.pending = { ...next, ...held.pending };
          return;
        }
      }
    } finally { held.running = false; }
  })();
}

export function hasComputerSettings(backend: BackendKey): boolean {
  return computers.get(backend) !== EMPTY;
}

/** Stable per-computer projection; reads allocate only after an actual change. */
export function getSettings(backend: BackendKey = HOME_BACKEND): Settings {
  const source = computers.get(backend);
  const preferences = frontendPreferences();
  const cached = projections.get(backend);
  if (cached?.source === source && cached.preferences === preferences) return cached.value;
  const value = { ...source.saved };
  for (const pending of source.pending) Object.assign(value, pending.patch);
  Object.assign(value, preferences);
  projections.set(backend, { source, preferences, value });
  return value;
}

// Serial within a computer, independent across computers. Failed operations
// handle their own errors, so they cannot poison the next operation's queue.
function enqueue(backend: BackendKey, work: () => Promise<void>): Promise<void> {
  const target = backendById(backend);
  const guarded = async () => {
    if (backendById(backend) !== target) return;
    await work();
  };
  const ahead = queues.get(backend);
  const run = ahead ? ahead.then(guarded) : guarded();
  queues.set(backend, run);
  void run.then(() => { if (queues.get(backend) === run) queues.delete(backend); });
  return run;
}

async function readSettings(backend: BackendKey, migrate: boolean): Promise<boolean> {
  const expected = generation;
  const target = backendById(backend);
  try {
    const result = await withBackendTarget(backend, () => GetSettings());
    if (expected !== generation || backendById(backend) !== target) return false;
    if (result) {
      const saved = mergeSettingsWithDefaults(result as Partial<Settings>);
      computers.set(backend, { saved, pending: computers.get(backend).pending });
      if (migrate && backend === HOME_BACKEND) seedFrontendPreferences(saved);
    }
    return true;
  } catch (err) {
    if (expected !== generation || backendById(backend) !== target) return false;
    console.error('Failed to load computer settings:', err);
    if (migrate) addToast('error', 'Failed to load settings');
    return false;
  }
}

export async function loadSettings(backend: BackendKey = HOME_BACKEND): Promise<boolean> {
  if (isFrontendOnly() && backend === HOME_BACKEND) return false;
  let loaded = false;
  await enqueue(backend, async () => { loaded = await readSettings(backend, true); });
  return loaded;
}

export function updateSetting<K extends keyof Settings>(
  key: K, value: Settings[K], backend: BackendKey = HOME_BACKEND,
): Promise<void> {
  return updateSettingsPatch({ [key]: value } as Partial<Settings>, backend);
}

export function updateSettingsPatch(
  patch: Partial<Settings>, backend: BackendKey = HOME_BACKEND,
): Promise<void> {
  updateFrontendPreferences(patch);
  const frontend = Object.fromEntries(Object.entries(patch).filter(([key]) => isFrontendPreference(key)));
  if (Object.keys(frontend).length) {
    for (const entry of attachedBackends()) mirrorFrontendPreferences(entry.id, frontend);
  }
  const host = Object.fromEntries(Object.entries(patch).filter(([key]) => !isFrontendPreference(key))) as Partial<Settings>;
  // A local preference is fully saved before returning. A sleeping host's
  // optional device-bucket mirror cannot hold a toggle or block host edits.
  if (!Object.keys(host).length) return Promise.resolve();
  const pending = { patch: host };
  const current = computers.get(backend);
  computers.set(backend, { ...current, pending: [...current.pending, pending] });
  const expected = generation;
  const target = backendById(backend);
  return enqueue(backend, async () => {
    if (expected !== generation || backendById(backend) !== target) return;
    try {
      // Host settings remain authoritative on the captured computer.
      const result = await withBackendTarget(backend, () => UpdateSettings(pending.patch));
      if (expected !== generation || backendById(backend) !== target) return;
      const state = computers.get(backend);
      const saved = result
        ? mergeSettingsWithDefaults(result as Partial<Settings>)
        : { ...state.saved, ...pending.patch };
      computers.set(backend, { ...state, saved });
    } catch (err) {
      if (expected !== generation || backendById(backend) !== target) return;
      console.error('Failed to update computer settings:', err);
      if (Object.keys(patch).some((key) => !isFrontendPreference(key))) {
        addToast('error', 'Failed to save setting');
      }
    } finally {
      if (expected === generation && backendById(backend) === target) {
        const state = computers.get(backend);
        computers.set(backend, { ...state, pending: state.pending.filter((p) => p !== pending) });
      }
    }
  });
}

/** Events contain changed keys, not secrets. Re-read their originating host. */
export function resyncSettings(backend: BackendKey = HOME_BACKEND): Promise<void> {
  return enqueue(backend, async () => { await readSettings(backend, false); });
}

/** Dedicated mutators return the same redacted snapshot as GetSettings. */
export function applySettingsSnapshot(
  result: Partial<Settings>, backend: BackendKey = HOME_BACKEND,
): void {
  computers.set(backend, {
    saved: mergeSettingsWithDefaults(result), pending: computers.get(backend).pending,
  });
}

export function resetSettingsForTest(): void {
  generation++;
  resetFrontendPreferencesForTest();
  computers.reset();
  projections.clear();
  queues.clear();
  mirrors.clear();
}

onBackendDetached(({ backendId }) => {
  computers.drop(backendId);
  projections.delete(backendId);
  queues.delete(backendId);
  mirrors.delete(backendId);
});
