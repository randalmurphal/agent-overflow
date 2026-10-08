// The backend's diagnostic record of one failure (internal/transport
// ErrorDetail). It arrives as untrusted wire input, so it is validated and
// bounded here before anything renders or copies it.

import type { BackendKey } from './backendKey';

export interface ErrorDetail {
  /** Matches the backend log line `(id: <ref>)` holding the full error. */
  ref: string;
  /** The bound method that failed. */
  method: string;
  /** When the backend recorded the failure, in Unix milliseconds. */
  at: number;
  /**
   * The error's wrap layers, outermost first. Only a caller on the
   * backend's own machine receives it; empty otherwise.
   */
  chain: string[];
  /** The attached backend that answered, stamped by the client. */
  backend: BackendKey;
}

const MAX_ID_LENGTH = 128;
const MAX_CHAIN_LAYERS = 32;
const MAX_LAYER_LENGTH = 4096;

export function parseErrorDetail(value: unknown, backend: BackendKey): ErrorDetail | undefined {
  if (value === null || typeof value !== 'object') return undefined;
  const raw = value as Record<string, unknown>;
  const { ref, method, at, chain } = raw;
  if (typeof ref !== 'string' || ref.length === 0 || ref.length > MAX_ID_LENGTH) return undefined;
  if (typeof method !== 'string' || method.length > MAX_ID_LENGTH) return undefined;
  if (typeof at !== 'number' || !Number.isFinite(at)) return undefined;
  const layers = Array.isArray(chain)
    ? chain
      .filter((layer): layer is string => typeof layer === 'string')
      .slice(0, MAX_CHAIN_LAYERS)
      .map((layer) => (layer.length > MAX_LAYER_LENGTH ? `${layer.slice(0, MAX_LAYER_LENGTH)}…` : layer))
    : [];
  return { ref, method, at, chain: layers, backend };
}
