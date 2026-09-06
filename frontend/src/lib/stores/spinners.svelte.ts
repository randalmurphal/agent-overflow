// Custom working-indicator sprites — one frontend-owned list, mirroring the
// themes pipeline. appearanceFiles.ts selects the local editable directory or
// this browser/phone's durable library. The backend lists
// <configDir>/spinners/ `<id>.png` + `<id>.json` pairs as opaque bytes
// (internal/spinner), this store parses the manifest, decodes the strip
// to learn its frame geometry, and every consumer (the activity rail's
// pool, Settings → Working indicator's pool list) derives from the one entry.
//
// Doctrine (frontend/CLAUDE.md → State Boundaries): the entity is the
// APP — the directory listing takes no key and a `spinner:changed` from
// the directory watcher invalidates the whole answer. Same shape as
// chatBarFavorites: one permanent hold, one loader, one retry curve.
//
// A bad file costs exactly that sprite: manifest and geometry failures
// land in `warnings` beside the usable rest (the pure rules live in
// lib/spinners/customs.ts), matching the themes principle that a broken
// user file must say so in the UI rather than silently doing nothing.

import { readSpinnerFiles, usesFrontendAssetLibrary } from './appearanceFiles';
import { onFrontendAssetsChanged } from './frontendAssets';
import { wailsEventOn } from './wailsEvents';
import type { SpinnerSprite } from '../spinners/catalog';
import { buildCustomSprite, parseCustomManifest, pngDimensions, MAX_LIBRARY_PIXELS } from '../spinners/customs';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';

const KEY = 'app';

export interface CustomSpinners {
  dir: string;
  sprites: SpinnerSprite[];
  warnings: string[];
}

// Sprite.PNG is a base64 STRING on the wire by contract
// (internal/spinner/AGENTS.md — deliberately not []byte, so the generated
// binding type and the runtime value agree). No byte-array branch here:
// one would be dead code, and a per-byte string build over a 4 MiB strip
// would hitch the main thread if it ever ran.
interface WireSprite {
  id: string;
  manifest: string;
  png: string;
}

interface WireFiles {
  dir?: string;
  sprites?: WireSprite[] | null;
  warnings?: string[] | null;
}

function pngDataUrl(png: string): string {
  return `data:image/png;base64,${png}`;
}

/** Decode one strip to learn its pixel size. Never rejects: a failed
 * decode answers 0×0, which buildCustomSprite turns into a warning. */
function imageSize(src: string, signal: AbortSignal): Promise<{ width: number; height: number }> {
  return new Promise((resolve) => {
    if (signal.aborted) { resolve({ width: 0, height: 0 }); return; }
    const image = new Image();
    const finish = (width = 0, height = 0): void => {
      clearTimeout(timer);
      signal.removeEventListener('abort', aborted);
      image.onload = null;
      image.onerror = null;
      image.src = '';
      resolve({ width, height });
    };
    const aborted = (): void => finish();
    const timer = setTimeout(() => finish(), 5_000);
    signal.addEventListener('abort', aborted, { once: true });
    image.onload = () => finish(image.naturalWidth, image.naturalHeight);
    image.onerror = () => finish();
    image.src = src;
  });
}

async function fetchCustomSpinners(signal: AbortSignal): Promise<CustomSpinners> {
  const wire = ((await readSpinnerFiles()) ?? {}) as WireFiles;
  const result: CustomSpinners = {
    dir: wire.dir ?? '',
    sprites: [],
    warnings: [...(wire.warnings ?? [])],
  };
  let pixels = 0;
  for (const entry of wire.sprites ?? []) {
    if (signal.aborted) break;
    const manifest = parseCustomManifest(entry.id, entry.manifest);
    if (typeof manifest === 'string') {
      result.warnings.push(manifest);
      continue;
    }
    const dimensions = pngDimensions(entry.png);
    if (!dimensions) {
      result.warnings.push(`${entry.id}.png: invalid PNG or image dimensions exceed the animation memory limit`);
      continue;
    }
    const area = dimensions.width * dimensions.height;
    if (pixels + area > MAX_LIBRARY_PIXELS) {
      result.warnings.push(`${entry.id}.png: the animation library exceeds its memory limit`);
      continue;
    }
    const src = pngDataUrl(entry.png);
    const { width, height } = await imageSize(src, signal);
    if (signal.aborted) break;
    const sprite = buildCustomSprite(entry.id, manifest, src, width, height);
    if (typeof sprite === 'string') {
      result.warnings.push(sprite);
      continue;
    }
    result.sprites.push(sprite);
    pixels += area;
  }
  return result;
}

const store = createEntityStore<CustomSpinners, void>({
  name: 'spinners',
  backendForKey: () => usesFrontendAssetLibrary() ? null : '',
  source: async ({ apply, signal }) => {
    apply(await fetchCustomSpinners(signal));
    // Nothing to release: the watcher lives backend-side and pushes
    // `spinner:changed`; the subscription below invalidates.
    return () => {};
  },
});

// Directory watcher push: any change in <configDir>/spinners/ re-lists.
// Module-scope subscription — the store is app-global and the event is
// entity-keyed to the app, so there is nothing to scope it to.
wailsEventOn('spinner:changed', () => {
  if (hold !== null && !usesFrontendAssetLibrary()) store.invalidate(KEY);
});
onFrontendAssetsChanged((kind) => {
  if (kind === 'spinners' && hold !== null && usesFrontendAssetLibrary()) store.invalidate(KEY);
});

let hold: EntityAttachment<CustomSpinners> | null = null;

/** Load the list once, lazily. Safe to call from every consumer. */
export function ensureCustomSpinners(): void {
  hold ??= store.attach(KEY, undefined);
}

const EMPTY: CustomSpinners = { dir: '', sprites: [], warnings: [] };

/** Reactive read; empty until the first load resolves. */
export function peekCustomSpinners(): CustomSpinners {
  return store.peek(KEY) ?? EMPTY;
}

export function customSpinnersError(): string | null { return store.peekError(KEY); }

/** Test seam: drop the entry and the hold, as a fresh module load would. */
export function __resetCustomSpinnersForTest(): void {
  hold?.release();
  hold = null;
  store.suspend();
  store.resetAll();
}
