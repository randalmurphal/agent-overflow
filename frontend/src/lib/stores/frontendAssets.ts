// Durable presentation files owned by this browser/shell, independent of the
// execution-computer cache. Forgetting a computer must never delete these.
import type { File as ThemeFile } from '../../../bindings/agent-overflow/internal/theme/models';
import type { Sprite } from '../../../bindings/agent-overflow/internal/spinner/models';
import { randomId } from '../utils/randomId';

export type AssetKind = 'themes' | 'spinners';
export interface AssetFiles {
  themes: { themes: ThemeFile[]; warnings: string[] };
  spinners: { sprites: Sprite[]; warnings: string[] };
}
export const FRONTEND_ASSETS_DB = 'agent-overflow:frontend-assets';
const STORE = 'files';
const TIMEOUT_MS = 2_000;
const ID = /^[a-z0-9][a-z0-9-]{0,63}$/;
const CHANGE_KEY = 'agent-overflow:frontend-assets-changed';
const listeners = new Set<(kind: AssetKind) => void>();

function notify(kind: AssetKind): void {
  for (const listener of listeners) listener(kind);
}
if (typeof window !== 'undefined') {
  window.addEventListener('storage', (event) => {
    if (event.key !== CHANGE_KEY) return;
    const kind = event.newValue?.split(':', 1)[0];
    if (kind === 'themes' || kind === 'spinners') notify(kind);
  });
}

export function onFrontendAssetsChanged(listener: (kind: AssetKind) => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid appearance files.');
  return value as Record<string, unknown>;
}
function text(value: unknown, max: number): string {
  if (typeof value !== 'string' || value.length > max) throw new Error('An appearance file exceeds its size limit.');
  return value;
}

/** Match the host file budgets. Validate both imports and persisted records;
 * keep only file content, never the source's selected theme or filesystem path. */
export function validateAssetFiles<K extends AssetKind>(kind: K, value: unknown): AssetFiles[K] {
  const source = record(value);
  const warnings = source.warnings ?? [];
  if (!Array.isArray(warnings) || warnings.length > 256) throw new Error('Invalid appearance file warnings.');
  const checkedWarnings = warnings.map((value) => text(value, 2_048));
  const files = source[kind === 'themes' ? 'themes' : 'sprites'] ?? [];
  if (!Array.isArray(files) || files.length > (kind === 'themes' ? 64 : 32)) throw new Error('Too many appearance files.');
  const ids = new Set<string>();
  let bytes = 0;
  const encoder = new TextEncoder();
  const checked = files.map((value) => {
    const file = record(value);
    const id = text(file.id, 64);
    if (!ID.test(id) || ids.has(id)) throw new Error('Appearance files contain an invalid or duplicate name.');
    ids.add(id);
    if (kind === 'themes') {
      const raw = text(file.raw, 1_048_576);
      const size = encoder.encode(raw).byteLength;
      if (size > 1_048_576) throw new Error(`${id}.json exceeds 1 MiB.`);
      bytes += size;
      return { id, raw };
    }
    const manifest = text(file.manifest, 16_384);
    const png = text(file.png, 4 * Math.ceil(4_194_304 / 3));
    if (encoder.encode(manifest).byteLength > 16_384 || png.length % 4 !== 0 || !/^[A-Za-z0-9+/]+={0,2}$/.test(png)) {
      throw new Error(`${id}: invalid sprite files.`);
    }
    const pngBytes = png.length / 4 * 3 - (png.endsWith('==') ? 2 : png.endsWith('=') ? 1 : 0);
    if (pngBytes > 4_194_304) throw new Error(`${id}.png exceeds 4 MiB.`);
    bytes += pngBytes + encoder.encode(manifest).byteLength;
    return { id, manifest, png };
  });
  if (bytes > (kind === 'themes' ? 4_194_304 : 25_165_824)) throw new Error('The appearance library exceeds its size limit.');
  return (kind === 'themes'
    ? { themes: checked, warnings: checkedWarnings }
    : { sprites: checked, warnings: checkedWarnings }) as AssetFiles[K];
}

/** One bounded transaction, closing even an open request that answers late.
 * A failed/aborted replacement leaves the prior library intact. */
function access<K extends AssetKind>(kind: K, write?: AssetFiles[K], onlyIfMissing = false): Promise<unknown> {
  return new Promise((resolve, reject) => {
    let db: IDBDatabase | undefined;
    let tx: IDBTransaction | undefined;
    let settled = false;
    const finish = (error: unknown, value?: unknown): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      db?.close();
      if (error) reject(error); else resolve(value);
    };
    const timer = setTimeout(() => {
      try { tx?.abort(); } catch { /* already finished */ }
      finish(new Error('Device appearance storage did not respond. Try again.'));
    }, TIMEOUT_MS);
    try {
      const open = indexedDB.open(FRONTEND_ASSETS_DB, 1);
      open.onupgradeneeded = () => {
        if (settled) { open.transaction?.abort(); return; }
        open.result.createObjectStore(STORE);
      };
      open.onerror = () => finish(open.error);
      open.onsuccess = () => {
        db = open.result;
        if (settled) { db.close(); return; }
        db.onversionchange = () => db?.close();
        try {
          tx = db.transaction(STORE, write ? 'readwrite' : 'readonly');
          const store = tx.objectStore(STORE);
          let answer: unknown;
          const read = store.get(kind);
          read.onsuccess = () => {
            try {
              answer = read.result;
              if (write && (!onlyIfMissing || answer === undefined)) {
                store.put(write, kind);
                answer = write;
              }
            } catch (error) {
              try { tx?.abort(); } catch { /* already finished */ }
              finish(error);
            }
          };
          tx.oncomplete = () => finish(null, answer);
          tx.onabort = () => finish(tx?.error ?? new Error('Device appearance storage could not be saved.'));
          tx.onerror = () => { /* abort owns the error */ };
        } catch (error) { finish(error); }
      };
    } catch (error) { finish(error); }
  });
}

export async function readFrontendAssets<K extends AssetKind>(kind: K): Promise<AssetFiles[K] | null> {
  const stored = await access(kind);
  return stored === undefined ? null : validateAssetFiles(kind, stored);
}

export async function saveFrontendAssets<K extends AssetKind>(kind: K, files: unknown, onlyIfMissing = false): Promise<AssetFiles[K]> {
  const checked = validateAssetFiles(kind, files);
  const saved = validateAssetFiles(kind, await access(kind, checked, onlyIfMissing));
  notify(kind);
  try { localStorage.setItem(CHANGE_KEY, `${kind}:${randomId()}`); } catch { /* same-tab delivery already happened */ }
  return saved;
}
