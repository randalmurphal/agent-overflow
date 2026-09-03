// Satisfying a step-up refusal with a passkey — where it happens now
// (one interception in the transport's dispatch path) and, more
// importantly, the cases where it must NOT try.
//
// Driven against a REAL WSClient over the fake socket, because the claims
// are about frames: which call carries the token, how many calls go out,
// and what the caller is told when the ceremony does not finish. A test
// against a stubbed `call()` could not see any of that — the token
// reaches a frame at construction time, and "exactly one retry" is a
// statement about what the socket saw.
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeCtor, flushMicrotasks, MockWebSocket } from '../../test/helpers/mockWebSocket';
import { resetBindingMocks } from '../../test/mocks/bindings-app';
import { PasskeyAbandonedError, setPasskeysAvailableFromBootstrap } from './passkey';
import { isStepUpRefusal } from './scopeRefusal';
import {
  BEGIN_PASSKEY_STEP_UP_ID,
  FINISH_PASSKEY_STEP_UP_ID,
  installStepUpProof,
} from './stepUp';
import { createWSClient, TransportError, wsClient, type StepUpProver, type WSClient } from './wsClient';

const bootstrap = async () => ({ wsUrl: 'ws://example/ws', token: 'test-token' });

/** An RPC frame as the socket received it. */
type SentFrame = Record<string, unknown>;

/**
 * A client with its socket open, brought up by a SUBSCRIBE so that every
 * RPC frame the fake records belongs to the case rather than to the
 * fixture.
 */
async function openClient(): Promise<{ client: WSClient; ws: MockWebSocket }> {
  const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
  client.subscribe('probe', () => {});
  await flushMicrotasks();
  const ws = MockWebSocket.instances[0]!;
  ws.acceptOpen();
  await flushMicrotasks();
  return { client, ws };
}

function rpcs(ws: MockWebSocket): SentFrame[] {
  return ws.sent.filter((frame) => frame.type === 'rpc');
}

/** Refuse a sent frame the way the backend's per-call gate does. */
function refuse(ws: MockWebSocket, frame: SentFrame, code = 'step_up_required'): void {
  ws.pushFrame({ type: 'rpc', id: frame.id, error: { code, message: 'needs a fresh proof' } });
}

/**
 * Wait for `what` to become true, on a WALL-CLOCK budget that FAILS when
 * it is spent (frontend/AGENTS.md § Testing). Everything being waited on
 * here is microtask work with no timer and no I/O in it, so a second is a
 * wedged-runtime tripwire rather than a race — the shape it catches is a
 * ceremony waiting on a call that is waiting on the ceremony.
 */
