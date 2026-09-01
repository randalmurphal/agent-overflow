// The browser half of the passkey ceremonies (docs/specs/remote-access.md
// §4 "Passkeys").
//
// Three ceremonies share one shape — the backend hands over options, the
// authenticator answers, the backend verifies — so this module owns that
// shape and nothing about what any of them MEANS. Which account, which
// session, and whether an assertion is good enough are decisions
// internal/identity makes; a caller here only carries bytes between a
// JSON envelope and `navigator.credentials`.
//
// **The codecs are the whole of the impedance mismatch.** WebAuthn's JSON
// dialect spells every binary member base64url; the DOM API takes and
// returns `ArrayBuffer`. So the options are walked on the way IN and the
// credential is rebuilt on the way OUT, by hand rather than through
// `PublicKeyCredential.parseCreationOptionsFromJSON` / `toJSON`. Those
// helpers are recent enough that a phone one major version behind has
// them missing, and an absent method is a TypeError, not a degraded
// feature — the same class that rendered a blank page from
// `crypto.randomUUID` (./deviceKey.ts). Hand-rolled, they work on every
// browser that has WebAuthn at all.
//
// A page with no secure context has no `navigator.credentials` either,
// which is the plain-HTTP LAN case from spec §15 constraint 6. It is a
// runtime test here, never a build flag: `passkeysUsable()` answers false
// and every surface asking keeps whatever it did before.

/** The `publicKey` member of a ceremony, as JSON with base64url binaries. */
type ChallengeOptions = Record<string, unknown>;

/** A ceremony the backend just started (transport.PasskeyChallenge). */
export interface PasskeyChallenge {
  ceremonyId: string;
  options: ChallengeOptions;
}

/**
 * Whether the backend offers passkeys at all: it published a canonical
 * domain, which is the only thing a credential can be registered under
 * (internal/app/app_passkey.go).
 *
 * Published from the manifest rather than from the connection's hello,
 * because the page that most needs the answer is the one whose socket the
 * backend will not open — a browser that has never paired. Latched the way
 * ./harnessMode.ts latches its own: set on every manifest resolution, read
 * synchronously by whoever is drawing.
 */
let backendOffersPasskeys = false;

/** Called by ./bootstrap.ts on every manifest resolution. */
export function setPasskeysAvailableFromBootstrap(available: boolean): void {
  backendOffersPasskeys = available;
}

/**
 * Whether this page can run a ceremony AND this backend offers one. Both
 * halves, because either alone draws a control that cannot work: an
 * affordance on a plain-HTTP page throws when pressed, and one against a
 * backend with no domain spends a round trip to be refused.
 */
export function passkeysUsable(): boolean {
  return backendOffersPasskeys && passkeysSupported();
}

/** Whether this page has the API at all. Secure contexts only. */
export function passkeysSupported(): boolean {
  return (
    typeof window !== 'undefined' &&
    typeof window.PublicKeyCredential === 'function' &&
    typeof navigator !== 'undefined' &&
    typeof navigator.credentials?.get === 'function' &&
    typeof navigator.credentials?.create === 'function'
  );
}

/**
 * A ceremony the person abandoned: they dismissed the prompt, or the
 * authenticator declined.
 *
 * Distinguished from every other failure because it is the one that needs
 * no sentence — nothing went wrong, somebody changed their mind, and a
 * surface that reports it as an error accuses them of a fault. The DOM
 * spells it `NotAllowedError` for both cases deliberately, so this cannot
 * tell "cancelled" from "refused" and does not try.
 */
export class PasskeyAbandonedError extends Error {
  constructor() {
    super('passkey ceremony abandoned');
    this.name = 'PasskeyAbandonedError';
  }
}

/** base64url → bytes. Padding is absent by definition in this dialect. */
export function decodeBase64url(value: string): Uint8Array {
  const padded = value.replaceAll('-', '+').replaceAll('_', '/');
  const binary = atob(padded + '='.repeat((4 - (padded.length % 4)) % 4));
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}

/** bytes → base64url, unpadded, which is what the Go side parses. */
export function encodeBase64url(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = '';
  // Chunked: String.fromCharCode is variadic and an attestation object can
  // run to a few kilobytes, which is enough to blow the argument limit on
  // some engines.
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
}

/**
 * The members the WebAuthn JSON dialect spells base64url, by the path
 * they sit at in an options object. Listed rather than inferred: a walker
 * that decoded every base64url-SHAPED string would corrupt any future
 * field that happened to look like one, and the spec's set is closed.
 */
