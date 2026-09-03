// The appearance selection and the theme files behind it — app-scoped
// state, RPC-loaded and push-fed.
//
// ENTITY. "Appearance" is one app-global value (the mode plus one theme id
// per axis) over one directory of files, so this is a plain module store in
// the shape of `settings.svelte.ts`, not an `entityStore` and not a
// `keyedSignalRegistry`: there is no key, and the only backend resource is a
// watcher the BACKEND owns for the process lifetime, so there is nothing to
// acquire and nothing to release.
//
// TWO CAPABILITIES, NOT ONE FLAG. The theme RPCs target the DESKTOP's own
// config dir and are classified SEPARATELY: `GetThemeFiles` is LAN-allowed,
// while `SetAppearance` and `SetWindowBackgroundColor` are local-only. A
// remote browser or a view-only `--connect` session therefore READS the themes
// directory perfectly well and is REFUSED every write. Collapsing both into
// one `available` boolean inverted the per-client residency the spec promises
// (docs/architecture/theme-system.md §6.1): every `theme:changed` refetch adopted the
// desktop's selection, clobbered the client's own choice, and re-armed a write
// path that could only fail. So the two are tracked apart:
//
//   readAvailable — did `GetThemeFiles` answer? (themes, dir, file warnings)
//   writesRefused — is `SetAppearance` known refused, or was this session
//                   never granted `settings:write` (`hasScope`)?
//
// A session with reads but no writes takes the FILES off the wire and keeps
// `localStorage` as the sole source of its selection. Nothing clears
// `writesRefused`: a refusal is structural, not transient.
//
// LOCALSTORAGE IS THIS CLIENT'S DURABLE COPY of the selection, written on
// every change even where the RPCs work — it is what a browser session
// re-reads next launch. It is NOT what the first-paint boot script reads:
// that is `theme/themeApply.svelte.ts`'s own `agent-overflow:theme:boot`
// stamp, written from the APPLIED cascade. Two keys on purpose — one records
// what was chosen, the other what it looked like.
//
// GENERATIONS. Two monotonic counters keep concurrent answers from lying:
//
//   loadGeneration  — stamped when a `GetThemeFiles` is issued. A landing
//                     answer whose stamp is not the newest is DROPPED, so two
//                     overlapping `theme:changed` refetches cannot land out of
//                     order and leave the older file content on screen.
//   writeGeneration — bumped by every local selection write. An in-flight
//                     refetch that was issued before it still contributes its
//                     FILES, but no longer adopts the wire's SELECTION: it
//                     read the file before the user's pick was persisted, and
//                     adopting it would silently revert the pick with nothing
//                     to heal it.

import { BUILTIN_CODE_THEME_ID, BUILTIN_UI_THEME_ID } from '../theme/builtins';
import {
  parseTheme,
  type ParsedTheme,
  type ThemeWarning,
} from '../theme/themeParse';
import { hasScope } from '../transport/scopes';
import { errString } from '../utils/errors';
import { SetAppearance, SetWindowBackgroundColor, ThemeAppearance, GetThemeFiles } from './bindings';
import type { ThemeFiles } from './bindings';
import { isMethodUnavailableError, onTransportStatusChange } from './transportStatus.svelte';
import { wailsEventOn } from './wailsEvents';

/** Mode enum, mirroring `internal/theme`'s `allowedModes`. */
export type AppearanceMode = 'system' | 'light' | 'dark';

/**
 * Defaults mirror `internal/theme`'s DefaultAppearance, and they are the
 * built-in ids themselves rather than a second spelling of them — a rename in
 * `theme/builtins.ts` that this file did not follow would resolve to a theme
 * that does not exist and fall back with a warning.
 */
export const DEFAULT_UI_THEME = BUILTIN_UI_THEME_ID;
export const DEFAULT_CODE_THEME = BUILTIN_CODE_THEME_ID;

/**
 * The selection as the frontend holds it: every field present, so no
 * consumer has to re-apply a default the backend already applies.
 */
