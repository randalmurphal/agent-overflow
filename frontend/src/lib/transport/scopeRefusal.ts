import { TransportError } from './wsClient';
import { passkeysUsable } from './passkey';
import { isScope, type Scope } from './scopes';

/**
 * The client-side half of the AUTHORIZATION refusal vocabulary — the
 * sibling of ./authReason.ts, which covers the CREDENTIAL half.
 *
 * The two are separate because the remedies are different in kind. A
 * credential refusal says "this device is not signed in, sign in again";
 * a scope refusal says "this device IS signed in, and was not granted
 * this". One module branching on both would put "pair again" in front of
 * somebody whose pairing is fine.
 *
 * The backend answers `scope_required` with the missing capability in a
 * FIELD (internal/transport/authorize.go scopeRefusal), because a method
 * error's prose is redacted for a non-loopback caller — the field is the
 * whole answer that survives the wire. `step_up_required` carries no
 * field: no grant satisfies it, so there is nothing to name.
 *
 * This is the REACTIVE backstop. The proactive half is ./scopes.ts, which
 * a gated surface reads to disable a control before anybody presses it.
 * A refusal reaching here means the two disagreed — a grant narrower than
 * the page believed, a method whose authority depends on its ARGUMENTS
 * rather than its name (transport.ScopeRequired), or a revocation that
 * landed mid-session — so it explains rather than blaming the person.
 *
 * It always answers. A capability name this bundle does not know degrades
 * to the generic sentence rather than showing nothing, for the reason
 * ./authReason.ts gives: a surface that silently does not work is worse
 * than one that says so vaguely. That case is expected, not exotic — a
 * tab stays loaded across a backend update, and a newer backend may
 * refuse with a capability this build has no word for.
 */

/**
 * What to show for a refusal: one sentence naming what is missing, and
 * one saying where it comes from.
 */
export interface ScopeRefusalPresentation {
  /** What happened, naming the capability when the backend named one. */
  title: string;
  /** Where the capability comes from, or why there is no self-service fix. */
  hint: string;
}

/**
 * How each GRANTABLE capability reads to a person. Short noun phrases,
 * because they are substituted into one sentence frame rather than read
 * alone, and because the wire spelling (`threads:autonomy`) is a
 * vocabulary for the audit log rather than for a screen.
 *
 * `host` and `session` are excluded by the type rather than worded here:
 * neither is a grant anybody can be given, so the sentence frame's hint —
 * "widen this device's access" — would be a false instruction for both.
 * Each gets its own presentation in `presentScope`, and the Exclude is
 * what makes a third such value decide on one too.
 */
const CAPABILITY_NOUNS: Record<Exclude<Scope, 'host' | 'session'>, string> = {
  'threads:read': 'reading threads',
  'files:read': 'reading files and diffs',
  'threads:operate': 'sending and steering threads',
  'approvals:respond': 'answering approvals',
  'threads:autonomy': 'running workflows and autonomous modes',
  'terminal:operate': 'using the terminal',
  'git:operate': 'git actions',
  'attachments:write': 'uploading attachments',
  'settings:read': 'reading settings',
  'settings:write': 'changing settings',
  'access:admin': 'managing devices and accounts',
};

/** Where a grant comes from. One sentence, reused so it stays one sentence. */
const GRANT_HINT = "Widen this device's access under Settings → Network → Devices on that computer.";

/**
 * The refusal shown when the backend named a capability this build has no
 * word for, or named none at all. Says what is true — this device was not
 * granted it — without inventing which one.
 */
export const UNKNOWN_SCOPE_REFUSAL: ScopeRefusalPresentation = {
  title: 'This device was not granted access to that.',
  hint: GRANT_HINT,
};

/**
 * The floor refusal, which a session caller cannot reach: `session` is
 * satisfied by naming a live session, and only a connection that named
 * one is judged by that gate at all. It exists because the presentation
 * must answer for every name the wire can carry — a bundle live against a
 * newer backend sees whatever that backend refuses with — and because
 * "ask for a wider grant" is the wrong remedy for a session the backend
 * did not accept.
 */
export const SESSION_REFUSAL: ScopeRefusalPresentation = {
  title: 'This device is no longer paired with this backend.',
  hint: 'Pair it again under Settings → Network → Devices on that computer.',
};

/**
 * The step-up refusal. Its own presentation rather than a shade of the
 * one above, because no grant can satisfy it: the remedy is to prove
 * somebody is present, not to be given something.
 */
export const STEP_UP_REFUSAL: ScopeRefusalPresentation = {
  title: 'That change can only be made at the computer running Agent Overflow.',
  hint: 'Open Agent Overflow there and make it from that screen.',
};

/**
 * The same refusal where a passkey is the second proof (./passkey.ts).
 *
 * Reaching it means the ceremony did not finish — ./stepUp.ts runs one
 * automatically and retries — so the sentence names the touch rather than
 * the machine. Keeping both is the honest arrangement: on a backend with
 * no canonical domain there IS no second proof, and telling somebody to
 * use a passkey they cannot register sends them nowhere.
 */
export const STEP_UP_PASSKEY_REFUSAL: ScopeRefusalPresentation = {
  title: 'That change needs you to confirm it is you.',
  hint: 'Try again and confirm with your passkey, or make the change at the computer running Agent Overflow.',
};

/**
 * Which step-up sentence this backend can honestly show, asked per
 * refusal rather than resolved once: the canonical domain is a live
 * setting, so a page open across the change would otherwise keep naming
 * a remedy that stopped (or started) existing.
 */
function stepUpRefusal(): ScopeRefusalPresentation {
  return passkeysUsable() ? STEP_UP_PASSKEY_REFUSAL : STEP_UP_REFUSAL;
}

/** Whether this call was refused for want of a capability. */
export function isScopeRefusal(err: unknown): err is TransportError {
  return err instanceof TransportError && err.code === 'scope_required';
}

/** Whether this call was refused for want of a fresh presence proof. */
export function isStepUpRefusal(err: unknown): err is TransportError {
  return err instanceof TransportError && err.code === 'step_up_required';
}

/**
 * Turn a capability name into a sentence. Exported for the disabled-state
 * path, which knows what it needs BEFORE anything is refused and should
 * say the same thing the refusal would.
 */
export function presentScope(scope: string | undefined | null): ScopeRefusalPresentation {
  if (!isScope(scope)) return UNKNOWN_SCOPE_REFUSAL;
  if (scope === 'host') {
    // Not a grant, so the grant hint would be a false instruction.
    return stepUpRefusal();
  }
  if (scope === 'session') return SESSION_REFUSAL;
  return {
    title: `This device was not granted ${CAPABILITY_NOUNS[scope]}.`,
    hint: GRANT_HINT,
  };
}

/**
 * The presentation for a caught transport error, or null when it was not
 * an authorization refusal. The null answer is what keeps a caller from
 * reporting an ordinary method failure as a permissions problem.
 */
export function scopeRefusalPresentation(err: unknown): ScopeRefusalPresentation | null {
  if (isStepUpRefusal(err)) return stepUpRefusal();
  if (isScopeRefusal(err)) return presentScope(err.scope);
  return null;
}

/**
 * The one-line form, for a toast or a title attribute. Null on anything
 * that was not an authorization refusal, so a call site can fall through
 * to its ordinary error path with one check.
 */
export function scopeRefusalMessage(err: unknown): string | null {
  const presentation = scopeRefusalPresentation(err);
  return presentation === null ? null : `${presentation.title} ${presentation.hint}`;
}
