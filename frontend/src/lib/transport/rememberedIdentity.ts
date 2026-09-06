// Last observed computer identity for offline display/cache lookup. This is
// never a live history attestation, grant, clock or connection status.
import type { BackendIdentity } from './backendIdentity';
import type { BackendKey } from './backendKey';
import { backendUrl } from './homeEndpoint';

const KEY = 'agent-overflow:computerIdentities';
interface Remembered { origin: string; identity: BackendIdentity }
function origin(backend: BackendKey): string {
  return new URL(backendUrl('/', backend), location.href).origin;
}
let cachedRaw: string | null | undefined;
let cachedValues: Record<string, Remembered> = {};
function read(): Record<string, Remembered> {
  try {
    const raw = localStorage.getItem(KEY);
    if (raw === cachedRaw) return cachedValues;
    const value = JSON.parse(raw ?? '{}');
    cachedValues = value && typeof value === 'object' && !Array.isArray(value) ? value : {};
    cachedRaw = raw;
    return cachedValues;
  } catch { return {}; }
}
export function rememberedIdentity(backend: BackendKey): BackendIdentity | null {
  try {
    const value = read()[backend];
    const id = value?.identity;
    if (value?.origin !== origin(backend) || !id || !valid(id.backendId) || !valid(id.generation)
      || typeof id.name !== 'string' || id.name.length > 256) return null;
    return id;
  } catch { return null; }
}
function valid(value: unknown): value is string {
  return typeof value === 'string' && /^[a-zA-Z0-9_-]{1,128}$/.test(value);
}
export function rememberIdentity(backend: BackendKey, identity: BackendIdentity): void {
  if (!valid(identity.backendId) || !valid(identity.generation)) return;
  try {
    const values = { ...read() };
    delete values[backend];
    values[backend] = { origin: origin(backend), identity: { ...identity, name: identity.name.slice(0, 256) } };
    localStorage.setItem(KEY, JSON.stringify(Object.fromEntries(Object.entries(values).slice(-64))));
  } catch { /* Optional offline cache; the live identity remains authoritative. */ }
}
export function forgetRememberedIdentity(backend: BackendKey): void {
  try {
    const values = { ...read() };
    delete values[backend];
    localStorage.setItem(KEY, JSON.stringify(values));
  } catch { /* A missing cache never prevents removing a connection. */ }
}
