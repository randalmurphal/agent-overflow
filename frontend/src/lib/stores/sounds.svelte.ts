// The custom notification-cue library — one listing of the BACKEND HOST's
// <configDir>/sounds directory, shared by every screen attached to it.
//
// Unlike themes and spinners this is not a frontend-owned appearance library
// (appearanceFiles.ts): a cue is picked by a settings key the backend reads
// when it decides to interrupt someone, and a paired phone choosing a sound
// its host has never heard of would be choosing nothing. So the entity is the
// APP, the backend is HOME, and `sound:changed` from the directory watcher
// invalidates the whole answer.
//
// Go validates every file on every listing (internal/soundlib) and returns
// the reasons beside the usable rest, so a cue that cannot be played says so
// in Settings rather than silently doing nothing.
//
// OBJECT URLS ARE OWNED HERE. Each cue arrives as base64 and becomes one Blob
// and one object URL, which the player hands to an <audio> element. A blob
// URL PINS ITS BYTES until it is revoked, so a listing's URLs are revoked the
// moment a newer listing has replaced it — never before, because the value
// being replaced is still on screen until then. Spinners need none of this:
// their strips travel as data: URLs, which own nothing.

import { DeleteSoundFile, GetSoundFiles, PutSoundFile } from './bindings';
import { wailsEventOn } from './wailsEvents';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';

const KEY = 'app';

export interface CustomSound {
  id: string;
  /** Object URL over the decoded WAV bytes. Revoked when the listing changes. */
  url: string;
}

export interface CustomSounds {
  dir: string;
  sounds: CustomSound[];
  warnings: string[];
}

// Sound.WAV is a base64 STRING on the wire by contract
// (internal/soundlib/AGENTS.md): the generated binding type and the runtime
// value agree, so there is no byte-array branch here to be dead code.
interface WireSound {
  id: string;
  wav: string;
}

interface WireFiles {
  dir?: string;
  sounds?: WireSound[] | null;
  warnings?: string[] | null;
}

const EMPTY: CustomSounds = { dir: '', sounds: [], warnings: [] };

/** The listing currently held by the store, for the revoke bookkeeping. */
let current: CustomSounds | null = null;

const listeners = new Set<() => void>();

function revokeSounds(listing: CustomSounds): void {
  for (const sound of listing.sounds) URL.revokeObjectURL(sound.url);
}

function announce(): void {
  for (const listener of listeners) listener();
}

/**
 * Decode one base64 cue into bytes. A body that is not base64 cannot come
 * from the backend — it validated the file it encoded — so this is the
 * defensive arm for a mangled frame, and it answers null rather than throwing
 * so one bad row cannot erase the rest of the library.
 */
function decodeWav(wav: string): Uint8Array | null {
  try {
    const binary = atob(wav);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
    return bytes;
  } catch {
    return null;
  }
}

function toCustomSounds(wire: WireFiles): CustomSounds {
  const result: CustomSounds = {
    dir: wire.dir ?? '',
    sounds: [],
    warnings: [...(wire.warnings ?? [])],
  };
  for (const entry of wire.sounds ?? []) {
    const bytes = decodeWav(entry.wav);
    if (!bytes) {
      result.warnings.push(`${entry.id}: skipped, the sound did not arrive intact`);
      continue;
    }
    result.sounds.push({
      id: entry.id,
      url: URL.createObjectURL(new Blob([bytes as BlobPart], { type: 'audio/wav' })),
    });
  }
  return result;
}

const store = createEntityStore<CustomSounds, void>({
  name: 'sounds',
  backendForKey: () => '',
  source: async ({ apply }) => {
    const listing = toCustomSounds(((await GetSoundFiles()) ?? {}) as WireFiles);
    apply(listing);
    // A SUPERSEDED run's apply is dropped by the store, which would strand
    // the URLs this run just minted: nothing references them, and nothing
    // will ever replace them. `apply` is synchronous, so one identity check
    // right here is the whole answer.
    if (current !== listing) revokeSounds(listing);
    // Nothing to release at the backend; the watcher lives there and pushes
    // `sound:changed`, which the subscription below turns into a re-list.
    return () => {};
  },
  onApply: (_key, listing, previous) => {
    current = listing;
    if (previous) revokeSounds(previous);
    announce();
  },
  // Last release, suspend, or a resetAll nobody held through: the listing is
  // gone, so nothing references its URLs any more.
  onDrop: () => {
    if (current) revokeSounds(current);
    current = null;
    announce();
  },
});