export interface AppearanceSelection {
  readonly mode: AppearanceMode;
  readonly uiTheme: string;
  readonly codeTheme: string;
  /**
   * Cached native-window ground, `#rrggbb`. Maintained by the applier, never
   * user-edited — see `internal/theme`'s Appearance doc comment.
   */
  readonly windowBackground: string;
}

const DEFAULT_SELECTION: AppearanceSelection = {
  mode: 'system',
  uiTheme: DEFAULT_UI_THEME,
  codeTheme: DEFAULT_CODE_THEME,
  windowBackground: '',
};

/** This client's durable copy of the selection. */
const STORAGE_KEY = 'agent-overflow:appearance';

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const INITIAL_SELECTION = readLocalSelection();

let selection = $state.raw<AppearanceSelection>(INITIAL_SELECTION);
/**
 * The mode, in a box of its own.
 *
 * Deliberately not derived from `selection` at the read site: `themeMode`'s
 * `getResolvedTheme()` is read by every palette consumer in the app, and
 * reading the whole selection box there made the applier's `windowBackground`
 * cache write — a value no consumer of the MODE can observe — wake every one
 * of them for a full re-resolve plus a `CSS.supports` pass that settled
 * identical. One narrow box costs one assignment per mode change.
 */
let selectionMode = $state<AppearanceMode>(INITIAL_SELECTION.mode);
let themes = $state.raw<readonly ParsedTheme[]>([]);
let fileWarnings = $state.raw<readonly string[]>([]);
let directory = $state('');
let readAvailable = $state(true);
let writesRefused = $state(false);
let loaded = $state(false);
/** A transient load failure, as a sentence. Null when there is nothing wrong. */
let loadError = $state<string | null>(null);
// Bumped when an answer actually CHANGED a theme file's content, never merely
// because an answer arrived. It is the third component of the palette
// identity, and every bump remounts every mermaid diagram ({#key}) and
// re-atlases every terminal — so a boot with no user themes, or a
// `theme:changed` for an unrelated file, must not move it.
let revision = $state(0);

let loadGeneration = 0;
let writeGeneration = 0;
let filesStamp = digestFiles([]);

export function getAppearance(): AppearanceSelection {
  return selection;
}

/**
 * The selected mode alone. `themeMode.svelte.ts` reads this rather than
 * `getAppearance().mode` — see `selectionMode`'s comment.
 */
export function getAppearanceMode(): AppearanceMode {
  return selectionMode;
}

export function getAppearanceThemes(): readonly ParsedTheme[] {
  return themes;
}

/** Backend-side file warnings (unreadable, oversized, badly named). */
export function getAppearanceFileWarnings(): readonly string[] {
  return fileWarnings;
}

/** Absolute path of `<configDir>/themes`, or '' in a degraded session. */
export function getThemeDirectory(): string {
  return directory;
}

/**
 * False when this session has no themes directory to read: the theme files
 * RPC answered `method_not_found`, so only the built-ins are selectable.
 * Says nothing about whether the selection can be PERSISTED — that is
 * {@link isAppearanceWritable}.
 */
export function isThemeDirectoryAvailable(): boolean {
  return readAvailable;
}

/**
 * Whether this session can persist a selection to the desktop's config dir.
 * False in a view-only session and after any refused write; the selection
 * still applies and is still remembered in `localStorage`.
 */
export function isAppearanceWritable(): boolean {
  return !writesBlocked();
}

/** A transient failure to read the theme files, as a user-facing sentence. */
export function getAppearanceLoadError(): string | null {
  return loadError;
}

/**
 * Whether the first load attempt has completed — successfully, refused, or
 * failed. The applier gates its FIRST write on this: until the answer is in,
 * a selected user theme resolves to the built-in fallback, and writing that
 * would overwrite the boot script's cached first-paint CSS with the very
 * flash the stamp exists to prevent.
 */
export function isAppearanceLoaded(): boolean {
  return loaded;
}

export function getAppearanceRevision(): number {
  return revision;
}

function writesBlocked(): boolean {
  return writesRefused || !hasScope('settings:write');
}

// ---------------------------------------------------------------------------
// localStorage
// ---------------------------------------------------------------------------