async function until(what: string, predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 1_000;
  while (!predicate()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
}

/** A prover that answers with `token` and counts its prompts. */
function stubProver(token = 'proof-abc'): StepUpProver & { prove: ReturnType<typeof vi.fn> } {
  return { wants: isStepUpRefusal, prove: vi.fn(async () => token) };
}

afterEach(() => {
  MockWebSocket.reset();
  resetBindingMocks();
  setPasskeysAvailableFromBootstrap(false);
  Reflect.deleteProperty(window, 'PublicKeyCredential');
  Reflect.deleteProperty(navigator, 'credentials');
  vi.restoreAllMocks();
});

beforeEach(() => {
  MockWebSocket.reset();
});

describe('the step-up interception', () => {
  it('proves the touch and dispatches the refused call once more, with the token on that frame alone', async () => {
    const { client, ws } = await openClient();
    const prover = stubProver();
    client.installStepUpProver(prover);

    const call = client.callByName('SetNetworkSettings', [{ bindAll: true }]);
    await until('the call to reach the socket', () => rpcs(ws).length === 1);
    refuse(ws, rpcs(ws)[0]!);
    await until('the refused call to be retried', () => rpcs(ws).length === 2);

    const [first, second] = rpcs(ws);
    // The retry is the SAME call — same method, same arguments — under a
    // new id, and the proof rides only it. A token on the first frame
    // would mean the slot leaked; one on a later frame would mean
    // somebody else's call spent this touch.
    expect(second).toMatchObject({
      method: 'SetNetworkSettings',
      params: [{ bindAll: true }],
      stepUpToken: 'proof-abc',
    });
    expect(first!.stepUpToken).toBeUndefined();
    expect(second!.id).not.toBe(first!.id);

    ws.pushFrame({ type: 'rpc', id: second!.id, result: 'saved' });
    await expect(call).resolves.toBe('saved');
    expect(prover.prove).toHaveBeenCalledTimes(1);
    client.close();
  });

  it('settles with the ORIGINAL refusal when the prompt is dismissed, and retries nothing', async () => {
    const { client, ws } = await openClient();
    const prover: StepUpProver = {
      wants: isStepUpRefusal,
      prove: vi.fn(async () => {
        throw new PasskeyAbandonedError();
      }),
    };
    client.installStepUpProver(prover);

    const call = client.callByName('MintDevicePairing', ['phone', 'full']);
    await until('the call to reach the socket', () => rpcs(ws).length === 1);
    refuse(ws, rpcs(ws)[0]!);

    // What happened from where the person sits is that the change did not
    // go through — not that WebAuthn raised something. `scopeRefusal.ts`
    // owns that sentence, and it can only own it if the refusal is what
    // arrives.
    const err = await call.then(
      () => null,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(TransportError);
    expect((err as TransportError).code).toBe('step_up_required');
    expect(rpcs(ws)).toHaveLength(1);
    client.close();
  });

  it('never lets the ceremony prove itself, so a refused ceremony call cannot wait on its own ceremony', async () => {
    // The recursion guard, stated as the deadlock it prevents: the
    // ceremony's own RPCs are dispatched while a ceremony is in flight,
    // and are therefore never intercepted. Were they, this case would
    // hang rather than fail — the second ceremony would queue behind the
    // first, which is waiting on the call that started the second.
    const { client, ws } = await openClient();
    const prove = vi.fn(async () => {
      await client.callByName('BeginPasskeyStepUpish', []);
      return 'unreachable';
    });
    client.installStepUpProver({ wants: isStepUpRefusal, prove });

    const call = client.callByName('SetProjectWorktreeSetup', ['p1', {}]);
    await until('the gated call to reach the socket', () => rpcs(ws).length === 1);
    refuse(ws, rpcs(ws)[0]!);
    await until("the ceremony's own call to reach the socket", () => rpcs(ws).length === 2);
    refuse(ws, rpcs(ws)[1]!);

    const err = await call.then(
      () => null,
      (e: unknown) => e,
    );
    expect((err as TransportError).code).toBe('step_up_required');
    expect(prove).toHaveBeenCalledTimes(1);
    // Two frames: the gated call and the ceremony's. A third would be a
    // retry of a ceremony call.
    expect(rpcs(ws)).toHaveLength(2);
    client.close();
  });

  it('puts one prompt on the screen at a time, and gives each refused call its own proof', async () => {
    const { client, ws } = await openClient();
    let live = 0;
    let mostLiveAtOnce = 0;
    const release: Array<() => void> = [];
    const prove = vi.fn(async () => {
      live++;
      mostLiveAtOnce = Math.max(mostLiveAtOnce, live);
      await new Promise<void>((resolve) => release.push(resolve));
      live--;
      return `proof-${prove.mock.calls.length}`;
    });
    client.installStepUpProver({ wants: isStepUpRefusal, prove });

    const first = client.callByName('SetNetworkSettings', [{ bindAll: true }]);
    const second = client.callByName('SetWSLDistroPreference', ['Ubuntu']);
    await until('both calls to reach the socket', () => rpcs(ws).length === 2);
    refuse(ws, rpcs(ws)[0]!);
    refuse(ws, rpcs(ws)[1]!);

    // One ceremony is open; the second refusal is waiting its turn rather
    // than raising a second prompt over the first.
    await until('the first ceremony to start', () => release.length === 1);
    expect(prove).toHaveBeenCalledTimes(1);
    release[0]!();
    await until('the second ceremony to start', () => release.length === 2);
    release[1]!();
    await until('both calls to be retried', () => rpcs(ws).length === 4);

    // A token proves ONE call, so sharing one across both would spend it
    // twice and the backend would refuse the second.
    expect(rpcs(ws).map((frame) => frame.stepUpToken)).toEqual([
      undefined,
      undefined,
      'proof-1',
      'proof-2',
    ]);
    expect(mostLiveAtOnce).toBe(1);

    for (const frame of rpcs(ws).slice(2)) {
      ws.pushFrame({ type: 'rpc', id: frame.id, result: 'ok' });
    }
    await expect(first).resolves.toBe('ok');
    await expect(second).resolves.toBe('ok');
    client.close();
  });

  it('leaves a refusal of any other kind entirely alone', async () => {
    const { client, ws } = await openClient();
    const prover = stubProver();
    client.installStepUpProver(prover);

    const call = client.callByName('MintDevicePairing', ['phone', 'full']);
    await until('the call to reach the socket', () => rpcs(ws).length === 1);
    refuse(ws, rpcs(ws)[0]!, 'scope_required');

    const err = await call.then(
      () => null,
      (e: unknown) => e,
    );
    expect((err as TransportError).code).toBe('scope_required');
    expect(prover.prove).not.toHaveBeenCalled();
    expect(rpcs(ws)).toHaveLength(1);
    client.close();
  });

  it('changes nothing when no prover is installed', async () => {
    // The plain-HTTP LAN page and any bundle whose boot has not installed
    // one: the refusal is the answer, and no round trip is spent learning
    // what the page already knows.
    const { client, ws } = await openClient();

    const call = client.callByName('SetNetworkSettings', [{ bindAll: true }]);
    await until('the call to reach the socket', () => rpcs(ws).length === 1);
    refuse(ws, rpcs(ws)[0]!);

    const err = await call.then(
      () => null,
      (e: unknown) => e,
    );
    expect((err as TransportError).code).toBe('step_up_required');
    expect(rpcs(ws)).toHaveLength(1);
    client.close();
  });

  it('retries exactly once, so a second refusal is the answer rather than a second touch', async () => {
    const { client, ws } = await openClient();
    const prover = stubProver();
    client.installStepUpProver(prover);

    const call = client.callByName('SetNetworkSettings', [{ bindAll: true }]);
    await until('the call to reach the socket', () => rpcs(ws).length === 1);
    refuse(ws, rpcs(ws)[0]!);
    await until('the retry', () => rpcs(ws).length === 2);
    refuse(ws, rpcs(ws)[1]!);

    const err = await call.then(
      () => null,
      (e: unknown) => e,
    );
    expect((err as TransportError).code).toBe('step_up_required');
    // A loop here would ask for a touch per lap, which trains somebody to
    // approve prompts that do not work.
    expect(prover.prove).toHaveBeenCalledTimes(1);
    expect(rpcs(ws)).toHaveLength(2);
    client.close();
  });
});

// ---------------------------------------------------------------------
// The prover that is actually installed, driven through the same client.
// ---------------------------------------------------------------------

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

/** The ceremony's own frames on the connection that refused. */
function ceremonyFrames(ws: MockWebSocket, methodId: number): SentFrame[] {
  return rpcs(ws).filter((frame) => frame.methodId === methodId);
}

/**
 * Answer the ceremony the way the REFUSING backend does, over its own
 * socket. The ceremony no longer goes through stores/bindings.ts: both
 * its methods route `home`, so a refusal on an attached machine would be
 * answered with a token minted on the wrong backend. What a test stubs is
 * frames, not bindings.
 */
async function answerBegin(ws: MockWebSocket): Promise<SentFrame> {
  await until(
    'the ceremony to begin on the refusing connection',
    () => ceremonyFrames(ws, BEGIN_PASSKEY_STEP_UP_ID).length === 1,
  );
  const begin = ceremonyFrames(ws, BEGIN_PASSKEY_STEP_UP_ID)[0]!;
  ws.pushFrame({
    type: 'rpc',
    id: begin.id,
    result: { ceremonyId: 'cer-1', options: { challenge: 'AQID' } },
  });
  return begin;
}

async function answerFinish(ws: MockWebSocket, token: string): Promise<SentFrame> {
  await until(
    'the ceremony to finish on the refusing connection',
    () => ceremonyFrames(ws, FINISH_PASSKEY_STEP_UP_ID).length === 1,
  );
  const finish = ceremonyFrames(ws, FINISH_PASSKEY_STEP_UP_ID)[0]!;
  ws.pushFrame({
    type: 'rpc',
    id: finish.id,
    result: { token, expiresAtMs: Date.now() + 120_000 },
  });
  return finish;
}

/**
 * The prover `installStepUpProof()` installs, captured rather than
 * rebuilt — a second construction here would be a second answer to what
 * the app actually runs. The install is intercepted so the singleton
 * transport is left as this suite found it.
 */
function installedProver(): StepUpProver {
  const install = vi.spyOn(wsClient, 'installStepUpProver').mockImplementation(() => {});
  installStepUpProof();
  const prover = install.mock.calls[0]![0];
  install.mockRestore();
  expect(prover, 'installStepUpProof must install a prover').toBeTruthy();
  return prover!;
}

describe('the installed passkey prover', () => {
  it('claims a step-up refusal, and only where a passkey could answer it', () => {
    installAuthenticator();
    const prover = installedProver();
    const refusal = new TransportError('step_up_required', 'needs a fresh proof');

    setPasskeysAvailableFromBootstrap(true);
    expect(prover.wants(refusal)).toBe(true);
    // Any other refusal is somebody else's to explain.
    expect(prover.wants(new TransportError('scope_required', 'not granted'))).toBe(false);
    expect(prover.wants(new Error('network down'))).toBe(false);
    // A backend with no canonical domain has no credential to assert
    // with, so a ceremony would spend a round trip to be refused again.
    setPasskeysAvailableFromBootstrap(false);
    expect(prover.wants(refusal)).toBe(false);
  });

  it('claims nothing on a page that cannot hold a credential', () => {
    // No authenticator installed: the plain-HTTP LAN page of spec §15
    // constraint 6, where the affordance is absent and everything else
    // keeps working.
    setPasskeysAvailableFromBootstrap(true);
    expect(
      installedProver().wants(new TransportError('step_up_required', 'needs a fresh proof')),
    ).toBe(false);
  });

  it('runs begin, assertion, finish — and the retried frame carries what the finish minted', async () => {
    const prompts = installAuthenticator();
    setPasskeysAvailableFromBootstrap(true);
    const { client, ws } = await openClient();
    client.installStepUpProver(installedProver());

    const call = client.callByName('MintDevicePairing', ['phone', 'full']);
    await until('the call to reach the socket', () => rpcs(ws).length === 1);
    refuse(ws, rpcs(ws)[0]!);

    // Both ceremony calls are on THIS connection, which is the whole
    // point: the token is minted for the session that began the ceremony
    // and judged against the session presenting it, so a ceremony run on
    // the page's own backend would prove nothing about an attached one.
    await answerBegin(ws);
    const finish = await answerFinish(ws, 'minted-token');
    // The finish names the ceremony the begin on THIS connection minted,
    // and carries the authenticator's answer to it.
    expect((finish.params as unknown[])[0]).toBe('cer-1');
    expect((finish.params as unknown[])[1]).toMatchObject({ id: 'cred-id' });

    await until('the retry', () => rpcs(ws).length === 4);
    expect(rpcs(ws)[3]).toMatchObject({
      method: 'MintDevicePairing',
      stepUpToken: 'minted-token',
    });
    expect(prompts.prompts).toBe(1);

    ws.pushFrame({ type: 'rpc', id: rpcs(ws)[3]!.id, result: { linkId: 'l1' } });
    await expect(call).resolves.toEqual({ linkId: 'l1' });
    client.close();
  });

  // The reason the handle is passed rather than resolved: a client
  // attached to a second machine refuses there, and both ceremony methods
  // are routed `home`. A ceremony that went through the generated
  // bindings would mint on the page's own backend, and the retry would
  // present a token for a session the refusing machine never issued.
  it('runs the ceremony on the ATTACHED connection that refused, not on the page own backend', async () => {
    installAuthenticator();
    setPasskeysAvailableFromBootstrap(true);
    const homeClient = await openClient();
    const attached = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    attached.subscribe('probe', () => {});
    await flushMicrotasks();
    const attachedWs = MockWebSocket.instances[1]!;
    attachedWs.acceptOpen();
    await flushMicrotasks();
    attached.installStepUpProver(installedProver());

    const call = attached.callByName('SetNetworkSettings', [{ bindAll: true }]);
    await until('the call to reach the attached socket', () => rpcs(attachedWs).length === 1);
    refuse(attachedWs, rpcs(attachedWs)[0]!);

    await answerBegin(attachedWs);
    await answerFinish(attachedWs, 'attached-token');
    await until('the retry on the attached socket', () => rpcs(attachedWs).length === 4);

    expect(rpcs(attachedWs)[3]).toMatchObject({
      method: 'SetNetworkSettings',
      stepUpToken: 'attached-token',
    });
    // Nothing at all crossed the page's own connection: not the begin,
    // not the finish, not the retry.
    expect(rpcs(homeClient.ws)).toHaveLength(0);

    attachedWs.pushFrame({ type: 'rpc', id: rpcs(attachedWs)[3]!.id, result: 'saved' });
    await expect(call).resolves.toBe('saved');
    attached.close();
    homeClient.client.close();
  });

  it('turns a dismissed prompt back into the refusal, not into a WebAuthn error', async () => {
    setPasskeysAvailableFromBootstrap(true);
    // A prompt answered with nothing is `PasskeyAbandonedError`: somebody
    // changed their mind, which is not a fault to report to them.
    Object.defineProperty(window, 'PublicKeyCredential', {
      configurable: true,
      value: function PublicKeyCredential() {},
    });
    Object.defineProperty(navigator, 'credentials', {
      configurable: true,
      value: { create: () => Promise.resolve(null), get: () => Promise.resolve(null) },
    });
    const { client, ws } = await openClient();
    client.installStepUpProver(installedProver());

    const call = client.callByName('SetNetworkSettings', [{ bindAll: true }]);
    await until('the call to reach the socket', () => rpcs(ws).length === 1);
    refuse(ws, rpcs(ws)[0]!);
    await answerBegin(ws);

    const err = await call.then(
      () => null,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(TransportError);
    expect((err as TransportError).code).toBe('step_up_required');
    // The begin, and nothing after it: no finish, no retry.
    expect(rpcs(ws)).toHaveLength(2);
    client.close();
  });
});

// A Wails method id is a hash of the method's NAME, so a rename moves it
// and a stale constant fails on the wire rather than at the compiler --
// the same failure mode `methodFamilies.test.ts` guards for the route
// families. The generated bindings are the reference because they are
// what methodgen wrote from the receiver itself.
describe('the ceremony method ids', () => {
  const generated = readFileSync(
    resolve(dirname(fileURLToPath(import.meta.url)), '../../../bindings/agent-overflow/app.ts'),
    'utf8',
  );

  function generatedId(methodName: string): number {
    const declaration = generated.indexOf(`export function ${methodName}(`);
    expect(declaration, `${methodName} is not a bound method any more`).toBeGreaterThan(-1);
    const call = /\$Call\.ByID\((\d+)/.exec(generated.slice(declaration));
    expect(call, `${methodName} issues no Call.ByID`).not.toBeNull();
    return Number(call![1]);
  }

  it('match the generated bindings', () => {
    expect(BEGIN_PASSKEY_STEP_UP_ID).toBe(generatedId('BeginPasskeyStepUp'));
    expect(FINISH_PASSKEY_STEP_UP_ID).toBe(generatedId('FinishPasskeyStepUp'));
  });
});
