// The sidebar's status for a thread on another computer, read by a page with
// no pane on that thread. The page learns of the thread's queued message from
// wildcard queue frames, but never of the echo that consumes it (item events
// reach watching clients only), so the running state between a queued
// message and its echo has to come from provider:sends_pending. Once the
// turns end, the row reads idle, as it does on the computer that ran them.
//
// While the computer is unreachable its row keeps the last status it
// reported, shown still and labelled as such, and the reconnect snapshot
// corrects it: the frames that would otherwise correct it are withheld.
import { expect, test, type WebSocketRoute } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import {
  RESULT_LINE, advance, claudeTurnsScenario, emit, seedAgentThread, startMock, textLines, waitForGate,
} from './agent-visibility-helpers.js';
import type { SeedResult } from './fixtures.js';

const QUEUED = 'Queued on the laptop: also update the changelog.';

test('a remote thread with no pane here settles to idle after its queued message, and holds still while unreachable', async ({ page }) => {
  test.setTimeout(180_000);
  page.setDefaultTimeout(15_000);
  let home: HarnessApp | undefined;
  let remote: HarnessApp | undefined;
  let remoteOnline = true;
  let withholdLive = false;
  let withheld = 0;
  let remoteSocket: { page: WebSocketRoute; server: WebSocketRoute } | undefined;
  // Live status frames the page must not need once it reconnects.
  const live = new Set(['provider:turn_completed', 'provider:sends_pending']);
  const keep = (event: { channel?: string }) => {
    if (!withholdLive || !live.has(event.channel ?? '')) return true;
    withheld += 1;
    return false;
  };
  await page.routeWebSocket(/\/ws\/backend\//, (socket) => {
    if (!remoteOnline) { void socket.close({ code: 1012 }); return; }
    const server = socket.connectToServer();
    remoteSocket = { page: socket, server };
    socket.onMessage((message) => server.send(message));
    server.onMessage((message) => {
      if (typeof message !== 'string') { socket.send(message); return; }
      const frame = JSON.parse(message) as { type?: string; channel?: string; events?: { channel?: string }[] };
      if (frame.type === 'event') {
        if (keep(frame)) socket.send(message);
      } else if (frame.type === 'batch' && frame.events) {
        const events = frame.events.filter(keep);
        if (events.length === frame.events.length) socket.send(message);
        else if (events.length > 0) socket.send(JSON.stringify({ ...frame, events }));
      } else {
        socket.send(message);
      }
    });
  });
  try {
    home = await launchHarness();
    remote = await launchHarness();
    const pairing = await headlessPairing(remote);
    try {
      const attachment = await home.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
      await pairing.confirm(attachment.verificationNumber);
    } finally { pairing.close(); }

    await home.rpc<SeedResult>('HarnessSeed', { projects: [{
      name: 'mac-project', repo: {}, threads: [{ title: 'Mac conversation', provider: 'claude',
        turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi there.' }] }] }],
    }] });
    await remote.rpc('HarnessSetScenario', {
      scenario: claudeTurnsScenario('remote-sidebar-status', [
        [
          emit(textLines('msg-working', 'Checking the tree first.')),
          { waitSignal: { name: 'finish' } },
          emit([...textLines('msg-final', 'The tree is clean.'), RESULT_LINE]),
        ],
        [
          { waitSignal: { name: 'reply' } },
          emit([...textLines('msg-reply', 'Changelog updated.'), RESULT_LINE]),
        ],
        [
          { waitSignal: { name: 'later' } },
          emit([...textLines('msg-later', 'Finished while unreachable.'), RESULT_LINE]),
        ],
      ], { holdQueuedInput: true }),
    });
    const threadId = await seedAgentThread(remote, 'laptop-project', 'Laptop conversation');

    // The page watches a thread of its own; the laptop's is a sidebar row.
    await home.open(page);
    await page.getByTestId('thread-row').filter({ hasText: 'Mac conversation' }).first().click();
    await expect(page.getByTestId('message-timeline-scroll')).toBeVisible();
    const laptopRow = page.getByTestId('thread-row').filter({ hasText: 'Laptop conversation' }).first();
    await expect(laptopRow).toBeVisible();
    const dot = laptopRow.getByTestId('thread-row-status-dot');

    // Turn 1 runs on the laptop; a message is queued behind it, flushes at
    // once and is consumed at the turn boundary, where turn 2 runs.
    const mockId = await startMock(remote, threadId);
    await remote.rpc('SendMessage', threadId, 'Check the tree.', null);
    await waitForGate(remote, 'finish');
    await expect(dot).toHaveAttribute('data-status', 'running');
    const flushed = remote.waitForEvent('provider:queue_flushed', (ev: any) => ev.threadId === threadId);
    await remote.rpc('RegisterQueueItem', threadId, QUEUED, {});
    await flushed;
    await advance(remote, mockId, 'finish');
    await waitForGate(remote, 'reply');
    await expect(dot).toHaveAttribute('data-status', 'running');
    await advance(remote, mockId, 'reply');
    await expect.poll(async () => (await remote!.rpc<{ activeTurn?: unknown; flushedItems?: unknown[]; queueItems?: unknown[] }>(
      'GetThreadLiveState', threadId,
    ))).toMatchObject({ flushedItems: [], queueItems: [] });
    await expect.poll(async () => (await remote!.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', threadId)).activeTurn).toBeFalsy();
    // The laptop's own answer: nothing running, nothing pending.
    await expect(dot).toHaveAttribute('data-status', 'idle');

    // Turn 3 is running when the laptop drops off this page's view.
    await remote.rpc('SendMessage', threadId, 'One more thing.', null);
    await waitForGate(remote, 'later');
    await expect(dot).toHaveAttribute('data-status', 'running');
    await expect(dot).toHaveClass(/animate-pulse/);
    remoteOnline = false;
    await remoteSocket!.page.close({ code: 1012, reason: 'test network outage' });
    await remoteSocket!.server.close();
    await expect(laptopRow).toHaveAttribute('data-machine-unreachable', 'true');
    // The last report, shown still and labelled; its place in the list is
    // the one it had.
    await expect(dot).toHaveAttribute('data-status', 'running');
    await expect(dot).not.toHaveClass(/animate-pulse/);
    await expect(dot).toHaveAttribute('title', /^Last known: Working\. .+ is unreachable$/);

    // The turn ends while the page cannot hear it. The reconnect's snapshot
    // corrects the row with the replayed completion withheld.
    await advance(remote, mockId, 'later');
    await expect.poll(async () => (await remote!.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', threadId)).activeTurn).toBeFalsy();
    withholdLive = true;
    remoteOnline = true;
    await expect(laptopRow).not.toHaveAttribute('data-machine-unreachable', 'true', { timeout: 30_000 });
    await expect(dot).toHaveAttribute('data-status', 'idle');
    await expect(dot).not.toHaveAttribute('title', /Last known/);
    expect(withheld).toBeGreaterThan(0);
  } finally {
    await page.close();
    await remote?.close();
    await home?.close();
  }
});
