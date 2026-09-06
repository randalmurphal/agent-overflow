// Model catalogs are account- and computer-specific. A refreshed Mac catalog
// never changes another computer's composer; stale loads cannot undo a switch.
import { GetModelsForProvider } from './bindings';
import { hasScope, pageGrantsResolved } from '../transport/scopes';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { backendById, onBackendDetached, withBackendTarget } from '../transport/backends';
import { compositeKey } from '../utils/compositeKey';
import type { ModelInfo } from '../types/settings';
import { asProviderID, PROVIDER_IDS, type ProviderID } from '../types/providers';
import {
  providerIsEnabled, PROVIDER_SETTINGS_ORDER, type ProviderEnablementSettings,
} from '../providers/catalog';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

const models = createKeyedSignalRegistry<ModelInfo[] | null>(null);
const inFlight = new Map<string, Promise<ModelInfo[]>>();
const generations = new Map<string, number>();
const EMPTY: ModelInfo[] = [];

export function getProviderModels(provider: ProviderID, backend: BackendKey = HOME_BACKEND): ModelInfo[] {
  return models.get(compositeKey(backend, provider)) ?? EMPTY;
}

export async function ensureProviderModels(provider: ProviderID, backend: BackendKey = HOME_BACKEND): Promise<ModelInfo[]> {
  const key = compositeKey(backend, provider);
  const cached = models.get(key);
  if (cached) return Promise.resolve(cached);
  if (!hasScope('threads:operate', backend)) return Promise.resolve(EMPTY);
  return inFlight.get(key) ?? loadProviderModels(provider, backend);
}

export async function refreshProviderModels(provider: ProviderID, backend: BackendKey = HOME_BACKEND): Promise<ModelInfo[]> {
  if (!hasScope('threads:operate', backend)) return Promise.resolve(EMPTY);
  const key = compositeKey(backend, provider);
  generations.set(key, (generations.get(key) ?? 0) + 1);
  return loadProviderModels(provider, backend);
}

function loadProviderModels(provider: ProviderID, backend: BackendKey): Promise<ModelInfo[]> {
  const key = compositeKey(backend, provider);
  const generation = generations.get(key) ?? 0;
  const target = backendById(backend);
  let request: Promise<ModelInfo[]>;
  request = (async () => {
    const result = await withBackendTarget(backend, () => GetModelsForProvider(provider));
    if (backendById(backend) !== target) throw new Error('Computer was removed while loading its models.');
    if ((generations.get(key) ?? 0) !== generation) {
      // A superseded caller joins the newer load instead of publishing its
      // old account's models. Retain an already loaded current catalog.
      const cached = models.get(key);
      if (cached) return cached;
      return ensureProviderModels(provider, backend);
    }
    const list = Array.isArray(result) ? result as ModelInfo[] : [];
    models.set(key, list);
    return list;
  })();
  inFlight.set(key, request);
  const clear = () => { if (inFlight.get(key) === request) inFlight.delete(key); };
  void request.then(clear, clear);
  return request;
}

export async function preloadProviderModelsForSettings(
  settings: ProviderEnablementSettings, backend: BackendKey = HOME_BACKEND,
): Promise<void> {
  if (backend === HOME_BACKEND) await pageGrantsResolved();
  const providers = PROVIDER_SETTINGS_ORDER.filter((provider) => providerIsEnabled(settings, provider));
  const results = await Promise.allSettled(providers.map((provider) => ensureProviderModels(provider, backend)));
  for (const [index, result] of results.entries()) {
    if (result.status === 'rejected') console.warn(`Failed to preload ${providers[index]} models:`, result.reason);
  }
}

export function invalidateProviderModels(
  provider?: ProviderID | string | null, backend: BackendKey = HOME_BACKEND,
): void {
  const selected = provider == null ? PROVIDER_IDS : [asProviderID(provider)].filter(Boolean) as ProviderID[];
  for (const id of selected) {
    const key = compositeKey(backend, id);
    generations.set(key, (generations.get(key) ?? 0) + 1);
    models.drop(key);
    inFlight.delete(key);
  }
}

export function resetProviderModelsForTest(): void {
  for (const key of new Set([...generations.keys(), ...inFlight.keys()])) {
    generations.set(key, (generations.get(key) ?? 0) + 1);
  }
  models.reset();
  inFlight.clear();
}

onBackendDetached(({ backendId }) => invalidateProviderModels(null, backendId));
