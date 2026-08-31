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
//   - localStorage `agent-overflow:deviceKey` — the bearer device
//     identifier, for a page that cannot hold a real key (below).
//   - IndexedDB `agent-overflow-device-key` — the non-extractable signing
//     keypair, when this page can hold one (./deviceKey.ts).
//
// Two kinds of device, and which one this page is depends on what it CAN
// do rather than on what it prefers (docs/specs/remote-access.md §4 and
// §15 constraint 6):
//
//   - A secure context has `crypto.subtle`, so pairing generates a
//     non-extractable ECDSA P-256 keypair and every credential request
//     carries a fresh signature over that request. The backend records the
//     device `key` and refuses the bare thumbprint from it afterwards, so
//     a copied credential string admits nothing.
//   - A plain-http LAN page has no `crypto.subtle` at all. It enrolls with
//     32 CSPRNG bytes minted once per origin and presents that string,
//     which is what it did before phase 5 and what it will keep doing:
//     the spec states there is deliberately no LAN-HTTP proof path.
//
// Migration. A session stored before phase 5 records no `proofKind`, which
// reads as `bearer` — matching its device row, which the v77 migration
// defaulted the same way — so an already-paired browser keeps working
// untouched and is never asked to re-pair. It upgrades only by pairing
// again, which is the one moment a device may choose its kind. The
// opposite case, a `key` session whose IndexedDB was cleared while
// localStorage survived, cannot sign and cannot fall back (the backend
// refuses the downgrade); it clears the stored session so the page asks to
// pair rather than retrying something that can never succeed.
//
// Refresh discipline (internal/identity/refresh.go): a refresh secret is
// single-use, and presenting a spent one reads as reuse evidence that
// revokes the whole session. So renewal here is single-flight, stores
// the response before anything may use it, and NEVER retries a request
// whose response it did not read — a lost response means the old secret
// is already spent, the next presentation would end the session, and the
// honest recovery is to let that happen and pair again.

import { enrollDeviceKey, mintDeviceProof } from './deviceKey';

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
  /**
   * The grant set this session holds, as the issuing response published
   * it. Absent when the backend is older than the field, which is why
   * `pairedSessionScopes` answers null rather than `[]` for that case:
   * "nothing granted" and "this backend does not say" take different
   * fallbacks in ./scopes.ts.
   */
  scopes?: string[];
  /**
   * How this session's device proves possession, as its enrolment
   * decided. Absent means `bearer`: a session stored before phase 5,
   * whose device row the v77 migration defaulted the same way. That
   * agreement is the whole migration — neither side has to be told.
   *
   * It is read to decide what this page must SEND, never to decide what
   * it may do. The backend re-checks against the device row on every
   * request, so a hand-edited value changes only whether this page's
   * requests are refused.
   */
  proofKind?: 'key' | 'bearer';
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
    // A hand-edited or half-written entry must not become a grant set.
    // Anything that is not an array of strings reads as "not published".
    if (!Array.isArray(parsed.scopes) || parsed.scopes.some((s) => typeof s !== 'string')) {
      delete parsed.scopes;
    }
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
 * The device-key header for ONE request, or null when this device holds a
 * key-bound session it can no longer sign for.
 *
 * The proof is minted per request because it is bound to the method and
 * path and is spent on first use: caching one would be refused as
 * `proof_replayed`, and reusing one across routes as `proof_not_bound`.
 *
 * The null answer is the missing-key case and is deliberately not a
 * fallback to the bearer string. The backend refuses that presentation
 * from a key-bound device (`proof_downgraded`) exactly so it cannot be
 * used as one, and sending it anyway would spend a round trip to be told
 * what this page already knows. Callers clear the session instead.
 */
async function deviceKeyHeader(
  held: StoredSession,
  method: string,
  path: string,
): Promise<Record<string, string> | null> {
  if (held.proofKind !== 'key') return { [DEVICE_KEY_HEADER]: deviceKeyThumbprint() };
  const proof = await mintDeviceProof(method, path);
  return proof === null ? null : { [DEVICE_KEY_HEADER]: proof };
}

/**
 * The headers a same-origin request presents to name the paired
 * session: the credential plus a proof of the key its enrollment bound.
 * Empty when this browser holds no paired session, so callers can spread
 * it unconditionally. The manifest fetch is the consumer — after a backend
 * restart the page cookie is dead and this credential is the one thing
 * that still admits the page.
 *
 * Asynchronous because minting a proof is: signing is a WebCrypto call.
 * A device whose key is gone clears its session here and answers empty,
 * so the fetch proceeds unpaired rather than carrying a credential the
 * backend is certain to refuse.
 */