function isMode(value: unknown): value is AppearanceMode {
  return value === 'system' || value === 'light' || value === 'dark';
}

/**
 * Theme-id shape, mirroring `internal/theme`'s idPattern. Applied to values
 * coming back out of localStorage as well as off the wire: a hand-edited
 * origin store must not be able to put something un-CSS-safe into a
 * selection the applier will look up.
 */
const ID_PATTERN = /^[a-z0-9][a-z0-9-]{0,63}$/;
const HEX_PATTERN = /^#[0-9a-fA-F]{6}$/;

function normalizeSelection(raw: Partial<Record<keyof AppearanceSelection, unknown>>): AppearanceSelection {
  const mode = isMode(raw.mode) ? raw.mode : DEFAULT_SELECTION.mode;
  const uiTheme =
    typeof raw.uiTheme === 'string' && ID_PATTERN.test(raw.uiTheme)
      ? raw.uiTheme
      : DEFAULT_SELECTION.uiTheme;
  const codeTheme =
    typeof raw.codeTheme === 'string' && ID_PATTERN.test(raw.codeTheme)
      ? raw.codeTheme
      : DEFAULT_SELECTION.codeTheme;
  const windowBackground =
    typeof raw.windowBackground === 'string' && HEX_PATTERN.test(raw.windowBackground)
      ? raw.windowBackground
      : '';
  return { mode, uiTheme, codeTheme, windowBackground };
}

function readLocalSelection(): AppearanceSelection {
  if (typeof localStorage === 'undefined') return DEFAULT_SELECTION;
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return DEFAULT_SELECTION;
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== 'object' || parsed === null) return DEFAULT_SELECTION;
    return normalizeSelection(parsed as Record<string, unknown>);
  } catch {
    return DEFAULT_SELECTION;
  }
}

function writeLocalSelection(value: AppearanceSelection): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(value));
  } catch {
    // Best-effort: the config file is the durable copy where there is one,
    // and a browser session that cannot persist just re-picks next launch.
  }
}

/** The ONE selection write. Keeps the narrow mode box from ever drifting. */
function setSelection(next: AppearanceSelection): void {
  selection = next;
  if (selectionMode !== next.mode) selectionMode = next.mode;
  writeLocalSelection(next);
}

// ---------------------------------------------------------------------------
// Content identity
// ---------------------------------------------------------------------------

interface WireThemeFile {
  readonly id: string;
  readonly raw: string;
}

function hashInto(hash: number, text: string): number {
  let next = hash;
  for (let i = 0; i < text.length; i += 1) {
    next ^= text.charCodeAt(i);
    next = Math.imul(next, 16777619);
  }
  return next >>> 0;
}

/**
 * A digest of what the files SAY, not of the fact that an answer arrived.
 *
 * The SELECTION is deliberately not folded in. The palette identity is
 * `uiTheme|codeTheme|revision`, so a selection change already moves it through
 * its first two components; adding it here would bump the revision as well and
 * charge a selection change two remounts instead of one.
 */
function digestFiles(files: readonly WireThemeFile[]): string {
  // Length-prefixed, so no separator has to be chosen that a raw file could
  // contain: `{id: 'ab', raw: 'c'}` and `{id: 'a', raw: 'bc'}` hash apart.
  let hash = 2166136261;
  for (const file of files) {
    hash = hashInto(hash, `${file.id.length}:${file.id}${file.raw.length}:${file.raw}`);
  }
  return `${files.length}:${hash.toString(16)}`;
}

