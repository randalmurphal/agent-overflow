// The paired-device session client: the browser half of pairing
// (docs/specs/remote-access.md §4).
//
// A device that opened a pairing link redeems it here, stores the
// credential pair this backend issued, keeps it fresh through the
// rotating-refresh exchange, and turns it into the single-use ticket the
// WebSocket upgrade names its session with. Everything is a same-origin
// fetch — the pairing link lands the browser on the backend it is
// pairing with, and the CSP allows nothing else.
//
// Storage is the browser's own, scoped by origin, which is scoped per
// backend (the port is stable per install):
//
//   - localStorage `agent-overflow:deviceSession` — the credential pair.
//   - localStorage `agent-overflow:deviceKey` — the device identifier
//     minted before redemption (below).
//
// On the device identifier: the spec's end state is a WebCrypto keypair
// with a per-request possession proof (phase 5, DPoP). That needs a
// secure context, and a LAN page served over plain http is not one —
// `crypto.subtle` does not exist there. Today's wire carries only the
// key THUMBPRINT string in either case (transport.DeviceKeyHeader swaps
// to a signature when phase 5 lands with TLS), so until then the
// identifier is 32 CSPRNG bytes minted once per origin and presented as
// the thumbprint. Same bytes on the wire, no property lost, and the
// phase-5 upgrade is a re-enrollment this module already has a slot for.
//
// Refresh discipline (internal/identity/refresh.go): a refresh secret is
// single-use, and presenting a spent one reads as reuse evidence that
// revokes the whole session. So renewal here is single-flight, stores
// the response before anything may use it, and NEVER retries a request
// whose response it did not read — a lost response means the old secret
// is already spent, the next presentation would end the session, and the
// honest recovery is to let that happen and pair again.

// Mirrors internal/transport/authroutes.go. Names, not policy: the
// backend decides what they mean.
const AUTH_PAIR_PATH = '/auth/pair';
const AUTH_TOKEN_PATH = '/auth/token';
const AUTH_TICKET_PATH = '/auth/ticket';
const SESSION_CREDENTIAL_HEADER = 'X-AO-Session';
const DEVICE_KEY_HEADER = 'X-AO-Device-Key';

const SESSION_STORE_KEY = 'agent-overflow:deviceSession';
const DEVICE_KEY_STORE_KEY = 'agent-overflow:deviceKey';

// How close to the access credential's expiry a dial triggers renewal
// first. One minute: far enough that the ticket minted with the old
// credential cannot outlive it mid-upgrade, small against the shortest
// access window the backend issues (15 minutes).
const RENEW_MARGIN_MS = 60_000;

// The pairing payload as PairingPayload.Encode() produced it
// (internal/identity/pairing.go). Additive-only on the Go side; unknown
// fields are ignored here for the same reason.
export interface PairingPayload {
  v: number;
  backendId: string;
  backendName?: string;
  endpoint: string;
  token: string;
  certFingerprint?: string;
}

// The version this build can redeem. Mirrors identity.PairingPayloadVersion.
const PAIRING_PAYLOAD_VERSION = 1;

// One credential pair as /auth/pair and /auth/token grant it
// (transport.TokenGrant).
interface StoredSession {
  sessionId: string;
  credential: string;
  expiresAtMs: number;
  refreshSecret?: string;
  refreshExpiresAtMs?: number;
  label?: string;
}

export class PairingRefusedError extends Error {
  /** Reason code from the closed refusal set, '' when the response carried none. */
  reason: string;
  status: number;
  constructor(status: number, reason: string) {
    super(`pairing refused: HTTP ${status}${reason ? ` (${reason})` : ''}`);
    this.name = 'PairingRefusedError';
    this.status = status;
    this.reason = reason;
  }
}

function readLocal(key: string): string | null {
  if (typeof localStorage === 'undefined') return null;
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeLocal(key: string, value: string | null): void {
  if (typeof localStorage === 'undefined') return;
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    // Storage refused (private mode, quota). The session then lives for
    // this page's lifetime only, which degrades to "pair again next
    // visit" rather than anything broken.
  }
}

/**
 * The device identifier presented as the key thumbprint: 43 chars of
 * base64url over 32 CSPRNG bytes, minted once per origin and stable
 * from then on — it names this device's row in the backend's device
 * table, so a re-mint would read as a different device.
 */
export function deviceKeyThumbprint(): string {
  const held = readLocal(DEVICE_KEY_STORE_KEY);
  if (held && /^[A-Za-z0-9_-]{43}$/.test(held)) return held;
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  const minted = btoa(bin).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
  writeLocal(DEVICE_KEY_STORE_KEY, minted);
  return minted;
}

/**
 * Parse `#pair=<encoded>` off a URL hash. Null when the hash is not a
 * pairing fragment; throws when it is one this build cannot redeem (a
 * version bump, a payload for some other backend) — the caller shows
 * that, because silently booting the normal app would eat the link.
 */
