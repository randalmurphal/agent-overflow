// Raw text highlighting is a frontend rendering service, not a HOME resource.
// Keep one schema for the page's content-addressed caches; another computer may
// compute spans only after proving it speaks that exact schema.
import { HighlightClassNames, HighlightSchemaVersion } from '../stores/bindings';
import { selectedBackend } from '../stores/selectedBackend.svelte';
import { attachedBackends, backendById, withBackendTarget, type BackendEntry } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { DisconnectedError, type TransportHello } from '../transport/wsClient';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';

export interface HighlightMetadata { version: string; names: string[] }
export interface HighlightSource {
  entry: BackendEntry;
  generation: string;
  hello: TransportHello | null;
}
class HighlightSchemaMismatch extends Error {
  constructor() { super('This computer uses an incompatible syntax highlighting version'); }
}
let canonical: HighlightMetadata | null = null;
let loading: Promise<HighlightMetadata> | null = null;
let metadata = new WeakMap<BackendEntry, { generation: string; hello: TransportHello | null; promise: Promise<HighlightMetadata> }>();

function candidates(): BackendEntry[] {
  const home = backendById(HOME_BACKEND);
  const selected = backendById(selectedBackend());
  const entries = [...new Set([home, selected, ...attachedBackends()].filter((entry): entry is BackendEntry => !!entry))];
  // Prefer an already connected host. When all are reconnecting, let the
  // normal transport wait/timeout own the request instead of polling here.
  const connected = entries.filter(entry => entry.status.status === 'connected');
  return connected.length ? connected : entries;
}

function readMetadata(entry: BackendEntry): Promise<HighlightMetadata> {
  const previous = metadata.get(entry);
  if (previous?.generation === entry.generation && previous.hello === entry.client.getHello()) return previous.promise;
  const generation = entry.generation;
  const hello = entry.client.getHello();
  const pending = Promise.all([
    withBackendTarget(entry.id, () => HighlightSchemaVersion()),
    withBackendTarget(entry.id, () => HighlightClassNames()),
  ]).then(([version, names]) => {
    if (backendById(entry.id) !== entry || (generation && entry.generation !== generation)
      || (hello && entry.client.getHello() !== hello)) {
      throw new DisconnectedError('Highlight computer changed while loading its schema');
    }
    if (!version || !Array.isArray(names)) throw new Error('Invalid syntax highlighting metadata');
    cached.generation = entry.generation;
    cached.hello = entry.client.getHello();
    return { version, names };
  });
  const cached = { generation, hello, promise: pending };
  metadata.set(entry, cached);
  void pending.catch(() => { if (metadata.get(entry) === cached) metadata.delete(entry); });
  return pending;
}

export function highlightMetadata(): Promise<HighlightMetadata> {
  if (canonical) return Promise.resolve(canonical);
  loading ??= (async () => {
    let last: unknown = new DisconnectedError('No computer is available for syntax highlighting');
    for (const entry of candidates()) {
      try {
        canonical = await readMetadata(entry);
        return canonical;
      } catch (error) {
        if (!isPassiveConnectionFailure(error)) throw error;
        last = error;
      }
    }
    throw last;
  })().finally(() => { loading = null; });
  return loading;
}

/** Validate an origin before accepting its contextual or pushed spans. */
export async function requireHighlightSchema(backend: BackendKey): Promise<HighlightSource> {
  const entry = backendById(backend);
  if (!entry) throw new DisconnectedError('Highlight computer is no longer attached');
  const generation = entry.generation;
  const hello = entry.client.getHello();
  const expected = await highlightMetadata();
  const actual = await readMetadata(entry);
  if (backendById(backend) !== entry || (generation && entry.generation !== generation)
    || (hello && entry.client.getHello() !== hello)) {
    throw new DisconnectedError('Highlight computer changed while checking its schema');
  }
  if (actual.version !== expected.version) throw new HighlightSchemaMismatch();
  return { entry, generation: entry.generation, hello: entry.client.getHello() };
}

export function assertHighlightSource(source: HighlightSource): void {
  const { entry, generation, hello } = source;
  if (backendById(entry.id) !== entry || entry.generation !== generation || entry.client.getHello() !== hello) {
    throw new DisconnectedError('Highlight computer changed during the request');
  }
}

/** Contextual reads stay on their owning computer and share the same
 * post-read restart guard as raw rendering work. */
export async function withHighlightBackend<T>(backend: BackendKey, issue: () => Promise<T>): Promise<T> {
  const source = await requireHighlightSchema(backend);
  assertHighlightSource(source);
  const result = await withBackendTarget(source.entry.id, issue)
    .catch(error => { assertHighlightSource(source); throw error; });
  assertHighlightSource(source);
  return result;
}

/** Capture the backend before the request; focus changes cannot reroute it. */
export async function withHighlightService<T>(issue: () => Promise<T>): Promise<T> {
  await highlightMetadata();
  let last: unknown = new DisconnectedError('No computer is available for syntax highlighting');
  for (const entry of candidates()) {
    try {
      return await withHighlightBackend(entry.id, issue);
    } catch (error) {
      last = error;
      // A schema mismatch may be bypassed by another compatible computer;
      // arbitrary rendering failures must remain visible.
      if (!isPassiveConnectionFailure(error) && !(error instanceof HighlightSchemaMismatch)) throw error;
    }
  }
  throw last;
}

export function resetHighlightServiceForTest(): void {
  canonical = null;
  loading = null;
  metadata = new WeakMap();
}
