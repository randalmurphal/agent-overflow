// Satisfying a step-up refusal from a device that is not the computer.
//
// A handful of calls need a proof that somebody is present RIGHT NOW
// rather than a grant somebody was given: re-keying the network, writing
// argv that later runs unattended, minting a credential
// (docs/specs/remote-access.md §4). Until passkeys the only such proof was
// host presence, so a remote owner simply could not make those changes;
// now an assertion from a registered credential is a second one.
//
// **This is a retry, not an interceptor.** It wraps a call the person just
// pressed a button for, because the ceremony puts a biometric prompt on
// their screen — and a prompt raised by a pane mounting, or by a
// background refresh, is one nobody asked for and nobody can attribute.
// So it is applied per deliberate action, never at the transport door.
//
// Exactly ONE retry, and no loop. A second refusal after a proof was
// accepted means the call was refused for a different reason than the
// first, and asking for another touch would train somebody to approve
// prompts that do not work.

// Through stores/bindings.ts like every other RPC in the app, and NOT a
// cycle: that module is a re-export of the generated tree, whose own
// runtime shim lands on ./handle.ts — nothing in that chain reaches back
// here, because this module is a leaf its callers press a button into.
import { BeginPasskeyStepUp, FinishPasskeyStepUp } from '../stores/bindings';
import { resolveTransport } from './handle';
import { answerChallenge, passkeysUsable, type PasskeyChallenge } from './passkey';
import { isStepUpRefusal } from './scopeRefusal';

/**
 * Run `call`, and if the backend refuses it for want of a fresh proof,
 * prove it with a passkey and run it once more.
 *
 * Rethrows the ORIGINAL refusal when there is nothing to try — no passkey
 * on this page, or the person dismissed the prompt — so the caller's
 * ordinary error path shows the step-up sentence rather than a WebAuthn
 * one. What went wrong from where they sit is that the change did not go
 * through, and ./scopeRefusal.ts owns that wording.
 */
export async function withStepUp<T>(call: () => Promise<T>): Promise<T> {
  try {
    return await call();
  } catch (err) {
    if (!isStepUpRefusal(err) || !passkeysUsable()) throw err;
    let token: string;
    try {
      token = await proveStepUp();
    } catch {
      throw err;
    }
    // Armed and dispatched in one synchronous step: the token is spent by
    // being presented, so the slot must not be left standing across an
    // await where another surface's call could take it.
    return await resolveTransport().withStepUpToken(token, call);
  }
}

/**
 * Run the step-up ceremony and answer the single-use token it minted.
 *
 * The token is bound to the session that BEGAN the ceremony, not to
 * anything this call names, and the backend judges it against the
 * presenting connection when it is spent — so it is worth nothing to any
 * other session and there is nothing to protect it as it sits here.
 */
async function proveStepUp(): Promise<string> {
  const begun = await BeginPasskeyStepUp();
  const challenge: PasskeyChallenge = { ceremonyId: begun.ceremonyId, options: begun.options };
  const response = await answerChallenge(challenge, 'get');
  const grant = await FinishPasskeyStepUp(begun.ceremonyId, JSON.parse(response) as unknown);
  return grant.token;
}
