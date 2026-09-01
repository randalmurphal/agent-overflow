// Satisfying a step-up refusal with a passkey, and — more importantly —
// the cases where it must NOT try.
//
// A step-up token is spent by being presented, so the assertions that
// matter are about what the retry costs when it cannot work: nothing, and
// no prompt.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { setPasskeysAvailableFromBootstrap } from './passkey';
import { withStepUp } from './stepUp';
import { TransportError, wsClient } from './wsClient';

function stepUpRefusal(): TransportError {
  return new TransportError('step_up_required', 'needs a fresh proof');
}

/**
 * A page that can run a ceremony, answering with a fixed assertion. The
 * shape is passkey.test.ts's; what this file cares about is only that a
 * ceremony ran at all.
 */
function installAuthenticator(): { prompts: number } {
  const counter = { prompts: 0 };
  const credential = {
    id: 'cred-id',
    rawId: new Uint8Array([1]).buffer,
    type: 'public-key',
    authenticatorAttachment: null,
    getClientExtensionResults: () => ({}),
    response: {
      clientDataJSON: new Uint8Array([2]).buffer,
      authenticatorData: new Uint8Array([3]).buffer,
      signature: new Uint8Array([4]).buffer,
      userHandle: new Uint8Array([5]).buffer,
    },
  };
  Object.defineProperty(window, 'PublicKeyCredential', {
    configurable: true,
    value: function PublicKeyCredential() {},
  });
  Object.defineProperty(navigator, 'credentials', {
    configurable: true,
    value: {
      create: () => Promise.resolve(credential),
      get: () => {
        counter.prompts++;
        return Promise.resolve(credential);
      },
    },
  });
  return counter;
}

function stubCeremony(token = 'token-1') {
  setBindingMock('BeginPasskeyStepUp', async () => ({
    ceremonyId: 'cer-1',
    options: { challenge: 'AQID' },
  }));
  return setBindingMock('FinishPasskeyStepUp', async () => ({
    token,
    expiresAtMs: Date.now() + 120_000,
  }));
}

beforeEach(() => {
  resetBindingMocks();
  setPasskeysAvailableFromBootstrap(true);
});

afterEach(() => {
  resetBindingMocks();
  setPasskeysAvailableFromBootstrap(false);
  Reflect.deleteProperty(window, 'PublicKeyCredential');
  Reflect.deleteProperty(navigator, 'credentials');
  vi.restoreAllMocks();
});

describe('withStepUp', () => {
  it('does nothing at all to a call that was not refused', async () => {
    installAuthenticator();
    const finish = stubCeremony();
    const call = vi.fn(async () => 'ok');
    expect(await withStepUp(call)).toBe('ok');
    expect(call).toHaveBeenCalledTimes(1);
    expect(finish).not.toHaveBeenCalled();
  });

  it('proves the touch and runs the call once more, with the token armed', async () => {
    const prompts = installAuthenticator();
    stubCeremony('proof-abc');
    const armed = vi.spyOn(wsClient, 'withStepUpToken');
    let attempts = 0;
    const call = vi.fn(async () => {
      attempts++;
      if (attempts === 1) throw stepUpRefusal();
      return 'landed';
    });

    expect(await withStepUp(call)).toBe('landed');
    expect(call).toHaveBeenCalledTimes(2);
    expect(prompts.prompts).toBe(1);
    // The retry is the ONLY call the token rides, and it is armed around
    // the call rather than left standing.
    expect(armed).toHaveBeenCalledTimes(1);
    expect(armed.mock.calls[0]![0]).toBe('proof-abc');
  });

  it('never prompts when the backend offers no passkey', async () => {
    const prompts = installAuthenticator();
    setPasskeysAvailableFromBootstrap(false);
    const finish = stubCeremony();
    const refusal = stepUpRefusal();
    const call = vi.fn(async () => {
      throw refusal;
    });

    await expect(withStepUp(call)).rejects.toBe(refusal);
    expect(call).toHaveBeenCalledTimes(1);
    expect(prompts.prompts).toBe(0);
    expect(finish).not.toHaveBeenCalled();
  });

  it('never prompts on a page that cannot hold a credential', async () => {
    // No authenticator installed: the plain-HTTP LAN page, spec §15
    // constraint 6. The refusal reaches the caller with its own sentence.
    const finish = stubCeremony();
    const refusal = stepUpRefusal();
    const call = vi.fn(async () => {
      throw refusal;
    });

    await expect(withStepUp(call)).rejects.toBe(refusal);
    expect(call).toHaveBeenCalledTimes(1);
    expect(finish).not.toHaveBeenCalled();
  });

  it('rethrows the ORIGINAL refusal when the ceremony does not finish', async () => {
    installAuthenticator();
    setBindingMock('BeginPasskeyStepUp', async () => {
      throw new Error('ceremony unavailable');
    });
    const refusal = stepUpRefusal();
    const call = vi.fn(async () => {
      throw refusal;
    });

    // The WebAuthn error is not what happened from where the person sits:
    // their change did not go through, and the step-up sentence is what
    // says so.
    await expect(withStepUp(call)).rejects.toBe(refusal);
    expect(call).toHaveBeenCalledTimes(1);
  });

  it('leaves a refusal of any OTHER kind entirely alone', async () => {
    const prompts = installAuthenticator();
    stubCeremony();
    const scoped = new TransportError('scope_required', 'not granted');
    const call = vi.fn(async () => {
      throw scoped;
    });

    await expect(withStepUp(call)).rejects.toBe(scoped);
    expect(prompts.prompts).toBe(0);
  });

  it('retries exactly once, so a second refusal is the answer', async () => {
    installAuthenticator();
    stubCeremony();
    const call = vi.fn(async () => {
      throw stepUpRefusal();
    });

    await expect(withStepUp(call)).rejects.toBeInstanceOf(TransportError);
    // Two attempts, one prompt. A loop here would ask for a touch per lap.
    expect(call).toHaveBeenCalledTimes(2);
  });
});
