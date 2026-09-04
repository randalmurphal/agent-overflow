// Thread state that CHANGES on the host, arriving on a paired device that
// is looking at the same thread — with no reload, no poll, and no click.
//
// WHY THIS FILE EXISTS. The push half of the wire was withheld from every
// off-host client for nineteen channels of thread and workspace state. The
// pull half was not: a paired phone could call `RegisterQueueItem`,
// `RespondToApproval`, `GetGitStatus` and `OpenTerminal` and be answered,
// because wave 6d2 replaced the per-method local-only table with the
// per-call scope gate. Only the CHANNEL rows were left behind, each one
// justifying `loopback-only` by citing the reachability of an RPC that had
// already stopped being local — so the phone drove the thread and then
// watched a screen that never changed under it: a queue row that stayed
// stale, an approval prompt that never appeared, a terminal with no output.
// Re-adjudicated by user ruling 2026-09-03: every event about a thread or a
// workspace must reach, in real time, any connected client that has
// visibility of it.
//
// A unit test can pin the policy row (`internal/transport`'s
// TestLoopbackOnlyIsForHostDirectivesOnly does, by name, in both
// directions). What it cannot show is that the frame survives the whole
// path — audience filter, per-connection scope gate, event pump, the
// client's own subscription, the store, the pane — to become something a
// person sees. That path is what this file walks, and it walks it for the
// two moments that made the defect user-visible.
//
// WHAT IS REAL HERE. The peer is a real second browser context on a real
// non-loopback address, paired through the shipped pairing screen with the
// shipped ceremony (`offhost-helpers.ts`, shared with the lifecycle and
// preview-gateway specs). The queue item is registered by an RPC on the
// host, exactly as the desk's own composer would. The approval is raised by
// the mock provider over the real `control_request/can_use_tool` wire and
// answered from the PHONE, which is spec §9's ruling in its strongest form:
// the phone is the full app, and an approval is answerable from it.
//
// WHY IT OWNS ITS BACKEND: the LAN-bind preference persists to the settings
// file and rebinds the listener, and `harness.reset()` undoes neither, so
// borrowing the worker fixture's instance would hand the next spec a
// LAN-bound backend. Both cases are one pairing on one device row, so they
// run `.serial` and carry it forward rather than re-staging a ceremony.

import { expect, test, type BrowserContext, type Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  RESULT_LINE,
  claudeScenario,
  emit,
  startMock,
  textLines,
  toolResultLine,
  toolUseLine,
} from './agent-visibility-helpers.js';
import {
  PAIRED_APP_MOUNT_MS,
  confirmOnHost,
  instrument,
  mintInvite,
  nonLoopbackIPv4,
  redeemOnScreen,
  type Surfaced,
} from './offhost-helpers.js';

interface SeedResult {
  projects: Array<{ projectId: string; path: string; threadIds: string[] }>;
}

const lanIP = nonLoopbackIPv4();

const PHONE_LABEL = 'Couch phone';
const QUEUE_THREAD = 'Queued from the desk';
const APPROVAL_THREAD = 'Approved from the couch';
const QUEUED_TEXT = 'and then run the whole suite';
const WRITE_PATH = 'note.txt';

