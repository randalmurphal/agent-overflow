// Waking a phone that is not connected, end to end
// (docs/specs/remote-access.md §9, "Push").
//
// WHY THIS FILE EXISTS. The Go tests drive the fan-out with hand-built
// payloads and the Vitest tests drive the phone's half with a fake
// plugin. Neither can answer the question the feature is about: does a
// REAL turn on a REAL provider, through the production mapping, the
// production dispatch queues, the production per-device preference gate
// and the production message constructor, reach one registered phone
// with exactly what §9 allows and nothing else. Every layer in that
// sentence is live here.
//
// WHAT IS SUBSTITUTED, and it is exactly one thing: the hop that would
// have talked to Google. `InstallHarnessPushSender` puts a recorder in
// the `push.Sender` seam at harness boot
// (`internal/app/app_push_harness.go`), and `HarnessPushSent` reads it
// back. Everything ABOVE that seam is the shipping code, so the
// redaction assertions below are assertions about the real payload
// rather than about a re-description of it.
//
// WHAT ONLY A DEVICE CAN PROVE: that Firebase accepts the message, that
// the phone's service is handed it, and that `TrayNotifier` renders it.
// The first needs a Firebase project; the second and third need an APK
// carrying a `google-services.json` (mobile/AGENTS.md §
// `google-services.json`, and this box). `TrayNotifierTest` covers the
// decision half of the third on the JVM.
//
// WHY IT PAIRS OVER LOOPBACK rather than over a LAN address like the
// off-host specs. Nothing here is about WHERE the phone is: the push
// path keys everything off the calling session's DEVICE row, which a
// loopback pairing produces just as truthfully. Avoiding the LAN bind is
// what keeps this file on the shared worker backend, because that bind
// persists to the settings file and `harness.reset()` does not undo it.
//
// WHY IT IS SERIAL, and what each case re-stages. Devices, their push
// registrations and the settings file are ACCESS and PREFERENCE state,
// which `HarnessReset` deliberately does not sweep — it resets threads
// and projects, not who is allowed in or what they asked for. So the
// phone is paired ONCE and carried forward, while the thread, the
// scenario and the recorded ledger are per-case because the reset takes
// all three.

import { expect, test } from './fixtures.js';
import {
  RESULT_LINE,
  emit,
  seedAgentThread,
  startMock,
  textLines,
} from './agent-visibility-helpers.js';
import {
  WireClient,
  answered,
  mintLink,
  pairDeviceOverWire,
  wsTicket,
  type PairingInvite,
} from './offhost-helpers.js';
import type { HarnessApp } from '../src/harness.js';

/** One recorded wake (`internal/harnessrpc/push.go`). */
interface PushMessage {
  token: string;
  tag: string;
  data: Record<string, string>;
}

/** The `notify.Send` shape, as the wire spells it. */
interface NotificationSend {
  id: string;
  kind: string;
  retract?: boolean;
  title: string;
  body: string;
  target: { kind: string; threadId?: string };
}

const DEVICE_KEY = 'e2e-push-phone-device-key';
const DEVICE_LABEL = 'e2e push phone';
const TOKEN = 'e2e-registration-token-1';
const ROTATED = 'e2e-registration-token-2';

/**
 * Four scripted turns on one thread, each answering with a line and a
 * result so every send completes rather than leaving a turn open at
 * teardown. Same shape `notifications.spec.ts` uses, for the same reason.
 */
function fourTurnScenario() {
  return {
    version: 1,
    name: 'push-turns',
    provider: 'claude',
    turns: [1, 2, 3, 4].map((n) => ({
      label: `turn-${n}`,
      steps: [emit([...textLines(`msg-${n}`, `Answer ${n}.`), RESULT_LINE])],
    })),
    afterTurns: 'silent',
  };
}

const turnComplete = (threadId: string) => (data: NotificationSend) =>
  !data.retract && data.kind === 'turn-complete' && data.target.threadId === threadId;

/** A thread on the mock provider, ready to be asked a question. */
async function stagedThread(harness: HarnessApp, project: string, title: string): Promise<string> {
  await harness.rpc('HarnessSetScenario', { scenario: fourTurnScenario() });
  const threadId = await seedAgentThread(harness, project, title);
  await startMock(harness, threadId);
  return threadId;
}

