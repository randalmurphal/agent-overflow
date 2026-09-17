// A notification on a screen that is not the backend machine's.
//
// `notifications.spec.ts` covers the HOST pipe: `notifyOS`, the gate against
// the backend screen's own settings, the `notification:send` frame a host-side
// presenter consumes. This file covers the other screen — a real browser
// paired from what the backend can only see as another machine — where three
// things have to be true at once and none of them is provable without a real
// off-host peer:
//
//   - the frame REACHES that page and something there presents it. Until the
//     browser presenter landed, `notification:send` arrived and nothing
//     consumed it, so the page's whole notification settings block decided
//     nothing.
//   - the page's OWN preferences and OWN focus decide it. The host's answer
//     is about the desk.
//   - `notification:sound` does NOT reach it. That channel is loopback-only
//     because the cue behind it was resolved from the desk's settings, and
//     the only place a channel's audience policy is observable is the wire:
//     to a paired device it is an absence, which no screen assertion can tell
//     apart from a backend that had nothing to say.
//
// WHAT IS REAL HERE. The pairing is the shipped screen and the shipped
// redemption (`offhost-helpers.ts`, shared with the lifecycle and
// preview-gateway specs); the page is the shipped `App.svelte`; the
// preference is set by clicking the radio in the shipped settings page, so
// the write lands in THAT device's bucket the way a person's would. The one
// stub is `window.Notification`: a raised banner is an OS artefact Playwright
// cannot read, and the honest assertion is what the page asked the platform
// for.
//
// WHY IT OWNS ITS BACKEND: the LAN bind persists to the settings file and
// rebinds the listener, and `harness.reset()` undoes neither — the same
// reason `harness-remote-device-lifecycle.spec.ts` owns its instance.

import { expect, test, type BrowserContext, type Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  PAIRED_APP_MOUNT_MS,
  confirmOnHost,
  instrument,
  mintInvite,
  mintLink,
  nonLoopbackIPv4,
  redeemOnScreen,
  type Surfaced,
} from './offhost-helpers.js';
import { RESULT_LINE, emit, seedAgentThread, startMock, textLines } from './agent-visibility-helpers.js';

/** What the page asked the platform for, recorded by the stub below. */
interface RaisedNotification {
  title: string;
  body: string;
  tag: string;
  silent: boolean;
}

interface NotificationProbe {
  raised: RaisedNotification[];
  closed: string[];
}

/**
 * Replace `window.Notification` before any app script runs.
 *
 * Granted rather than prompting: the permission is a browser-profile state
 * Playwright cannot answer from the page, and what this spec is about is the
 * decision and the payload, not the ceremony (that is
 * `NotificationsSection.test.ts`). The recorder is the whole surface the
 * presenter touches — the constructor, `permission`, and `close`.
 */
async function stubNotifications(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const probe = { raised: [] as RaisedNotification[], closed: [] as string[] };
    (window as unknown as { __aoNotifications: typeof probe }).__aoNotifications = probe;
    class StubNotification {
      static readonly permission = 'granted';
      static requestPermission(): Promise<string> {
        return Promise.resolve('granted');
      }

      onclick: (() => void) | null = null;
      onclose: (() => void) | null = null;
      private readonly tag: string;

      constructor(title: string, options: NotificationOptions = {}) {
        this.tag = options.tag ?? '';
        probe.raised.push({
          title,
          body: options.body ?? '',
          tag: this.tag,
          silent: options.silent === true,
        });
      }

      close(): void {
        probe.closed.push(this.tag);
        this.onclose?.();
      }
    }
    Object.defineProperty(window, 'Notification', {
      configurable: true,
      writable: true,
      value: StubNotification,
    });
  });
}

function probeOf(page: Page): Promise<NotificationProbe> {
  return page.evaluate(
    () => (window as unknown as { __aoNotifications: NotificationProbe }).__aoNotifications,
  );
}

/** Two scripted turns, each answering and completing so no turn is left open. */
function twoTurnScenario() {
  return {
    version: 1,
    name: 'remote-notification-turns',
    provider: 'claude',
    turns: [1, 2].map((n) => ({
      label: `turn-${n}`,
      steps: [emit([...textLines(`msg-${n}`, `Answer ${n}.`), RESULT_LINE])],
    })),
    afterTurns: 'silent',
  };
}

const lanIP = nonLoopbackIPv4();
const THREAD_TITLE = 'Remote notification thread';