export function parsePairingFragment(hash: string): PairingPayload | null {
  if (!hash.startsWith('#pair=')) return null;
  const encoded = hash.slice('#pair='.length);
  let payload: PairingPayload;
  try {
    if (!/^[A-Za-z0-9_-]+$/.test(encoded)) throw new Error('not base64url');
    payload = JSON.parse(atob(encoded.replaceAll('-', '+').replaceAll('_', '/'))) as PairingPayload;
  } catch {
    throw new Error('This pairing link is damaged. Ask for a new one.');
  }
  if (payload.v !== PAIRING_PAYLOAD_VERSION) {
    throw new Error('This pairing link came from a newer version of the app. Update this page and try again.');
  }
  if (!payload.token) {
    throw new Error('This pairing link is damaged. Ask for a new one.');
  }
  return payload;
}

/**
 * Refuse a payload whose endpoint is not the origin this page loaded
 * from. The minting surface builds both halves of the link from one
 * base, so a mismatch means a stale or foreign payload — and the CSP
 * would block the cross-origin redemption anyway.
 */
export function endpointMatchesOrigin(payload: PairingPayload, origin: string): boolean {
  try {
    return new URL(payload.endpoint).origin === origin;
  } catch {
    return false;
  }
}

function readStoredSession(): StoredSession | null {
  const raw = readLocal(SESSION_STORE_KEY);
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as StoredSession;
    if (typeof parsed.sessionId !== 'string' || typeof parsed.credential !== 'string') return null;
    return parsed;
  } catch {
    return null;
  }
}

function storeSession(session: StoredSession | null): void {
  writeLocal(SESSION_STORE_KEY, session === null ? null : JSON.stringify(session));
}

/** Whether this browser holds a paired-device session for this origin. */
export function hasPairedSession(): boolean {
  return readStoredSession() !== null;
}

/**
 * The headers a same-origin request presents to name the paired
 * session: the credential plus the device identifier its enrollment
 * bound. Empty when this browser holds no paired session, so callers
 * can spread it unconditionally. The manifest fetch is the consumer —
 * after a backend restart the page cookie is dead and this credential
 * is the one thing that still admits the page.
 */
export function pairedSessionHeaders(): Record<string, string> {
  const held = readStoredSession();
  if (!held) return {};
  return {
    [SESSION_CREDENTIAL_HEADER]: held.credential,
    [DEVICE_KEY_HEADER]: deviceKeyThumbprint(),
  };
}

/**
 * One renewal attempt on the stored session, for a caller whose request
 * was refused with the stored credential (the manifest fetch). Answers
 * whether a fresh credential is now stored; a renewal the backend
 * refuses as dead clears the store, so a false answer with
 * hasPairedSession() still true means "unproven, retry later" exactly
 * as it does for the ticket mint.
 */
export function renewPairedSession(fetcher: typeof fetch = fetch): Promise<boolean> {
  return renewSession(fetcher);
}

/** The stored session's id, for "this device" affordances. Null when unpaired. */
export function pairedSessionId(): string | null {
  return readStoredSession()?.sessionId ?? null;
}

/** Drop the stored session. The device key survives — it names the device, not the session. */
export function clearPairedSession(): void {
  storeSession(null);
}

interface GrantBody {
  sessionId?: string;
  credential?: string;
  expiresAtMs?: number;
  refreshSecret?: string;
  refreshExpiresAtMs?: number;
  awaitingConfirmation?: boolean;
  verificationNumber?: string;
  pairingId?: string;
  reason?: string;
}

async function readGrant(res: Response): Promise<GrantBody> {
  try {
    return (await res.json()) as GrantBody;
  } catch {
    return {};
  }
}

export interface RedemptionOutcome {
  /** The six digits the owner compares against their minting surface. */
  verificationNumber: string;
  sessionId: string;
}

/**
 * Spend the pairing link: mint/reuse the device identifier, present it
 * with the link token, store the (still unactivated) credential pair.
 */
export async function redeemPairing(
  payload: PairingPayload,
  label: string,
  fetcher: typeof fetch = fetch,
): Promise<RedemptionOutcome> {
  const thumbprint = deviceKeyThumbprint();
  const res = await fetcher(AUTH_PAIR_PATH, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      token: payload.token,
      keyThumbprint: thumbprint,
      label,
      platform: navigator.platform || '',
    }),
  });
  const body = await readGrant(res);
  if (!res.ok || !body.sessionId || !body.credential) {
    throw new PairingRefusedError(res.status, body.reason ?? '');
  }
  storeSession({
    sessionId: body.sessionId,
    credential: body.credential,
    expiresAtMs: body.expiresAtMs ?? 0,
    refreshSecret: body.refreshSecret,
    refreshExpiresAtMs: body.refreshExpiresAtMs,
    label,
  });
  return {
    verificationNumber: body.verificationNumber ?? '',
    sessionId: body.sessionId,
  };
}

// Single-flight guards. Two callers asking at once must observe one
// exchange: a second concurrent renewal would present a spent secret and
// end the session as reuse evidence.
let renewalInFlight: Promise<boolean> | null = null;
let ticketInFlight: Promise<string | null> | null = null;