let hold: EntityAttachment<CustomSounds> | null = null;
let cancelWatch: (() => void) | null = null;

/** Load the list once, lazily. Safe to call from every consumer. */
export function ensureCustomSounds(): void {
  if (hold !== null) return;
  // The watcher nudge is subscribed WITH the hold rather than at module
  // load: nothing can act on a directory change before something is holding
  // the listing, and a screen that never plays or configures a cue never
  // subscribes at all.
  cancelWatch = wailsEventOn('sound:changed', () => {
    store.invalidate(KEY);
  });
  hold = store.attach(KEY, undefined);
}

/** Reactive read; empty until the first load resolves. */
export function peekCustomSounds(): CustomSounds {
  return store.peek(KEY) ?? EMPTY;
}

/**
 * Whether the first listing has arrived.
 *
 * Settings needs it to tell "this backend has no cue by that name" from "this
 * screen has not asked yet": both read as an empty library, and only the first
 * is worth warning a user about.
 */
export function customSoundsLoaded(): boolean {
  return store.peek(KEY) !== null;
}

export function customSoundsError(): string | null {
  return store.peekError(KEY);
}

/**
 * The playable URL for one cue id, or null when this backend's library does
 * not have it.
 *
 * NON-REACTIVE: the caller is the player, which reads a URL only to start a
 * sound. Subscribing it to the listing would wake an audio-element cache on
 * every directory change for no benefit — the player drops its cached
 * elements from `onCustomSoundsChanged` instead.
 */
export function customSoundUrl(id: string): string | null {
  return current?.sounds.find((sound) => sound.id === id)?.url ?? null;
}

/**
 * Register for "the listing was replaced". The player uses it to drop cached
 * <audio> elements whose object URL has just been revoked; keeping one would
 * mean a cue that plays nothing forever after an unrelated edit.
 */
export function onCustomSoundsChanged(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/**
 * Encode rendered cue bytes for the wire.
 *
 * Chunked because `String.fromCharCode(...bytes)` spreads one argument per
 * byte: a 3-second cue is 264644 of them, which overflows the call stack on
 * every engine. 32 KiB per call is well inside every limit.
 */
function encodeWav(wav: Uint8Array): string {
  let binary = '';
  const chunk = 0x8000;
  for (let i = 0; i < wav.length; i += chunk) {
    binary += String.fromCharCode(...wav.subarray(i, i + chunk));
  }
  return btoa(binary);
}

/**
 * Store one rendered cue on the backend host under `id`.
 *
 * Rejects on refusal — a duplicate id, a file the host does not accept as a
 * canonical cue, a directory it cannot write — and the caller shows the
 * message. The listing is re-read on success rather than patched locally: the
 * host is what decides which files are playable, and this screen has no way
 * to know what it just did to a directory it does not own.
 */
export async function addCustomSound(id: string, wav: Uint8Array): Promise<void> {
  await PutSoundFile(id, encodeWav(wav));
  store.invalidate(KEY);
}

/** Remove one cue from the backend host's library. Rejects on refusal. */
export async function deleteCustomSound(id: string): Promise<void> {
  await DeleteSoundFile(id);
  store.invalidate(KEY);
}

/**
 * Test seam: drop the entry and the hold, as a fresh module load would.
 * `listeners` is deliberately NOT cleared — the player registers its one
 * listener at module scope, and a reset that dropped it would leave the
 * player holding revoked URLs for the rest of the run.
 */
export function __resetCustomSoundsForTest(): void {
  cancelWatch?.();
  cancelWatch = null;
  hold?.release();
  hold = null;
  store.suspend();
  store.resetAll();
}