test.describe.serial('notifications on a remote screen', () => {
  // A host with no non-loopback interface genuinely cannot produce the peer
  // this spec is about. A visible skip is the honest outcome; a vacuous pass
  // is not.
  test.skip(
    lanIP === null,
    'no non-loopback IPv4 interface on this host, so no off-host peer can be produced',
  );

  let harness: HarnessApp;
  let couchContext: BrowserContext;
  let couch: Page;
  let surfaced: Surfaced;
  let threadId: string;

  test.beforeAll(async ({ browser }) => {
    harness = await launchHarness();
    const network = await harness.rpc<{ bindAll: boolean }>('SetNetworkSettings', { bindAll: true });
    expect(network.bindAll).toBe(true);

    await harness.rpc('HarnessSetScenario', { scenario: twoTurnScenario() });
    threadId = await seedAgentThread(harness, 'remote-notifications', THREAD_TITLE);
    await startMock(harness, threadId);

    couchContext = await browser.newContext();
    couch = await couchContext.newPage();
    surfaced = await instrument(couch);
    await stubNotifications(couch);

    const invite = await mintInvite(harness, 'full');
    const shown = await redeemOnScreen(couch, invite, 'Couch browser');
    await confirmOnHost(harness, shown);
    await expect(couch.getByTestId('thread-row')).toHaveCount(1, { timeout: PAIRED_APP_MOUNT_MS });

    // The couch screen's OWN quiet-when reading, written by clicking the
    // radio on the shipped settings page — which is what puts it in this
    // device's bucket rather than the desk's. Without it the page would
    // suppress its own notifications correctly (it is focused, and the
    // shipped default is "quiet while focused"), and the assertions below
    // would be about a refusal instead of a presentation.
    await couch.getByTestId('sidebar-settings-button').click();
    await expect(couch.getByRole('tablist', { name: 'Settings Sections' })).toBeVisible();
    await couch.getByRole('tab', { name: 'Notifications', exact: true }).click();
    await couch.getByTestId('quiet-when-option-never').click();
    await expect(
      couch.locator('[data-testid="quiet-when-option-never"] input[type="radio"]'),
    ).toBeChecked();
    await couch.keyboard.press('Escape');
  });

  test.afterAll(async () => {
    await couchContext?.close();
    // Leave the instance as we found it: the bind persists to the settings
    // file and outlives the listener.
    await harness?.rpc('SetNetworkSettings', { bindAll: false }).catch(() => undefined);
    await harness?.close();
  });

  test('a completed turn raises a Web Notification on the remote screen, silently', async () => {
    const sent = harness.waitForEvent<{ id: string; kind: string; retract?: boolean }>(
      'notification:send',
      (data) => !data.retract && data.kind === 'turn-complete',
    );
    await harness.rpc('SendMessage', threadId, 'first question', null);
    const presented = await sent;

    await expect
      .poll(async () => (await probeOf(couch)).raised.length, {
        message: 'the remote page must present the send it received',
      })
      .toBe(1);
    const [raised] = (await probeOf(couch)).raised;

    // Named after the thread, saying only that it is at rest: the redaction
    // line is the same one the host's banner honours, because it is drawn in
    // internal/notify's mapping, before either presenter sees the payload.
    expect(raised.title).toBe(THREAD_TITLE);
    expect(raised.body).toBe('Completed');
    expect(raised.body).not.toContain('Answer 1');

    // EXACTLY ONE SOUND, resolved from THIS screen's settings: the shipped
    // default cue for turn-complete is a built-in, which the page plays
    // itself, so the banner must ask the platform for silence.
    expect(raised.silent).toBe(true);

    // The tag is namespaced by backend, so one machine's notification can
    // never silently replace another's.
    expect(raised.tag.endsWith(`|${presented.id}`)).toBe(true);
  });

  test('the host screen’s cue frame never reaches this page', async () => {
    // The precondition: this page's wire was captured and did carry the
    // notification traffic. Without it, "no sound frame" would be equally
    // true of a page that never connected.
    expect(
      surfaced.eventChannels,
      'the paired page must have received the notification send it presented',
    ).toContain('notification:send');
    // AudienceLoopbackOnly, and this is the only place that policy is
    // observable: the cue was resolved from the DESK's settings and its
    // speakers are in another room.
    expect(surfaced.eventChannels).not.toContain('notification:sound');
  });

  test('resuming the thread withdraws the notification this page raised', async () => {
    const before = (await probeOf(couch)).closed.length;
    const withdrawn = harness.waitForEvent<{ id: string; retract?: boolean }>(
      'notification:send',
      (data) => Boolean(data.retract) && data.id === `thread:${threadId}`,
    );
    await harness.rpc('SendMessage', threadId, 'second question', null);
    await withdrawn;

    // A retraction is gated by nothing on either screen: what it withdraws is
    // already in front of somebody.
    await expect
      .poll(async () => (await probeOf(couch)).closed.length, {
        message: 'the remote page must close the notification it raised',
      })
      .toBe(before + 1);
  });
});
