// PHASE 7, HOME-ONLY: `GetSettings` / `UpdateSettings` take the `home`
// route, so this store reads and writes the page's own backend even when
// others are attached. Per-backend settings is remote-access §10's own
// plan item and is deliberately not built here — a single projection over
// several machines' settings is a merge policy question (which machine's
// theme? whose provider paths?), not a keying one.
import type { Settings } from "../types/settings";
import { SETTINGS_DEFAULTS } from "../generated/settingsDefaults";
import { GetSettings, UpdateSettings } from "./bindings";
import { addToast } from "./toast.svelte";

/**
 * The defaults are GENERATED from internal/settings.DefaultSettings
 * (`go generate ./internal/settings`), never hand-mirrored: the two used
 * to be kept in step by comments, which is a synchronization method with
 * no failure mode short of a user noticing the wrong value. Which fields
 * get a default and which stay undefined is the generator's deny-list.
 *
 * They are load-bearing at runtime, not just a pre-load placeholder: Go's
 * `omitempty` drops zero-valued fields on the wire, so every GetSettings
 * read comes back missing keys that mergeSettingsWithDefaults fills from
 * here.
 */
function defaultSettings(): Settings {
  // Deep-copied per call: the store mutates what it hands out, and the
  // generated object is module-level shared state.
  return {
    ...SETTINGS_DEFAULTS,
    recentWorkspaces: [...SETTINGS_DEFAULTS.recentWorkspaces],
    network: { ...SETTINGS_DEFAULTS.network },
    retention: { ...SETTINGS_DEFAULTS.retention },
    gitlabSelfHostedHosts: [...SETTINGS_DEFAULTS.gitlabSelfHostedHosts],
    claudeHiddenModels: [...SETTINGS_DEFAULTS.claudeHiddenModels],
    codexHiddenModels: [...SETTINGS_DEFAULTS.codexHiddenModels],
    claudeCustomEnv: [...SETTINGS_DEFAULTS.claudeCustomEnv],
    codexCustomEnv: [...SETTINGS_DEFAULTS.codexCustomEnv],
    spinnerCustomVerbs: [...SETTINGS_DEFAULTS.spinnerCustomVerbs],
    spinnerDisabledAnimations: [...SETTINGS_DEFAULTS.spinnerDisabledAnimations],
  };
}

// The generated bindings return the wails model CLASS, whose field
// declarations materialize every wire-omitted optional key as an OWN
// property holding undefined. Spread copies own properties whether or
// not they hold a value, so without this strip an omitted key STOMPS
// its default with undefined instead of leaving it in place — which is
// how the untouched compaction-sprite slot ("" on our side, omitted by
// Go's omitempty) reached the resolver as undefined and fell through to
// the random pool (field bug 2026-08-22). Applies to the nested model
// classes too.
function withoutUndefined<T extends object>(value: T): T {
  const out: Record<string, unknown> = {};
  for (const [key, entry] of Object.entries(value)) {
    if (entry !== undefined) out[key] = entry;
  }
  return out as T;
}

function mergeSettingsWithDefaults(raw: Partial<Settings>): Settings {
  const defaults = defaultSettings();
  const result = withoutUndefined(raw);
  if (result.network) result.network = withoutUndefined(result.network);
  if (result.retention) result.retention = withoutUndefined(result.retention);
  return {
    ...defaults,
    ...result,
    recentWorkspaces: result.recentWorkspaces
      ? [...result.recentWorkspaces]
      : defaults.recentWorkspaces,
    network: {
      ...defaults.network,
      ...result.network,
    },
    retention: {
      ...defaults.retention,
      ...result.retention,
    },
    gitlabSelfHostedHosts: result.gitlabSelfHostedHosts
      ? [...result.gitlabSelfHostedHosts]
      : defaults.gitlabSelfHostedHosts,
    claudeHiddenModels: result.claudeHiddenModels
      ? [...result.claudeHiddenModels]
      : defaults.claudeHiddenModels,
    codexHiddenModels: result.codexHiddenModels
      ? [...result.codexHiddenModels]
      : defaults.codexHiddenModels,
    claudeCustomEnv: result.claudeCustomEnv
      ? [...result.claudeCustomEnv]
      : defaults.claudeCustomEnv,
    codexCustomEnv: result.codexCustomEnv
      ? [...result.codexCustomEnv]
      : defaults.codexCustomEnv,
    spinnerCustomVerbs: result.spinnerCustomVerbs
      ? [...result.spinnerCustomVerbs]
      : defaults.spinnerCustomVerbs,
    spinnerDisabledAnimations: result.spinnerDisabledAnimations
      ? [...result.spinnerDisabledAnimations]
      : defaults.spinnerDisabledAnimations,
  };
}

let settings: Settings = $state(defaultSettings());

export function getSettings(): Settings {
  return settings;
}

export async function loadSettings(): Promise<boolean> {
  try {
    const result = await GetSettings();
    if (result) {
      settings = mergeSettingsWithDefaults(result as Partial<Settings>);
    }
    return true;
  } catch (err) {
    console.error("Failed to load settings:", err);
    addToast("error", "Failed to load settings");
    return false;
  }
}

export async function updateSetting<K extends keyof Settings>(
  key: K,
  value: Settings[K],
): Promise<void> {
  await updateSettingsPatch({ [key]: value } as Partial<Settings>);
}

