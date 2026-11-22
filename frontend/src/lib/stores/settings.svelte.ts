import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
// Each computer owns a settings snapshot and save queue. This frontend owns
// its presentation preferences, overlaid on every computer's projection.
import { FRONTEND_DEVICE_SETTINGS_KEYS, SETTINGS_DEFAULTS, SETTINGS_KEYS_OMITTED_WHEN_EMPTY } from '../generated/settingsDefaults';
import type { Settings } from '../types/settings';
import { GetSettings, UpdateSettings } from './bindings';
import { addErrorToast } from './toast.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { defaultSettings, mergeSettingsWithDefaults } from './settingsDefaults';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { isFrontendOnly } from '../transport/runMode';
import { attachedBackends, backendById, onBackendDetached, withBackendTarget } from '../transport/backends';
import { backendDisplayName, getAttachedBackends } from './attachedBackends.svelte';
import {
  frontendPreferences, seedFrontendPreferences, updateFrontendPreferences,
  isFrontendPreference, resetFrontendPreferencesForTest, validatedFrontendPreferences,
} from './frontendPreferences.svelte';

type PendingPatch = { patch: Partial<Settings> };
interface ComputerSettings {
  saved: Settings;
  pending: readonly PendingPatch[];
  /** The keys the computer's last settings answer named; null before one. */
  named: ReadonlySet<string> | null;
}
const EMPTY: ComputerSettings = { saved: defaultSettings(), pending: [], named: null };
// The keys an answer of this build always names and this frontend keeps a
// default for: the ones an answer without them says a release lacks.
const omittedWhenEmpty = new Set<string>(SETTINGS_KEYS_OMITTED_WHEN_EMPTY);
const alwaysNamed = new Set(Object.keys(SETTINGS_DEFAULTS).filter((key) => !omittedWhenEmpty.has(key)));
const computers = createKeyedSignalRegistry<ComputerSettings>(EMPTY);
const queues = new Map<BackendKey, Promise<void>>();
const projections = new Map<BackendKey, {
  source: ComputerSettings; preferences: Partial<Settings>; value: Settings;
}>();
let generation = 0;

const mirroredKeys = new Set<string>(FRONTEND_DEVICE_SETTINGS_KEYS);
interface PreferenceMirror { pending: Partial<Settings>; running: boolean }
const mirrors = new Map<BackendKey, PreferenceMirror>();

/**
 * Only device-tier keys may cross this seam; user-tier defaults never do. A
 * computer is mirrored once its settings answer says which keys it has, and
 * never a key it lacks (computerHasSetting).
 */
export function mirrorFrontendPreferences(backend: BackendKey, patch: Partial<Settings> = frontendPreferences()): void {
  if (isFrontendOnly() && backend === HOME_BACKEND) return;
  if (!backendById(backend)) return;
  const values = Object.fromEntries(Object.entries(validatedFrontendPreferences(patch)).filter(([key]) => mirroredKeys.has(key))) as Partial<Settings>;
  if (!Object.keys(values).length) return;
  let mirror = mirrors.get(backend);
  if (!mirror) { mirror = { pending: {}, running: false }; mirrors.set(backend, mirror); }
  Object.assign(mirror.pending, values);
  flushMirror(backend);
}

