import { GetModelsForProvider } from './bindings';
import type { ModelInfo } from '../types/settings';
import {
  asProviderID,
  PROVIDER_IDS,
  type ProviderID,
} from '../types/providers';

let modelsByProvider = $state(new Map<ProviderID, ModelInfo[]>());
const inFlight = new Map<ProviderID, Promise<ModelInfo[]>>();
const generations = new Map<ProviderID, number>();

function setProviderModels(provider: ProviderID, models: ModelInfo[]): void {
  const next = new Map(modelsByProvider);
  next.set(provider, models);
  modelsByProvider = next;
}

export function getProviderModels(provider: ProviderID): ModelInfo[] {
  return modelsByProvider.get(provider) ?? [];
}

export async function ensureProviderModels(provider: ProviderID): Promise<ModelInfo[]> {
  const cached = modelsByProvider.get(provider);
  if (cached) return cached;

  const existing = inFlight.get(provider);
  if (existing) return existing;

  const generation = generations.get(provider) ?? 0;
  const request = loadProviderModels(provider, generation);
  inFlight.set(provider, request);
  request.then(
    () => clearInFlight(provider, request),
    () => clearInFlight(provider, request),
  );
  return request;
}

export async function refreshProviderModels(provider: ProviderID): Promise<ModelInfo[]> {
  const generation = (generations.get(provider) ?? 0) + 1;
  generations.set(provider, generation);
  const request = loadProviderModels(provider, generation);
  inFlight.set(provider, request);
  request.then(
    () => clearInFlight(provider, request),
    () => clearInFlight(provider, request),
  );
  return request;
}

function clearInFlight(provider: ProviderID, request: Promise<ModelInfo[]>): void {
  if (inFlight.get(provider) === request) {
    inFlight.delete(provider);
  }
}

async function loadProviderModels(
  provider: ProviderID,
  generation: number,
): Promise<ModelInfo[]> {
  return (async () => {
    try {
      const result = (await GetModelsForProvider(provider)) as ModelInfo[] | null;
      const models = Array.isArray(result) ? result : [];
      if ((generations.get(provider) ?? 0) === generation) {
        setProviderModels(provider, models);
      }
      return models;
    } catch (err) {
      throw err;
    }
  })();
}

export function invalidateProviderModels(
  provider?: ProviderID | string | null,
): void {
  if (provider === undefined || provider === null) {
    modelsByProvider = new Map();
    inFlight.clear();
    for (const providerID of PROVIDER_IDS) {
      generations.set(providerID, (generations.get(providerID) ?? 0) + 1);
    }
    return;
  }

  const providerID = asProviderID(provider);
  if (!providerID) return;

  generations.set(providerID, (generations.get(providerID) ?? 0) + 1);
  const next = new Map(modelsByProvider);
  next.delete(providerID);
  modelsByProvider = next;
  inFlight.delete(providerID);
}

export function resetProviderModelsForTest(): void {
  invalidateProviderModels();
}