/**
 * Serializes the UpdateSettings round trips. The transport dispatches RPCs
 * concurrently per connection, and a single gesture can issue two DEPENDENT
 * whole-list writes — the System prompt section commits a textarea on blur
 * and then handles the click of the button that took the focus, each sending
 * the entire override list. Landed out of order, the earlier list is what
 * persists AND its full-snapshot answer overwrites the store, so the typing
 * disappears with no error anywhere. Only the RPC and the merge of its answer
 * are ordered; the optimistic write stays synchronous at call time.
 *
 * Null means idle, and an idle queue dispatches STRAIGHT AWAY rather than off
 * a microtask: a lone settings write is the overwhelmingly common case and
 * has nothing to be ordered against, so it should not start a turn later than
 * it used to.
 */
let updateQueue: Promise<void> | null = null;

function pickKeys(
  source: Settings,
  keys: readonly (keyof Settings)[],
): Partial<Settings> {
  const picked: Record<string, unknown> = {};
  // An absent optional key is picked as undefined ON PURPOSE: restoring it
  // has to re-erase the field, not leave the failed patch's value behind.
  for (const key of keys) picked[key] = source[key];
  return picked as Partial<Settings>;
}

export async function updateSettingsPatch(
  patch: Partial<Settings>,
): Promise<void> {
  // Captured against what this gesture actually replaced, before the RPC is
  // queued — the store's state when the answer arrives may include another
  // gesture's optimistic write, which is not ours to undo.
  const replaced = pickKeys(settings, Object.keys(patch) as (keyof Settings)[]);
  settings = { ...settings, ...patch };

  // sendSettingsPatch handles its own failure and never rejects, so a failed
  // write cannot poison the queue for every later one — which is also why the
  // idle reset below can be a plain `then`.
  const ahead = updateQueue;
  const run: Promise<void> = ahead
    ? ahead.then(() => sendSettingsPatch(patch, replaced))
    : sendSettingsPatch(patch, replaced);
  updateQueue = run;
  void run.then(() => {
    if (updateQueue === run) updateQueue = null;
  });
  return run;
}

async function sendSettingsPatch(
  patch: Partial<Settings>,
  replaced: Partial<Settings>,
): Promise<void> {
  try {
    const result = await UpdateSettings(patch);
    if (result) {
      settings = mergeSettingsWithDefaults(result as Partial<Settings>);
    }
  } catch (err) {
    console.error("Failed to update setting:", err);
    // Only the keys this patch wrote. Restoring the whole pre-patch snapshot
    // reverts every optimistic write made since, including ones whose own
    // RPC is still queued behind this one.
    settings = { ...settings, ...replaced };
    addToast("error", "Failed to save setting");
  }
}

/**
 * Converge on the backend's settings after a `settings:updated` frame (or a
 * transport gap on that channel).
 *
 * The frame names the tier and the changed KEYS, never the values: settings
 * carry redacted fields (endpoint tokens, sensitive environment values) with
 * no read path, so the wire cannot carry the new state and the client re-reads
 * the same redacted projection its own writes get back.
 *
 * Queued behind any in-flight write on the SAME queue `updateSettingsPatch`
 * uses, which is what makes this safe rather than a race: an unordered re-read
 * could be issued before a local optimistic write reached the backend and then
 * land after it, discarding the field the user just changed.
 *
 * Not coalesced. A save that moved keys in two tiers pushes two frames and so
 * costs two serialized reads of an in-memory value — the same answer either
 * way, and skipping a later frame's read is how a client ends up converged on
 * a state one write behind.
 *
 * The initiator receives its own echo and re-reads too. That costs one cheap
 * RPC and buys the guarantee that every client, initiator included, ends on
 * exactly the backend's projection — the same reasoning `thread:updated` uses
 * for broadcasting the row the RPC returned.
 */
export function resyncSettings(): Promise<void> {
  const ahead = updateQueue;
  const run: Promise<void> = ahead
    ? ahead.then(readSettingsIntoStore)
    : readSettingsIntoStore();
  updateQueue = run;
  void run.then(() => {
    if (updateQueue === run) updateQueue = null;
  });
  return run;
}

// Never rejects, for the same reason sendSettingsPatch does not: a failed read
// must not poison the shared write queue for every write behind it.
async function readSettingsIntoStore(): Promise<void> {
  try {
    const result = await GetSettings();
    if (result) {
      settings = mergeSettingsWithDefaults(result as Partial<Settings>);
    }
  } catch (err) {
    console.error('Failed to converge settings after a settings:updated event:', err);
  }
}

/**
 * Re-seeds the store from a full Settings snapshot returned by a dedicated
 * mutator (the custom-environment CRUD). Those bindings return the same
 * redacted shape GetSettings does, so the store stays consistent without a
 * second round trip — and unlike updateSettingsPatch there is no optimistic
 * pre-write, because the backend is the only side that knows what the
 * validated, deduped list looks like.
 */
export function applySettingsSnapshot(result: Partial<Settings>): void {
  settings = mergeSettingsWithDefaults(result);
}

export function resetSettingsForTest(): void {
  settings = defaultSettings();
  // The queue is module state too: a test whose RPC never settles would
  // otherwise wedge every write in every test that follows it.
  updateQueue = null;
}