export async function pairedSessionHeaders(
  method = 'GET',
  path = '/bootstrap.json',
): Promise<Record<string, string>> {
  const held = readStoredSession();
  if (!held) return {};
  const keyHeader = await deviceKeyHeader(held, method, path);
  if (!keyHeader) {
    clearPairedSession();
    return {};
  }
  return { [SESSION_CREDENTIAL_HEADER]: held.credential, ...keyHeader };
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

/**
 * The grant set the stored paired session holds, or null when this
 * browser holds no paired session — or holds one from a backend too old
 * to publish grants.
 *
 * Null is a distinct answer from `[]` on purpose. `[]` means the backend
 * said "this session was granted nothing"; null means it said nothing at
 * all, and ./scopes.ts falls back to judging the page rather than
 * treating silence as a refusal of every surface.
 *
 * Read on demand rather than cached: the store moves on redemption and
 * on rotation, and the two callers ask at exactly those moments.
 */
export function pairedSessionScopes(): readonly string[] | null {
  return readStoredSession()?.scopes ?? null;
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
  scopes?: string[];
}

/**
 * The grant set a credential response published, or undefined when it
 * published none. Filters to strings so a value from a backend speaking
 * a shape this build does not know cannot land in storage as one.
 */
function grantedScopesFrom(body: GrantBody): string[] | undefined {
  if (!Array.isArray(body.scopes)) return undefined;
  return body.scopes.filter((scope): scope is string => typeof scope === 'string');
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
  // The one moment a device chooses its kind, and it chooses by what it
  // can do. A page that can sign generates its keypair here and proves it
  // in the redemption itself, so the thumbprint the backend records is
  // derived from a key this page just demonstrated it holds — there is no
  // separate "register the key" step that could be skipped.
  // enrollDeviceKey is the ONLY generation site in the app: everywhere
  // else reads the stored key and answers null when there is none, so a
  // key that went missing can never be silently replaced under a session
  // still bound to the old one. It persists before returning, which is
  // what lets the mint below read it back.
  const proof = (await enrollDeviceKey()) ? await mintDeviceProof('POST', AUTH_PAIR_PATH) : null;
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (proof !== null) headers[DEVICE_KEY_HEADER] = proof;
  const res = await fetcher(AUTH_PAIR_PATH, {
    method: 'POST',
    headers,
    body: JSON.stringify({
      token: payload.token,
      // Sent only on the bearer path. A signed redemption names its key
      // inside the proof, and the backend ignores this field entirely
      // when one is present, so filling it would be a second and weaker
      // claim about the same fact.
      keyThumbprint: proof === null ? deviceKeyThumbprint() : '',
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
    scopes: grantedScopesFrom(body),
    proofKind: proof === null ? 'bearer' : 'key',
  });
  return {
    verificationNumber: body.verificationNumber ?? '',
    sessionId: body.sessionId,
  };
}

/**
 * The headers one `/auth/ticket` mint presents. Null when the device
 * cannot sign for a key-bound session — the caller then answers "no
 * ticket", which its own contract already covers.
 *
 * The session is cleared on that path for the same reason renewal does
 * it: a key-bound session with no key never heals, and a dial that keeps
 * retrying one would leave the page reconnecting forever instead of
 * showing the pairing prompt.
 */
async function ticketHeaders(held: StoredSession): Promise<Record<string, string> | null> {
  const keyHeader = await deviceKeyHeader(held, 'POST', AUTH_TICKET_PATH);
  if (!keyHeader) {
    clearPairedSession();
    return null;
  }
  return { [SESSION_CREDENTIAL_HEADER]: held.credential, ...keyHeader };
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
    const keyHeader = await deviceKeyHeader(held, 'POST', AUTH_TOKEN_PATH);
    if (!keyHeader) {
      // A key-bound session whose key is gone. Renewal is the one exchange
      // that could END the session by being retried, so this must not
      // reach the wire: clear it here and let the page ask to pair.
      clearPairedSession();
      return false;
    }
    let res: Response;
    try {
      res = await fetcher(AUTH_TOKEN_PATH, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...keyHeader },
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
      // Rotation never changes how this device proves possession — the
      // device row decides that and nothing rotates it — so the kind is
      // carried forward rather than re-derived.
      proofKind: held.proofKind,
      // A rotation that did not publish grants keeps the ones the
      // redemption did. Grants are immutable for a session's lifetime,
      // so the carried copy is still true, and dropping it would turn a
      // renewal into a downgrade of what this page offers.
      scopes: grantedScopesFrom(body) ?? held.scopes,
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
      const headers = await ticketHeaders(held);
      if (!headers) return null;
      res = await fetcher(AUTH_TICKET_PATH, { method: 'POST', headers });
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
          // A FRESH proof: the one the first attempt sent is spent, and
          // re-sending it would be refused as a replay.
          const headers = await ticketHeaders(renewed);
          if (!headers) return null;
          const retry = await fetcher(AUTH_TICKET_PATH, { method: 'POST', headers });
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
    const headers = await ticketHeaders(held);
    if (!headers) return false;
    res = await fetcher(AUTH_TICKET_PATH, { method: 'POST', headers });
  } catch {
    return false;
  }
  if (!res.ok) return false;
  // The probe minted a real ticket; it goes unused and lapses in
  // seconds, which the ticket book prices in (mint evicts, TTL sweeps).
  void res.body?.cancel();
  return true;
}
