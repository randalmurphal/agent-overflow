/** Credential-free alternatives to one paired computer. Mirror
 * internal/computerroute; a valid origin still needs identity verification. */
export interface ComputerRoute { endpoint: string; certFingerprint?: string }
export const MAX_COMPUTER_ROUTES = 4;
const PIN = /^sha256:[0-9a-f]{64}$/;

export function normalizeComputerRoute(value: unknown): ComputerRoute | null {
  if (!value || typeof value !== 'object') return null;
  const route = value as Record<string, unknown>;
  if (typeof route.endpoint !== 'string' || route.endpoint.length > 2048 || route.endpoint.includes('\\')) return null;
  const text = route.endpoint.trim();
  // Validate the raw origin before URL can erase tabs or normalize /a/.. .
  if (!/^https:\/\/[^/?#]+\/?$/.test(text) || /[\x00-\x20]/.test(text)) return null;
  try {
    const url = new URL(text);
    if (!url.hostname || url.username || url.password || url.pathname !== '/' || text.includes('?') || text.includes('#')) return null;
    const authority = text.slice('https://'.length).split('/')[0];
    if (authority.endsWith(':') || url.port === '0' || /[^\x00-\x7f]/.test(authority)) return null;
    if (!url.hostname.startsWith('[') && (url.hostname.length > 253 || url.hostname.split('.').some((label) => !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label)))) return null;
    if (route.certFingerprint !== undefined && route.certFingerprint !== '' && (typeof route.certFingerprint !== 'string' || !PIN.test(route.certFingerprint))) return null;
    return { endpoint: url.origin, ...(route.certFingerprint ? { certFingerprint: route.certFingerprint as string } : {}) };
  } catch { return null; }
}

export function mergeComputerRoutes(previous: readonly ComputerRoute[], advertised: unknown): ComputerRoute[] {
  const result: ComputerRoute[] = [];
  for (const source of [Array.isArray(advertised) ? advertised : [], previous]) {
    for (const value of source.slice(0, MAX_COMPUTER_ROUTES * 8)) {
      const route = normalizeComputerRoute(value);
      if (route && !result.some((held) => held.endpoint === route.endpoint)) result.push(route);
      if (result.length === MAX_COMPUTER_ROUTES) return result;
    }
  }
  return result;
}

/** Explicit address repair reuses a saved private pin or WebPKI for the same
 * hostname. A new public domain cannot prove ownership by claiming a UUID. */
export function repairComputerRouteCandidates(primary: ComputerRoute, known: readonly ComputerRoute[], endpoint: string): ComputerRoute[] {
  const target = normalizeComputerRoute({ endpoint });
  if (!target) throw new Error('Enter an HTTPS computer address without a path or sign-in link.');
  const trusted = mergeComputerRoutes([], known);
  const original = normalizeComputerRoute(primary);
  if (original && !trusted.some((route) => route.endpoint === original.endpoint)) trusted.push(original);
  const hostname = new URL(target.endpoint).hostname;
  const candidates: ComputerRoute[] = [];
  for (const route of trusted) {
    if (!route.certFingerprint && new URL(route.endpoint).hostname !== hostname) continue;
    if (!candidates.some((candidate) => (candidate.certFingerprint || '') === (route.certFingerprint || ''))) {
      candidates.push({ endpoint: target.endpoint, ...(route.certFingerprint ? { certFingerprint: route.certFingerprint } : {}) });
    }
  }
  if (!candidates.length) throw new Error('This address cannot be verified with the saved computer trust. Use a new pairing link from that computer.');
  return candidates;
}
