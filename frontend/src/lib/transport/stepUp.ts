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

// Through stores/bindings.ts like every other RPC in the app, and NOT a
// cycle: that module is a re-export of the generated tree, whose own
// runtime shim lands on ./handle.ts. Nothing in that chain reaches back
// here, because the transport holds this module through a slot it FILLS
// at boot rather than through an import.
import { BeginPasskeyStepUp, FinishPasskeyStepUp } from '../stores/bindings';
import { resolveTransport } from './handle';
import { answerChallenge, passkeysUsable, type PasskeyChallenge } from './passkey';
import { isStepUpRefusal } from './scopeRefusal';

/**
 * Install the passkey ceremony as the transport's step-up prover. Called
 * once, at boot (`src/main.ts`), and never per call site.
 */
export function installStepUpProof(): void {
  resolveTransport().installStepUpProver({ wants: canProve, prove: proveStepUp });
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
 * Run the step-up ceremony and answer the single-use token it minted.
 *
 * The token is bound to the session that BEGAN the ceremony, not to
 * anything a later call names, and the backend judges it against the
 * presenting connection when it is spent — so it is worth nothing to any
 * other session and there is nothing to protect it as it sits here.
 *
 * Rejecting means there was nothing to try: an abandoned prompt
 * (`PasskeyAbandonedError`), or a backend that refused the ceremony. The
 * transport turns either into the original refusal, so nobody is shown a
 * WebAuthn error for a change that simply did not go through.
 */
async function proveStepUp(): Promise<string> {
  const begun = await BeginPasskeyStepUp();
  const challenge: PasskeyChallenge = { ceremonyId: begun.ceremonyId, options: begun.options };
  const response = await answerChallenge(challenge, 'get');
  const grant = await FinishPasskeyStepUp(begun.ceremonyId, JSON.parse(response) as unknown);
  return grant.token;
}