function bumpRevisionIfContentMoved(files: readonly WireThemeFile[]): void {
  const stamp = digestFiles(files);
  if (stamp === filesStamp) return;
  filesStamp = stamp;
  revision += 1;
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

function applyFiles(files: ThemeFiles, adoptSelection: boolean): void {
  const accepted: WireThemeFile[] = [];
  const parsed: ParsedTheme[] = [];
  const rejected: string[] = [];
  for (const file of files.themes ?? []) {
    if (!file || typeof file.id !== 'string' || typeof file.raw !== 'string') continue;
    // The id becomes a selection value, a catalog key and a CSS-adjacent
    // lookup, and the store is the last line before all three — the same
    // doctrine `normalizeSelection` applies to the selection itself.
    if (!ID_PATTERN.test(file.id)) {
      rejected.push(
        `"${file.id.slice(0, 64)}" is not a usable theme id (lowercase letters, digits and dashes, 64 characters at most), so that file was skipped.`,
      );
      continue;
    }
    accepted.push({ id: file.id, raw: file.raw });
    parsed.push(parseTheme(file.id, file.raw));
  }

  themes = parsed;
  directory = files.dir ?? '';
  fileWarnings = [...(files.warnings ?? []), ...rejected];
  if (adoptSelection) {
    setSelection(normalizeSelection((files.appearance ?? {}) as Record<string, unknown>));
  }
  readAvailable = true;
  loaded = true;
  loadError = null;
  bumpRevisionIfContentMoved(accepted);
}

/**
 * The read was REFUSED: this session has no themes directory at all. Latching
 * is correct here — `GetThemeFiles` is the LAN-allowed half of the pair, so a
 * session refused the read is refused both writes too.
 */
function degradeRefused(): void {
  themes = [];
  directory = '';
  fileWarnings = [];
  readAvailable = false;
  writesRefused = true;
  loaded = true;
  loadError = null;
  bumpRevisionIfContentMoved([]);
}

/**
 * The read FAILED — a dropped socket, a timeout, a backend error.
 *
 * Nothing latches. A WebSocket blip during boot is not a reason to stop
 * persisting for the rest of the session, and it is not evidence that the
 * themes directory is gone: whatever files were already loaded stay in use,
 * the failure becomes user-facing state, and the next `theme:changed` or
 * transport reconnect retries.
 */
function degradeFailed(err: unknown): void {
  loaded = true;
  loadError = `The theme files could not be read (${errString(err)}). The app is using what it last loaded.`;
}

/**
 * Loads the theme files and the selection. Never rejects: an unavailable RPC
 * degrades to built-ins, and any other failure is reported as state and
 * retried later — a theme system that cannot load must not stop the app from
 * rendering.
 */
export async function loadAppearance(): Promise<void> {
  const generation = (loadGeneration += 1);
  const writesAtIssue = writeGeneration;
  try {
    const files = await GetThemeFiles();
    if (generation !== loadGeneration) return;
    // The FILES always land; the SELECTION only when this answer is still the
    // newest word on it — not when the session cannot persist a selection at
    // all (the wire's copy is then another machine's), and not when a local
    // pick was made after this request went out (the answer predates it).
    applyFiles(files, !writesBlocked() && writeGeneration === writesAtIssue);
  } catch (err) {
    if (generation !== loadGeneration) return;
    if (isMethodUnavailableError(err)) {
      degradeRefused();
      return;
    }
    console.warn('theme files could not be read; keeping the loaded themes', err);
    degradeFailed(err);
  }
}

/**
 * Subscribes to the backend's themes-directory watcher, and to the transport
 * edge behind it.
 *
 * Every watcher fire refetches, which is the agent-edit loop: write the file,
 * see the app repaint. The reconnect leg is what makes that loop survive an
 * outage — a `theme:changed` emitted while the socket was down is simply gone,
 * so the app would otherwise render a stale palette until something else
 * happened to reload.
 */
export function installAppearanceEvents(): () => void {
  const stopWatcher = wailsEventOn('theme:changed', () => {
    void loadAppearance();
  });
  let wasDown = false;
  const stopEdge = onTransportStatusChange((snapshot) => {
    if (snapshot.status !== 'connected') {
      wasDown = true;
      return;
    }
    if (!wasDown) return;
    wasDown = false;
    void loadAppearance();
  });
  return () => {
    stopWatcher();
    stopEdge();
  };
}

// ---------------------------------------------------------------------------
// Write
// ---------------------------------------------------------------------------

function replacedKeys(
  previous: AppearanceSelection,
  patch: Partial<AppearanceSelection>,
): Partial<AppearanceSelection> {
  const picked: Record<string, unknown> = {};
  for (const key of Object.keys(patch) as (keyof AppearanceSelection)[]) {
    picked[key] = previous[key];
  }
  return picked as Partial<AppearanceSelection>;
}

/**
 * Applies a partial selection optimistically and persists it.
 *
 * The optimistic write is what makes a theme swap feel instant — the applier
 * re-resolves from this state, not from the RPC's answer. A refused or failed
 * write restores exactly the keys this call replaced, OVER LIVE STATE, in the
 * shape `settings.svelte.ts` established: `syncWindowBackground` is a second
 * writer on the same object, so restoring a whole pre-call snapshot would
 * revert a mode change that landed while this RPC was in flight.
 */
export async function setAppearance(patch: Partial<AppearanceSelection>): Promise<void> {
  const previous = selection;
  const next = normalizeSelection({ ...selection, ...patch });
  if (
    next.mode === previous.mode &&
    next.uiTheme === previous.uiTheme &&
    next.codeTheme === previous.codeTheme &&
    next.windowBackground === previous.windowBackground
  ) {
    return;
  }
  // Supersede the selection half of any refetch already in flight: it read the
  // file before this pick existed.
  writeGeneration += 1;
  setSelection(next);

  if (writesBlocked()) return;
  try {
    await SetAppearance(toWire(next));
  } catch (err) {
    if (isMethodUnavailableError(err)) {
      // A read-only session: keep the local choice (it is the only copy that
      // can exist here) and stop trying to persist it.
      writesRefused = true;
      return;
    }
    console.error('Failed to save appearance:', err);
    writeGeneration += 1;
    setSelection(normalizeSelection({ ...selection, ...replacedKeys(previous, patch) }));
  }
}

function toWire(value: AppearanceSelection): ThemeAppearance {
  return new ThemeAppearance({
    mode: value.mode,
    uiTheme: value.uiTheme,
    codeTheme: value.codeTheme,
    // Absent means "no cached value yet" on the Go side, so an empty string
    // must not be sent as a value that would fail the hex check.
    ...(value.windowBackground ? { windowBackground: value.windowBackground } : {}),
  });
}

/**
 * Paints the native window ground and caches it for the NEXT launch's window
 * construction, but only when it actually moved.
 *
 * Both halves are deliberate and deliberately separate (see
 * `app_appearance.go#SetWindowBackgroundColor`): the RPC is the live repaint, the
 * persisted value is what `main_desktop.go` reads before a webview exists.
 * The change guard is what keeps this off the write path entirely for the
 * overwhelmingly common case of a theme that did not move the ground —
 * without it, every re-resolve would be an RPC and a file write.
 */
export async function syncWindowBackground(hex: string): Promise<void> {
  if (!HEX_PATTERN.test(hex)) return;
  if (writesBlocked()) return;
  if (hex === selection.windowBackground) return;
  try {
    await SetWindowBackgroundColor(hex);
  } catch (err) {
    if (!isMethodUnavailableError(err)) {
      console.warn('Failed to paint the native window background:', err);
    }
    // A refused paint does not stop the cache from being useful next launch,
    // but a refused paint in a remote session means the write below is
    // refused too — setAppearance latches on that answer.
  }
  await setAppearance({ windowBackground: hex });
}

// ---------------------------------------------------------------------------
// Warnings
// ---------------------------------------------------------------------------

/**
 * Every parse warning the loaded files produced, whether or not the file is
 * selected. Settings → Theme renders these grouped per file, which is
 * what answers the "my theme does nothing" case for a file that is not the
 * one applied — the applier only ever sees the two selected.
 */
export function getThemeParseWarnings(): readonly ThemeWarning[] {
  const out: ThemeWarning[] = [];
  for (const theme of themes) out.push(...theme.warnings);
  return out;
}

/** Test-only reset. */
export function resetAppearanceForTest(): void {
  selection = DEFAULT_SELECTION;
  selectionMode = DEFAULT_SELECTION.mode;
  themes = [];
  fileWarnings = [];
  directory = '';
  readAvailable = true;
  writesRefused = false;
  loaded = false;
  loadError = null;
  revision = 0;
  loadGeneration = 0;
  writeGeneration = 0;
  filesStamp = digestFiles([]);
}