test.describe.serial('off-host live thread state', () => {
  // Not green-washed: a host with no non-loopback interface genuinely
  // cannot produce the peer this spec is about, and saying so is the
  // honest outcome. A skip is visible in the report; a vacuous pass is not.
  test.skip(
    lanIP === null,
    'no non-loopback IPv4 interface on this host, so no off-host peer can be produced',
  );

  let harness: HarnessApp;
  let phoneContext: BrowserContext;
  let phone: Page;
  let surfaced: Surfaced;
  let queueThreadId = '';
  let approvalThreadId = '';

  test.beforeAll(async ({ browser }) => {
    harness = await launchHarness();

    const network = await harness.rpc<{ bindAll: boolean }>('SetNetworkSettings', {
      bindAll: true,
    });
    expect(network.bindAll).toBe(true);

    // Two threads, because the cases must not share one: the queued item
    // in the first is deliberately never flushed (nothing starts a session
    // on that thread), and starting one for the approval case would drain
    // it mid-assertion. Both carry a seeded turn, since a draft thread is
    // hidden from the sidebar and the phone reaches a thread by clicking
    // its row. The repo is real because the approval names a file in it.
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        {
          name: 'offhost-live-state',
          repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
          threads: [
            {
              title: QUEUE_THREAD,
              provider: 'claude',
              turns: [{ userText: 'first', items: [{ kind: 'assistant_text', summary: 'one' }] }],
            },
            {
              title: APPROVAL_THREAD,
              provider: 'claude',
              turns: [{ userText: 'second', items: [{ kind: 'assistant_text', summary: 'two' }] }],
            },
          ],
        },
      ],
    });
    [queueThreadId, approvalThreadId] = seed.projects[0].threadIds;
    expect(queueThreadId, 'the fixture must seed two visible threads').toBeTruthy();
    expect(approvalThreadId, 'the fixture must seed two visible threads').toBeTruthy();

    phoneContext = await browser.newContext();
    phone = await phoneContext.newPage();
    surfaced = await instrument(phone);

    // Full access, because both channels under test are gated on grants a
    // view-only device does not hold (`threads:operate` for the queue,
    // `approvals:respond` for the approval) — and the scope column, not the
    // audience column, is what a narrower device is refused by.
    const invite = await mintInvite(harness, 'full');
    const shown = await redeemOnScreen(phone, invite, PHONE_LABEL);
    await confirmOnHost(harness, shown);
    await expect(phone.getByTestId('thread-row')).toHaveCount(2, {
      timeout: PAIRED_APP_MOUNT_MS,
    });
  });

  test.afterAll(async () => {
    await phoneContext?.close();
    // Leave the instance as we found it: the settings file outlives the
    // listener, and a future reader of this data dir should not find a LAN
    // bind nobody asked for.
    await harness?.rpc('SetNetworkSettings', { bindAll: false }).catch(() => undefined);
    await harness?.close();
  });

  // -------------------------------------------------------------------
  // provider:queue_state_changed — `threads:operate`, the grant
  // GetQueueState takes for the same snapshot.
  // -------------------------------------------------------------------
  test("a queue item registered on the host appears above the paired page's composer, live", async () => {
    await phone.getByTestId('thread-row').filter({ hasText: QUEUE_THREAD }).click();
    // The pane is mounted and this thread is the one open: the preview is a
    // control of the open composer, so asserting an absence before the
    // composer exists would assert nothing.
    await expect(phone.getByLabel('Message Input')).toBeEnabled();
    await expect(phone.getByTestId('send-queue-preview')).toHaveCount(0);
    // A paired device is never `host`, whatever address it arrived from:
    // open-in-editor names an editor on the host's screen, which this
    // device has no use for (the owner's phone over `adb reverse` — a
    // loopback peer — was offered it, 2026-09-04).
    await expect(phone.getByTestId('chat-header-open-editor')).toHaveCount(0);

    // The host queues a message the way its own composer does. Nothing is
    // sent to this page and nothing on it is clicked from here on, so the
    // row below can only arrive as a push.
    await harness.rpc('RegisterQueueItem', queueThreadId, QUEUED_TEXT, {});

    // The wire first, because it is the half this spec exists for and the
    // half a screen assertion cannot name: a channel withheld by its
    // audience row reaches a paired device as an absence, and
    // `Surfaced.eventChannels` is the one place that absence is legible.
    await expect
      .poll(() => surfaced.eventChannels, {
        message: 'the queue snapshot must reach a paired device as a pushed frame',
      })
      .toContain('provider:queue_state_changed');

    // And then the screen: live, not merely eventually, because the page
    // was never reloaded and never re-read the thread.
    const row = phone.getByTestId('send-queue-preview-row');
    await expect(row).toHaveCount(1);
    await expect(row).toHaveAttribute('data-state', 'queued');
    await expect(row).toContainText(QUEUED_TEXT);

    // And what it is showing is the backend's state rather than a local
    // echo: the authoritative snapshot holds exactly this one item.
    const queued = await harness.rpc<Array<{ message: string }>>('GetQueueState', queueThreadId);
    expect(queued.map((item) => item.message)).toEqual([QUEUED_TEXT]);
  });

  // -------------------------------------------------------------------
  // provider:approval — `approvals:respond`, the grant RespondToApproval
  // takes to answer the prompt this frame is the ask for. The pair is the
  // point: withholding the push while admitting the answer is a phone
  // that can approve nothing, because it is never asked.
  // -------------------------------------------------------------------
  test('an approval the provider raises reaches the paired page live, and the paired page answers it', async () => {
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeScenario('offhost-approval', [
        emit([
          ...textLines('msg-lead', 'Writing the note.'),
          toolUseLine('msg-write', 'tu-write', 'Write', {
            file_path: WRITE_PATH,
            content: 'hello',
          }),
        ]),
        {
          approval: {
            toolName: 'Write',
            input: { file_path: WRITE_PATH, content: 'hello' },
            toolUseId: 'tu-write',
            onAllow: [
              emit([
                toolResultLine('tu-write', 'File created successfully.'),
                ...textLines('msg-final', 'Wrote the note.'),
                RESULT_LINE,
              ]),
            ],
            onDeny: [emit([RESULT_LINE])],
          },
        },
      ]),
    });

    // The phone opens the thread BEFORE the turn starts. A pending approval
    // is also readable from GetThreadLiveState on attach, so a page that
    // opened the thread afterwards would prove the PULL path and say
    // nothing about the push one.
    await phone.getByTestId('thread-row').filter({ hasText: APPROVAL_THREAD }).click();
    await expect(phone.getByLabel('Message Input')).toBeEnabled();
    await expect(phone.getByTestId('composer-pending-approval')).toHaveCount(0);

    await startMock(harness, approvalThreadId);
    await harness.rpc('SendMessage', approvalThreadId, 'write the note', null);

    // The provider is parked on the control request. Everything after this
    // barrier is about who learns of it.
    await harness.waitForEvent(
      'harness:mock',
      (ev: any) => ev.report.kind === 'approval_pending',
    );

    await expect(phone.getByTestId('composer-pending-approval')).toBeVisible();
    expect(
      surfaced.eventChannels,
      'the ask must reach the device that is allowed to answer it',
    ).toContain('provider:approval');

    // Answered from the couch, which is the ruling (spec §9): the click is
    // the shipped control, the RPC behind it is `approvals:respond`, and the
    // provider's own report is what confirms the decision crossed back.
    await phone.getByTestId('approval-allow').click();
    await harness.waitForEvent(
      'harness:mock',
      (ev: any) => ev.report.kind === 'approval_decided' && ev.report.detail === 'allow',
    );

    // The resolution is a push too, on the same channel, so the prompt
    // clears on the phone without it asking.
    await expect(phone.getByTestId('composer-pending-approval')).toHaveCount(0);
    await harness.waitForEvent('provider:turn_completed');

    // Nothing was surfaced for a flow that worked, on either channel: a
    // widened audience that also delivers a frame the client cannot handle
    // would show up here as a console error rather than a missing row.
    expect(surfaced.errorToasts, 'a working live sync must surface no error toast').toEqual([]);
    expect(surfaced.consoleErrors, 'a working live sync must log no console error').toEqual([]);
  });
});