function flushMirror(backend: BackendKey): void {
  const target = backendById(backend);
  const mirror = mirrors.get(backend);
  if (!target || !mirror || mirror.running) return;
  if (target.status.status !== 'connected' && (!target.home || target.client.getHello?.())) return;
  // readSettings flushes again once the answer lands.
  if (computers.get(backend).named === null) return;
  const held = mirror;
  held.running = true;
  void (async () => {
    try {
      while (mirrors.get(backend) === held && backendById(backend) === target && Object.keys(held.pending).length) {
        const next = Object.fromEntries(Object.entries(held.pending)
          .filter(([key]) => computerHasSetting(key as keyof Settings, backend))) as Partial<Settings>;
        held.pending = {};
        if (!Object.keys(next).length) continue;
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

/**
 * Whether the computer's release has the setting `key`, from its own
 * settings answer: this build's answer names every key in `alwaysNamed`, so
 * an answer without one is from a release without it. True until the
 * computer's settings are read. Home is no exception: an installed app's
 * home can run an older release. Nothing writes a key there that this says
 * it lacks (updateSettingsPatch, the preference mirror); a release classes
 * an unknown key host tier, refused without a step-up.
 */
export function computerHasSetting(key: keyof Settings, backend: BackendKey = HOME_BACKEND): boolean {
  if (!alwaysNamed.has(key)) return true;
  const named = computers.get(backend).named;
  return named === null || named.has(key);
}

/** What a page or control for settings `lacks` says on a computer whose release has none. */
export function olderComputerNote(backend: BackendKey, lacks: string): string {
  return `${settingsComputerName(backend) || 'This computer'} runs an older version: it has no ${lacks}.`;
}

/**
 * The name a settings page gives its computer: empty when only one
 * computer is attached, where the page says "this computer".
 */
export function settingsComputerName(backend: BackendKey): string {
  const attached = getAttachedBackends();
  const entry = attached.find((candidate) => candidate.id === backend);
  return entry && attached.length > 1 ? backendDisplayName(entry) : '';
}

function namedKeys(result: object): ReadonlySet<string> {
  return new Set(Object.keys(result));
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

/**
 * The one description of "which settings change a rendered row's height at
 * a fixed content width". It keys the timeline's measured row-height priors
 * (`utils/virtual/priors.ts`, `SizePriorsGeometry.typography`), so a replay
 * under different typography misses and re-measures instead of restoring
 * heights from the old settings.
 *
 * Every field here is a FRONTEND preference (`FRONTEND_SETTINGS_KEYS`), so
 * it reads the same on every computer and needs no backend argument:
 *
 *   - `fontSize`     root font scale on <html> (utils/zoom.ts), so it
 *                    rescales every row.
 *   - `sansFont`     `--font-sans` typeface; metrics shift line counts and
 *                    line heights at a fixed width.
 *   - `monoFont`     `--font-mono` typeface; same, for code, diffs and
 *                    command output.
 *   - `collapseDiffPreviews`
 *                    default expand/collapse of an un-overridden inline
 *                    diff card (DiffFileBlock). Not covered by
 *                    `expansionSignature`, which serializes reader
 *                    deviations from the default and is empty when the
 *                    default itself moves.
 *   - `diffWordWrap` wraps vs horizontally scrolls tool-result and diff
 *                    text (ToolResultCard), which changes line count at a
 *                    fixed width.
 *
 * Add a new height-affecting display setting HERE, not at a call site: a
 * partial list silently replays wrong heights for whatever it omits.
 * Settings that only change motion, cadence or chrome (`lowPowerMode`,
 * `streamingEnabled`, the spinner keys) are deliberately absent — they do
 * not change a settled row's height. `paneDensity` is absent because it
 * changes the pane's width, which the width half of the key already
 * carries.
 */
export function typographySignature(): string {
  const settings = getSettings();
  return [
    `f${settings.fontSize}`,
    `s${settings.sansFont}`,
    `m${settings.monoFont}`,
    `c${settings.collapseDiffPreviews ? 1 : 0}`,
    `w${settings.diffWordWrap ? 1 : 0}`,
  ].join('/');
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
      computers.set(backend, {
        saved, pending: computers.get(backend).pending, named: namedKeys(result),
      });
      if (migrate && backend === HOME_BACKEND) seedFrontendPreferences(saved);
      flushMirror(backend);
    }
    return true;
  } catch (err) {
    if (expected !== generation || backendById(backend) !== target) return false;
    if (isPassiveConnectionFailure(err)) return false;
    console.error('Failed to load computer settings:', err);
    if (migrate) addErrorToast('Failed to load settings', err);
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
): Promise<SettingsWrite> {
  return updateSettingsPatch({ [key]: value } as Partial<Settings>, backend);
}

/**
 * How a settings write ended. The store has already applied each outcome
 * (shown the patch, then the saved snapshot, or put the saved values back
 * with an error toast); the result tells a caller that must know which.
 *
 * - `local`: nothing for the computer to store; every key is a frontend
 *   preference (saved before the call returns) or one the computer's release
 *   lacks (computerHasSetting).
 * - `saved`: the computer stored the patch; `saved` is its settings after.
 * - `refused`: the computer refused the patch; `saved` is its settings,
 *   unchanged by it.
 * - `dropped`: the computer this write was for went away or the store was
 *   reset before it answered, so how it ended is unknown.
 */
export type SettingsWrite =
  | { status: 'local' }
  | { status: 'saved'; saved: Settings }
  | { status: 'refused'; saved: Settings; error: unknown }
  | { status: 'dropped' };

export function updateSettingsPatch(
  patch: Partial<Settings>, backend: BackendKey = HOME_BACKEND,
): Promise<SettingsWrite> {
  updateFrontendPreferences(patch);
  const frontend = Object.fromEntries(Object.entries(patch).filter(([key]) => isFrontendPreference(key)));
  if (Object.keys(frontend).length) {
    for (const entry of attachedBackends()) mirrorFrontendPreferences(entry.id, frontend);
  }
  // A key the computer's release lacks is not written there (computerHasSetting).
  const host = Object.fromEntries(Object.entries(patch).filter(([key]) =>
    !isFrontendPreference(key) && computerHasSetting(key as keyof Settings, backend))) as Partial<Settings>;
  // A local preference is fully saved before returning. A sleeping host's
  // optional device-bucket mirror cannot hold a toggle or block host edits.
  if (!Object.keys(host).length) return Promise.resolve({ status: 'local' });
  const pending = { patch: host };
  const current = computers.get(backend);
  computers.set(backend, { ...current, pending: [...current.pending, pending] });
  const expected = generation;
  const target = backendById(backend);
  let outcome: SettingsWrite = { status: 'dropped' };
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
      computers.set(backend, { ...state, saved, named: result ? namedKeys(result) : state.named });
      outcome = { status: 'saved', saved };
    } catch (err) {
      if (expected !== generation || backendById(backend) !== target) return;
      console.error('Failed to update computer settings:', err);
      if (Object.keys(patch).some((key) => !isFrontendPreference(key))) {
        addErrorToast('Failed to save setting', err);
      }
      outcome = { status: 'refused', saved: computers.get(backend).saved, error: err };
    } finally {
      if (expected === generation && backendById(backend) === target) {
        const state = computers.get(backend);
        computers.set(backend, { ...state, pending: state.pending.filter((p) => p !== pending) });
      }
    }
  }).then(() => outcome);
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
    named: namedKeys(result),
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