/**
 * Ask a question and wait for the completion to reach the wire.
 *
 * The `notification:send` event is the BARRIER, not the assertion: it
 * says the mapping ran and dispatched, which is what puts the push job on
 * its own queue behind it. Absence assertions need a barrier rather than
 * a timeout, and this is the one every case here uses.
 */
async function askAndSettle(harness: HarnessApp, threadId: string, text: string): Promise<void> {
  const completed = harness.waitForEvent<NotificationSend>(
    'notification:send',
    turnComplete(threadId),
  );
  await harness.rpc('SendMessage', threadId, text, null);
  await completed;
}

/** Ask a question and wait for the retraction its START withdraws. */
async function askAndWithdraw(harness: HarnessApp, threadId: string, text: string): Promise<void> {
  const withdrawn = harness.waitForEvent<NotificationSend>(
    'notification:send',
    (data) => Boolean(data.retract) && data.id === `thread:${threadId}`,
  );
  await harness.rpc('SendMessage', threadId, text, null);
  await withdrawn;
}

/**
 * Everything recorded so far, once at least `atLeast` messages exist.
 *
 * Polled rather than awaited on an event, because the push fan-out runs
 * on its OWN serial queue behind the notification queue: the wire event
 * says the mapping ran, not that the wake did.
 */
async function wakes(harness: HarnessApp, atLeast: number): Promise<PushMessage[]> {
  let sent: PushMessage[] = [];
  await expect
    .poll(
      async () => {
        sent = await harness.rpc<PushMessage[]>('HarnessPushSent');
        return sent.length;
      },
      { message: `the push fan-out must record at least ${atLeast} message(s)` },
    )
    .toBeGreaterThanOrEqual(atLeast);
  return sent;
}

