// Satisfying a step-up refusal from a device that is not the computer.
//
// A handful of calls need a proof that somebody is present RIGHT NOW
// rather than a grant somebody was given: re-keying the network, writing
// argv that later runs unattended, minting a credential
// (docs/specs/remote-access.md §4). Until passkeys the only such proof was
// host presence, so a remote owner simply could not make those changes;
// now an assertion from a registered credential is a second one.
//
// **This module is the ceremony, and nothing else.** Where it runs is
// ./wsClient.ts's one interception in the RPC dispatch path: a refused
// call proves the touch and is dispatched once more with the token, and
// its caller sees only the outcome. The alternative — wrapping each gated
// call at its own call site — is a wrapper the next `//ao:stepup`
// method's UI forgets, and the forgetting is invisible where it is
// written, because on the owner's own machine host presence satisfies the
// gate and no ceremony ever runs. That was the state after wave 8f: one
// wrapped call site, and every other gated surface silently unreachable
// from a phone.
//
// A prompt nobody asked for is the cost this shape has to keep paying
// attention to, and the gate's own shape is what pays it: every
// `//ao:stepup` method is a WRITE somebody pressed a button for
// (internal/transport/AGENTS.md), so a passive load — a pane mounting, a
// background refresh — has nothing here to trip.

// The ceremony's two RPCs go through the REFUSING connection rather than
// through stores/bindings.ts, and that is the one thing this module does
// not get to choose freely. Both methods are routed `home`
// (./methodRoutes.ts), so a call refused on an ATTACHED machine would be
// answered with a token minted on the page's own backend: bound to a
// different session, refused on arrival, and the person would see the
// change fail after touching their sensor. The transport hands the
// ceremony the handle that refused (`StepUpTarget`), so mint and spend
// are the same session by construction.
import { installStepUpProverEverywhere } from './backends';
import { answerChallenge, passkeysUsable, type PasskeyChallenge } from './passkey';
import { isStepUpRefusal } from './scopeRefusal';
import type { StepUpTarget } from './wsClient';

// The two method ids the ceremony calls by, hand-written because the
// generated tables carry no name-to-id map at runtime: `methodRoutes.ts`
// is keyed BY id and names the method only in a comment.
//
// A Wails method id is a hash of the method's name, so a rename moves it
// and a stale constant fails at the wire rather than at the compiler.
// `stepUp.test.ts` reads the generated bindings and pins both, which is
// the same tripwire `methodFamilies.test.ts` is for the route families.
export const BEGIN_PASSKEY_STEP_UP_ID = 3214812657;
export const FINISH_PASSKEY_STEP_UP_ID = 1569276637;

/**
 * The shape BeginPasskeyStepUp answers with (app.PasskeyChallengeResult),
 * which is `PasskeyChallenge` itself: the ceremony id plus the WebAuthn
 * options blob, crossing this layer unread.
 */
type BegunCeremony = PasskeyChallenge;

/** The shape FinishPasskeyStepUp answers with (app.PasskeyStepUpGrant). */
interface StepUpGrant {
  token: string;
}

/**
 * Install the passkey ceremony as the transport's step-up prover. Called
 * once, at boot (`src/main.ts`), and never per call site.
 *
 * EVERY attached handle, and every handle attached afterwards
 * (./backends.ts owns that half). A per-connection install done once at
 * boot would leave a backend attached later unable to satisfy a step-up
 * refusal, and the omission would be invisible on the owner's own machine
 * — the same shape as the per-call-site wrapping this module exists to
 * replace.
 */
export function installStepUpProof(): void {
  installStepUpProverEverywhere({ wants: canProve, prove: proveStepUp });
}

/**
 * Whether a refusal is one a ceremony could satisfy from this page: the
 * backend refused for want of a fresh proof, and this page can produce
 * one at all.
 *
 * Both halves, and this is the ONLY predicate — a page with no passkey
 * (no canonical domain on this backend, or no secure context) must not
 * spend a round trip to be refused a second time, and its caller's
 * ordinary error path already shows the step-up sentence
 * (./scopeRefusal.ts).
 */
function canProve(err: unknown): boolean {
  return isStepUpRefusal(err) && passkeysUsable();
}

/**
 * Run the step-up ceremony ON THE CONNECTION THAT REFUSED, and answer the
 * single-use token it minted.
 *
 * The token is bound to the session that BEGAN the ceremony, not to
 * anything a later call names, and the backend judges it against the
 * presenting connection when it is spent, which is exactly why the
 * ceremony runs on the refusing handle. Running it anywhere else mints a
 * proof for the wrong session, and it is worth nothing to any other
 * session, so there is nothing to protect it as it sits here.
 *
 * Rejecting means there was nothing to try: an abandoned prompt
 * (`PasskeyAbandonedError`), or a backend that refused the ceremony. The
 * transport turns either into the original refusal, so nobody is shown a
 * WebAuthn error for a change that simply did not go through.
 */
async function proveStepUp(target: StepUpTarget): Promise<string> {
  const begun = (await target.callByID(BEGIN_PASSKEY_STEP_UP_ID, [])) as BegunCeremony;
  const response = await answerChallenge(begun, 'get');
  const grant = (await target.callByID(FINISH_PASSKEY_STEP_UP_ID, [
    begun.ceremonyId,
    JSON.parse(response) as unknown,
  ])) as StepUpGrant;
  return grant.token;
}