/**
 * Rotate the credential pair. Resolves true when the stored pair is
 * fresh afterwards, false when renewal is not possible (no secret, or
 * the backend refused — in which case the stored session is cleared,
 * because a refused renewal never heals on retry).
 *
 * A NETWORK failure also resolves false but keeps the stored session:
 * the secret may or may not be spent server-side, and only the next
 * deliberate presentation can find out.
 */
function renewSession(fetcher: typeof fetch): Promise<boolean> {
  if (renewalInFlight) return renewalInFlight;
  renewalInFlight = (async () => {
    const held = readStoredSession();
    if (!held?.refreshSecret) return false;
    let res: Response;
    try {
      res = await fetcher(AUTH_TOKEN_PATH, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          [DEVICE_KEY_HEADER]: deviceKeyThumbprint(),
        },
        body: JSON.stringify({ refreshSecret: held.refreshSecret }),
      });
    } catch {
      return false;
    }
    const body = await readGrant(res);
    if (!res.ok || !body.sessionId || !body.credential) {
      // Clear only on a REFUSAL of this credential (401 carries the
      // reason), and not while the owner simply has not confirmed the
      // pairing yet — that session is real and becomes admitted the
      // moment they do. A 429 or a 5xx says nothing about the
      // credential and clears nothing.
      if (res.status === 401 && body.reason !== 'pending_confirmation') {
        clearPairedSession();
      }
      return false;
    }
    storeSession({
      sessionId: body.sessionId,
      credential: body.credential,
      expiresAtMs: body.expiresAtMs ?? 0,
      refreshSecret: body.refreshSecret,
      refreshExpiresAtMs: body.refreshExpiresAtMs,
      label: held.label,
    });
    return true;
  })().finally(() => {
    renewalInFlight = null;
  });
  return renewalInFlight;
}

/**
 * Mint the single-use ticket the WebSocket upgrade names its session
 * with. Null when this browser holds no paired session, or when the
 * backend will not honour the one it holds. The caller distinguishes
 * the two by re-reading the store: a mint that PROVED the session dead
 * cleared it (the dial then proceeds unpaired), while a null with the
 * session still held means unproven — the dial must fail rather than
 * fall back to a page cookie that would name a different session.
 *
 * Also the activation probe: while the owner has not confirmed the
 * pairing, the backend answers 404, and this keeps answering null
 * without clearing anything — the stored credential is real and simply
 * not admitted yet.
 */
export function mintDialTicket(fetcher: typeof fetch = fetch): Promise<string | null> {
  if (ticketInFlight) return ticketInFlight;
  ticketInFlight = (async () => {
    let held = readStoredSession();
    if (!held) return null;
    if (held.expiresAtMs > 0 && held.expiresAtMs - Date.now() < RENEW_MARGIN_MS) {
      await renewSession(fetcher);
      held = readStoredSession();
      if (!held) return null;
    }
    let res: Response;
    try {
      res = await fetcher(AUTH_TICKET_PATH, {
        method: 'POST',
        headers: {
          [SESSION_CREDENTIAL_HEADER]: held.credential,
          [DEVICE_KEY_HEADER]: deviceKeyThumbprint(),
        },
      });
    } catch {
      return null;
    }
    if (!res.ok) {
      // 404 covers both "not admitted yet" (awaiting confirmation) and
      // "not admitted any more" (expired between renewals, revoked). One
      // renewal attempt tells them apart: a live session rotates and the
      // retried mint succeeds; a dead one refuses the rotation, which
      // clears the store.
      if (await renewSession(fetcher)) {
        const renewed = readStoredSession();
        if (!renewed) return null;
        try {
          const retry = await fetcher(AUTH_TICKET_PATH, {
            method: 'POST',
            headers: {
              [SESSION_CREDENTIAL_HEADER]: renewed.credential,
              [DEVICE_KEY_HEADER]: deviceKeyThumbprint(),
            },
          });
          if (retry.ok) {
            const grant = (await retry.json()) as { ticket?: string };
            return grant.ticket ?? null;
          }
        } catch {
          return null;
        }
      }
      return null;
    }
    const grant = (await res.json()) as { ticket?: string };
    return grant.ticket ?? null;
  })().finally(() => {
    ticketInFlight = null;
  });
  return ticketInFlight;
}

/**
 * Whether the stored session is admitted yet: the pairing screen's
 * confirmation probe. True the moment the owner confirms; false while
 * pending, and false-forever once the confirm window lapses — the
 * caller owns the deadline, this owns one probe.
 */
export async function probeActivation(fetcher: typeof fetch = fetch): Promise<boolean> {
  const held = readStoredSession();
  if (!held) return false;
  let res: Response;
  try {
    res = await fetcher(AUTH_TICKET_PATH, {
      method: 'POST',
      headers: {
        [SESSION_CREDENTIAL_HEADER]: held.credential,
        [DEVICE_KEY_HEADER]: deviceKeyThumbprint(),
      },
    });
  } catch {
    return false;
  }
  if (!res.ok) return false;
  // The probe minted a real ticket; it goes unused and lapses in
  // seconds, which the ticket book prices in (mint evicts, TTL sweeps).
  void res.body?.cancel();
  return true;
}
