// Custom working-indicator sprites — one app-global list, mirroring the
// themes pipeline one directory over: the backend lists
// <configDir>/spinners/ `<id>.png` + `<id>.json` pairs as opaque bytes
// (internal/spinner), this store parses the manifest, decodes the strip
// to learn its frame geometry, and every consumer (the activity rail's
// pool, Settings → Appearance's pool list) derives from the one entry.
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

import { GetSpinnerFiles } from './bindings';
import { wailsEventOn } from './wailsEvents';
import type { SpinnerSprite } from '../spinners/catalog';
import { buildCustomSprite, parseCustomManifest } from '../spinners/customs';
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
function imageSize(src: string): Promise<{ width: number; height: number }> {
  return new Promise((resolve) => {
    const image = new Image();
    image.onload = () => resolve({ width: image.naturalWidth, height: image.naturalHeight });
    image.onerror = () => resolve({ width: 0, height: 0 });
    image.src = src;
  });
}

async function fetchCustomSpinners(): Promise<CustomSpinners> {
  const wire = ((await GetSpinnerFiles()) ?? {}) as WireFiles;
  const result: CustomSpinners = {
    dir: wire.dir ?? '',
    sprites: [],
    warnings: [...(wire.warnings ?? [])],
  };
  for (const entry of wire.sprites ?? []) {
    const manifest = parseCustomManifest(entry.id, entry.manifest);
    if (typeof manifest === 'string') {
      result.warnings.push(manifest);
      continue;
    }
    const src = pngDataUrl(entry.png);
    const { width, height } = await imageSize(src);
    const sprite = buildCustomSprite(entry.id, manifest, src, width, height);
    if (typeof sprite === 'string') {
      result.warnings.push(sprite);
      continue;
    }
    result.sprites.push(sprite);
  }
  return result;
}

const store = createEntityStore<CustomSpinners, void>({
  name: 'spinners',
  source: async ({ apply }) => {
    apply(await fetchCustomSpinners());
    // Nothing to release: the watcher lives backend-side and pushes
    // `spinner:changed`; the subscription below invalidates.
    return () => {};
  },
});

// Directory watcher push: any change in <configDir>/spinners/ re-lists.
// Module-scope subscription — the store is app-global and the event is
// entity-keyed to the app, so there is nothing to scope it to.
wailsEventOn('spinner:changed', () => {
  if (hold !== null) store.invalidate(KEY);
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

/** Test seam: drop the entry and the hold, as a fresh module load would. */
export function __resetCustomSpinnersForTest(): void {
  hold?.release();
  hold = null;
  store.suspend();
  store.resetAll();
}