test.describe.serial('phone push', () => {
  let phone: WireClient | null = null;

  test.afterAll(() => {
    phone?.close();
  });

  /** Pair once; every later case rides the same device row. */
  async function pairedPhone(harness: HarnessApp): Promise<WireClient> {
    if (phone !== null) return phone;
    const port = harness.bootstrap.port;
    const link = mintLink(await harness.rpc<PairingInvite>('MintDevicePairing', 'phone', 'full'));
    const grant = await pairDeviceOverWire(harness, '127.0.0.1', port, link, DEVICE_KEY, DEVICE_LABEL);
    phone = await WireClient.connect(
      '127.0.0.1',
      port,
      await wsTicket('127.0.0.1', port, grant, DEVICE_KEY),
    );
    return phone;
  }

  test('a registered phone is woken by a completed turn, and told almost nothing', async ({
    harness,
  }) => {
    const client = await pairedPhone(harness);
    // The registration names NO device: the row it writes is the one
    // behind this connection's own session, which is what makes it safe
    // at the session floor (`app_push.go`, callerPushDevice).
    answered(
      await client.call('RegisterPushToken', 'android', TOKEN),
      'a paired phone must be able to register its own token',
    );

    const status = (await harness.rpc('GetPushSenderStatus')) as {
      configured: boolean;
      registeredDevices: number;
    };
    expect(status.configured, 'the harness recorder must present as a configured sender').toBe(true);
    expect(status.registeredDevices).toBe(1);

    const threadId = await stagedThread(harness, 'push-mapping', 'Rewrite the parser');
    const completed = harness.waitForEvent<NotificationSend>(
      'notification:send',
      turnComplete(threadId),
    );
    await harness.rpc('SendMessage', threadId, 'first question', null);
    // The DESKTOP notification names the thread. That contrast is the
    // point of the assertions below: one delivery stays on this machine,
    // the other transits a third party.
    expect((await completed).title).toBe('Rewrite the parser');

    // A thread's ledger OPENS with a retraction, and that is not noise: a
    // turn's start withdraws whatever rest notification that thread had,
    // unconditionally and before any work happens, so a phone woken by the
    // previous turn stops showing a stale prompt the moment the agent goes
    // back to work. The two rode one serial queue, so their order is the
    // contract rather than a race.
    const [withdrawn, woken, ...extra] = await wakes(harness, 2);
    expect(withdrawn.data.retract, 'a turn start withdraws before it works').toBe('1');
    expect(extra, 'a completed turn wakes a registered phone exactly once').toEqual([]);
    expect(woken.token).toBe(TOKEN);
    expect(woken.tag).toBe(`thread:${threadId}`);
    expect(woken.data.id).toBe(`thread:${threadId}`);
    expect(woken.data.kind).toBe('turn-complete');
    // A fixed phrase, and the machine. Never the thread.
    expect(woken.data.title).toBe('Turn complete');
    expect(woken.data.body).toBeTruthy();
    expect(woken.data.retract).toBeUndefined();
    expect(JSON.stringify(woken.data)).not.toContain('Rewrite the parser');
    expect(JSON.stringify(woken.data)).not.toContain('Answer 1');

    // The route travels as ONE document, because `Target`'s own JSON
    // spells a `kind` and so does the notification's (`push.KeyTarget`).
    expect(JSON.parse(woken.data.target)).toMatchObject({ kind: 'thread', threadId });
  });

  test('working the thread again withdraws the wake under the same tag', async ({ harness }) => {
    // Paired, registered and still live from the case above: this one is
    // about the WITHDRAWAL, not about arriving again.
    await pairedPhone(harness);
    const threadId = await stagedThread(harness, 'push-retraction', 'Withdrawn thread');
    await askAndSettle(harness, threadId, 'first question');
    await askAndWithdraw(harness, threadId, 'second question');

    const recorded = await wakes(harness, 2);
    const retraction = recorded.find((message) => message.data.retract === '1');
    expect(retraction, 'a retraction must reach the phone as its own message').toBeTruthy();
    expect(retraction!.tag).toBe(`thread:${threadId}`);
    expect(retraction!.data.id).toBe(`thread:${threadId}`);
    // Nothing to render and nowhere to go: the narrower contract
    // `notify.ValidateSend` holds a retraction to.
    expect(retraction!.data.title).toBeUndefined();
    expect(retraction!.data.body).toBeUndefined();
    expect(retraction!.data.target).toBeUndefined();
    // One tag for both, which is what makes the phone REPLACE rather than
    // stack, and what lets the withdrawal cancel exactly the right one.
    expect(new Set(recorded.map((message) => message.tag)).size).toBe(1);
  });

  test("the phone's own toggle silences its wakes, and never its retractions", async ({
    harness,
  }) => {
    const client = await pairedPhone(harness);
    // Written over the PHONE's connection, so it lands in that device's
    // own settings bucket — the same one `pushAllowed` reads. The owner's
    // desktop toggles are a different screen and do not move by this.
    answered(
      await client.call('UpdateSettings', { notifyTurnComplete: false }),
      "a paired device must be able to set its own screen's preferences",
    );

    const threadId = await stagedThread(harness, 'push-prefs', 'Silenced thread');
    await askAndSettle(harness, threadId, 'first question');
    await askAndWithdraw(harness, threadId, 'second question');

    const recorded = await wakes(harness, 1);
    expect(
      recorded.filter((message) => message.data.retract !== '1'),
      'a device whose toggle is off must not be woken',
    ).toEqual([]);
    expect(
      recorded.filter((message) => message.data.retract === '1').length,
      'a retraction is never gated: a toggle flipped mid-flight must not strand a lock screen',
    ).toBeGreaterThan(0);
  });

  test('revoking the device stops every wake, whatever it registered', async ({ harness }) => {
    const client = await pairedPhone(harness);
    // A dead session cannot re-register, so the token is refreshed first:
    // what is under test is that a REVOKED row is not woken, not that a
    // revoked session cannot call.
    answered(
      await client.call('RegisterPushToken', 'android', ROTATED),
      'a rotated token must replace the row rather than add one',
    );

    const overview = (await harness.rpc('GetAccessOverview')) as {
      devices: { id: string; label: string }[];
    };
    const deviceId = overview.devices.find((device) => device.label === DEVICE_LABEL)?.id ?? '';
    expect(deviceId, 'the paired phone must be on the access list').toBeTruthy();
    await harness.rpc('RevokeAccessDevice', deviceId);

    const status = (await harness.rpc('GetPushSenderStatus')) as { registeredDevices: number };
    expect(status.registeredDevices, 'a revoked device is not a live registration').toBe(0);

    const threadId = await stagedThread(harness, 'push-revoked', 'Revoked thread');
    await askAndSettle(harness, threadId, 'first question');
    // The retraction that rides the next turn's start is the barrier, and
    // a revoked device gets neither it nor the completion.
    await askAndWithdraw(harness, threadId, 'second question');

    expect(
      await harness.rpc<PushMessage[]>('HarnessPushSent'),
      'nothing at all is sent to a device the owner revoked',
    ).toEqual([]);
  });
});
