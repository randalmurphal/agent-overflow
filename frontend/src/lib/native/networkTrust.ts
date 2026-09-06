// Pairing writes the original origin's private-certificate trust here. Verified
// bootstrap advertisements live separately in transport/computerRoutes. A URL
// never learns its own pin from an untrusted response, and a changed certificate
// never falls back to WebPKI. Go uses this same sha256-of-leaf-DER contract.
import { isNativeShell } from './platform';
import type { PairingPayload } from '../transport/deviceSession';

const KEY = 'agent-overflow:certificatePins';
const PIN = /^sha256:[0-9a-f]{64}$/;

// Retain a refusal marker if the entire map is unreadable. Re-pairing can
// repair one origin without silently removing the trust requirement for others.
const DAMAGED = '!damaged';
const WEB_PKI = 'webpki';
function pins(repair = false): Record<string, string> {
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) return parsed as Record<string, string>;
  } catch { /* The explicit pairing action below is the only repair door. */ }
  if (repair) return { [DAMAGED]: 'true' };
  throw new Error('Saved computer trust is damaged. Pair the computer again.');
}

/** Resolve a scanned endpoint before storing it or sending any credential. */
export function pairingEndpoint(payload: PairingPayload): string {
  const url = new URL(payload.endpoint);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password
    || url.pathname !== '/' || url.search || url.hash) throw new Error('Invalid computer address in pairing link.');
  if (!isNativeShell()) return url.origin;
  if (payload.certFingerprint && !PIN.test(payload.certFingerprint)) throw new Error('Invalid certificate fingerprint in pairing link.');
  if (payload.certFingerprint) url.protocol = 'https:';
  const known = pins(true);
  delete known[url.origin];
  known[url.origin] = payload.certFingerprint || WEB_PKI;
  const entries = Object.entries(known);
  if (entries.length > 64) throw new Error('Too many saved computer addresses. Remove an unused computer first.');
  localStorage.setItem(KEY, JSON.stringify(known));
  return url.origin;
}

export function certificatePin(url: string): string | null {
  if (!isNativeShell()) return null;
  const address = new URL(url, globalThis.location?.href);
  if (address.protocol === 'wss:') address.protocol = 'https:';
  const known = pins();
  const value = known[address.origin];
  if (value === WEB_PKI || (value === undefined && !known[DAMAGED])) return null;
  if (address.protocol !== 'https:' || !PIN.test(value)) {
    throw new Error('Saved computer trust is damaged. Pair the computer again.');
  }
  return value;
}

export function forgetCertificatePin(origin: string): void {
  if (!isNativeShell() || !origin) return;
  try {
    const known = pins(true);
    delete known[origin];
    localStorage.setItem(KEY, JSON.stringify(known));
  } catch { /* Trust storage failure must not prevent forgetting a connection. */ }
}