function decodeOptions(options: ChallengeOptions): ChallengeOptions {
  const decoded = { ...options } as Record<string, unknown>;
  if (typeof decoded.challenge === 'string') {
    decoded.challenge = decodeBase64url(decoded.challenge).buffer;
  }
  const user = decoded.user as { id?: unknown } | undefined;
  if (user && typeof user.id === 'string') {
    decoded.user = { ...user, id: decodeBase64url(user.id).buffer };
  }
  for (const key of ['excludeCredentials', 'allowCredentials'] as const) {
    const list = decoded[key];
    if (!Array.isArray(list)) continue;
    decoded[key] = list.map((entry: { id?: unknown }) =>
      typeof entry?.id === 'string' ? { ...entry, id: decodeBase64url(entry.id).buffer } : entry,
    );
  }
  // Answered as the loose shape it came in as, and cast at the DOM call
  // rather than here. The two ceremonies' option types share no required
  // member, so an intersection would be a type no value can inhabit — and
  // the shape is the backend's to define in either case.
  return decoded;
}

/**
 * Rebuild the JSON envelope go-webauthn parses from a live credential.
 *
 * Written for both response kinds in one function because they differ in
 * exactly three members, and two functions sharing five would be two
 * places to keep the outer shape right.
 */
function encodeCredential(credential: PublicKeyCredential): string {
  const response = credential.response;
  const inner: Record<string, unknown> = {
    clientDataJSON: encodeBase64url(response.clientDataJSON),
  };
  // Which response arrived is decided by a member, never by `instanceof`:
  // the interface globals are themselves secure-context-only, so naming
  // one is a ReferenceError on precisely the pages this file already has
  // to survive.
  if ('attestationObject' in response) {
    const attestation = response as AuthenticatorAttestationResponse;
    inner.attestationObject = encodeBase64url(attestation.attestationObject);
    // What the authenticator says it can be reached by. Optional in the
    // API and absent on older engines, which is why it is conditional:
    // an empty list and "did not say" are the same to the backend, and
    // sending `null` would not parse.
    if (typeof attestation.getTransports === 'function') {
      const transports = attestation.getTransports();
      if (transports.length > 0) inner.transports = transports;
    }
  } else {
    const assertion = response as AuthenticatorAssertionResponse;
    inner.authenticatorData = encodeBase64url(assertion.authenticatorData);
    inner.signature = encodeBase64url(assertion.signature);
    // The account the authenticator resolved. REQUIRED for a discoverable
    // sign-in — it is the only thing naming whose credential this is —
    // and absent from a registration response, which names its account
    // from the ceremony instead.
    if (assertion.userHandle) inner.userHandle = encodeBase64url(assertion.userHandle);
  }
  return JSON.stringify({
    id: credential.id,
    rawId: encodeBase64url(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment ?? undefined,
    clientExtensionResults: credential.getClientExtensionResults(),
    response: inner,
  });
}

/**
 * Run one ceremony against the platform authenticator and answer the JSON
 * the backend verifies.
 *
 * `create` for a registration, `get` for either discoverable ceremony —
 * sign-in and step-up are byte-identical here, which is exactly why the
 * backend records a PURPOSE with the challenge rather than trusting the
 * shape (internal/identity/AGENTS.md § Passkeys).
 */
export async function answerChallenge(
  challenge: PasskeyChallenge,
  kind: 'create' | 'get',
): Promise<string> {
  if (!passkeysSupported()) throw new Error('This browser cannot use passkeys.');
  const publicKey = decodeOptions(challenge.options);
  let credential: Credential | null;
  try {
    credential =
      kind === 'create'
        ? await navigator.credentials.create({
            publicKey: publicKey as unknown as PublicKeyCredentialCreationOptions,
          })
        : await navigator.credentials.get({
            publicKey: publicKey as unknown as PublicKeyCredentialRequestOptions,
          });
  } catch (err) {
    // One DOM name covers "the person dismissed the prompt" and "the
    // authenticator declined", by design — the API refuses to tell a
    // caller which, so neither does this.
    if (err instanceof DOMException && err.name === 'NotAllowedError') {
      throw new PasskeyAbandonedError();
    }
    throw err;
  }
  if (credential === null) throw new PasskeyAbandonedError();
  return encodeCredential(credential as PublicKeyCredential);
}
