// A paired computer that is off is ordinary state, and the console says so
// once per outage rather than once per attempt, subscriber or list refresh.
// A "+ New" draft pane's synthetic thread id is never routed: with two
// computers attached no entity index can resolve it, and the tray must not
// ask. Its checkout still belongs to its project's computer, so a draft in
// the laptop's project shows the laptop's git status. Covers wsClient's
// outage logging, computerRows' fan-out skip, the ActivityRail draft guard
// and the draft's workspace key against the real backend proxy, and the
// dial cadence itself: the browser logs every refused request, so the
// ladder's rungs are what bounds the console during an outage. The screen
// is as quiet: the closed Projects section gains no status line and runs no
// animation through the retries, the computer's rows dim only once it has
// been down the settle time (thirty seconds in the product, shortened
// here), and the Projects menu names its state.
import { type Locator, type Page } from '@playwright/test';
import { test, expect } from './base-test.js';
import { harnessTiming, launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import type { SeedResult } from './fixtures.js';
import { expectProjectsMenuSays, watchProjectsQuiet } from './sidebar-quiet-helpers.js';
import { methodNameById, PROBE_FIELD, trackPageCalls } from './offhost-helpers.js';

// The unreachable settle (UNREACHABLE_SETTLE_MS). Ten seconds keeps the
// window the dial ceiling below was sized for: the outage is watched from
// the drop until the row dims.
const SETTLE_MS = 10_000;

interface Entry { type: string; text: string }

interface Counts { preparationFailed: number; ensureConnected: number; ownerUnknown: number; http503: number; socketFailed: number; errors: string[] }

function collect(page: Page): () => Counts {
  const entries: Entry[] = [];
  page.on('console', (msg) => entries.push({ type: msg.type(), text: msg.text() }));
  page.on('pageerror', (err) => entries.push({ type: 'pageerror', text: err.message }));
  return () => {
    const counts: Counts = { preparationFailed: 0, ensureConnected: 0, ownerUnknown: 0, http503: 0, socketFailed: 0, errors: [] };
    for (const entry of entries.splice(0, entries.length)) {
      if (entry.text.includes('connection preparation failed')) counts.preparationFailed++;
      else if (entry.text.includes('ensureConnected failed')) counts.ensureConnected++;
      else if (entry.text.includes('owns this item is unknown')) counts.ownerUnknown++;
      // The browser's own lines, one per dial: a refused upgrade while the
      // manifest is still cached, then the proxy's 503 once it is refetched.
      else if (/status of 503/.test(entry.text)) counts.http503++;
      else if (/^WebSocket connection to .* failed/.test(entry.text)) counts.socketFailed++;
      else if (entry.type === 'error' || entry.type === 'pageerror') counts.errors.push(entry.text.slice(0, 200));
    }
    return counts;
  };
}

// The open pane's refresh trigger, and the workspace read it refreshes with.
const REFRESH = { trigger: 'provider:background_tasks_changed', method: 'GetWorkspaceActivity' };

/**
 * Every call the page sends that names a draft placeholder (an id starting
 * `draft:`, frontend draftPlaceholderId.ts) as its first argument, by method:
 * a placeholder has no thread row anywhere, so no read may carry one.
 * Installed before the page opens, so it sees every socket.
 */
function placeholderCalls(page: Page): () => string[] {
  const asked: string[] = [];
  page.on('websocket', (socket) => {
    socket.on('framesent', ({ payload }) => {
      if (typeof payload !== 'string' || !payload.startsWith('{"type":"rpc"')) return;
      const sent = JSON.parse(payload) as { methodId?: number; params?: unknown[] };
      const first = sent.params?.[0];
      if (typeof first === 'string' && first.startsWith('draft:')) asked.push(methodNameById(sent.methodId ?? -1));
    });
  });
  return () => asked.splice(0);
}

test('an offline paired computer logs once per outage and a draft pane is never routed', async ({ page }) => {
  test.setTimeout(240_000);
  page.setDefaultTimeout(15_000);
  let home: HarnessApp | undefined;
  let remote: HarnessApp | undefined;
  const drain = collect(page);
  const calls = await trackPageCalls(page, { refresh: REFRESH });
  const draftAsked = placeholderCalls(page);
  try {
    home = await launchHarness({ env: harnessTiming({ 'unreachable-settle': `${SETTLE_MS}ms` }) });
    remote = await launchHarness();
    const pairing = await headlessPairing(remote);
    try {
      const attachment = await home.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
      await pairing.confirm(attachment.verificationNumber);
    } finally { pairing.close(); }
    const seed = await home.rpc<SeedResult>('HarnessSeed', { projects: [{
      name: 'local-project', repo: {}, threads: [{ title: 'Local conversation', provider: 'claude',
        turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi there.' }] }] }],
    }] });
    const threadId = seed.projects[0].threadIds[0];
    await remote.rpc<SeedResult>('HarnessSeed', { projects: [{
      name: 'laptop-project',
      repo: { commits: [{ files: { 'notes.txt': 'one\n' } }], dirty: { 'notes.txt': 'two\n' } }, threads: [{ title: 'Laptop conversation', provider: 'claude',
        turns: [{ userText: 'Remote hello', items: [{ kind: 'assistant_text', summary: 'Remote hi.' }] }] }],
    }] });
    const localRow = page.getByTestId('thread-row').filter({ hasText: 'Local conversation' }).first();
    const laptopRow = page.getByTestId('thread-row').filter({ hasText: 'Laptop conversation' }).first();
    // The Regenerate title row of the open thread's header menu, read while
    // the menu is open. Opened from the keyboard: the offline banner lies
    // over the header's controls.
    const withRegenerateRow = async (check: (row: Locator) => Promise<void>) => {
      await page.getByTestId('chat-header-more').focus();
      await page.keyboard.press('Enter');
      await check(page.locator('[data-popover]').getByRole('menuitem', { name: 'Regenerate title' }));
      await page.keyboard.press('Escape');
      await expect(page.locator('[data-popover]')).toHaveCount(0);
    };
    const openLocal = async () => {
      await localRow.click();
      await expect(page.getByTestId('message-timeline-scroll')).toBeVisible();
    };
    // A thread on the laptop, opened once the drop has lasted the settle
    // time: the banner says the laptop is offline, with no countdown, and
    // so does the composer, which takes no message.
    const expectLaptopThreadSaysOffline = async () => {
      await laptopRow.click();
      const banner = page.getByTestId('transport-status-banner');
      await expect(banner).toContainText('Offline');
      await expect(banner).not.toContainText('Reconnecting in');
      const input = page.getByLabel('Message Input');
      await expect(input).toBeDisabled();
      await expect(input).toHaveAttribute('placeholder', / is offline$/);
      // No control on the thread says the laptop refused this device.
      await withRegenerateRow(async (row) => {
        await expect(row).toHaveCount(1);
        await expect(row).not.toHaveAttribute('title', 'Not granted to this device');
      });
    };
    // The open pane's refresh trigger, done once the page has read the
    // workspace again after receiving this trigger, so whatever that refresh
    // logs is in before the console is drained. Each trigger carries a
    // marker of its own, so a read the page made for another reason or an
    // earlier trigger does not stand in for this one.
    let pokes = 0;
    const poke = async () => {
      const probe = `refresh-${++pokes}`;
      await home!.rpc('HarnessEmit', REFRESH.trigger, { threadId, [PROBE_FIELD]: probe });
      await expect.poll(() => calls.refreshedAfter(probe), { message: `the page did not refresh the open pane on trigger ${probe}` }).toBe(true);
    };
    // A draft pane reads nothing on that trigger, so its reads are done once
    // every call the page made opening it is answered: a call that routed
    // its placeholder id would have failed, and logged, by then.
    const draftSettled = async () => {
      await calls.drain('the draft pane\'s reads did not finish');
      expect(draftAsked(), 'calls the page made for a draft placeholder').toEqual([]);
    };

    await home.open(page);
    await expect(laptopRow).toBeVisible();
    await openLocal();
    await poke();
    expect(drain()).toEqual({ preparationFailed: 0, ensureConnected: 0, ownerUnknown: 0, http503: 0, socketFailed: 0, errors: [] });

    // "+ New" with two computers attached: the placeholder id is asked of
    // nobody, so nothing is refused. The laptop's draft reads its dirty
    // checkout from the laptop.
    const newDraftIn = (project: string) => page.getByTestId('project-item')
      .filter({ hasText: project }).getByTestId('project-item-new-thread').click();
    await newDraftIn('local-project');
    await expect(page.getByLabel('Message Input')).toBeVisible();
    await draftSettled();
    expect(drain()).toEqual({ preparationFailed: 0, ensureConnected: 0, ownerUnknown: 0, http503: 0, socketFailed: 0, errors: [] });
    await newDraftIn('laptop-project');
    await expect(page.getByTestId('workspace-diff-counts')).toHaveText(/^\+1\s*-1$/);
    await draftSettled();
    expect(drain()).toEqual({ preparationFailed: 0, ensureConnected: 0, ownerUnknown: 0, http503: 0, socketFailed: 0, errors: [] });
    await openLocal();
    drain();

    // The laptop goes away. The ladder keeps dialing through the proxy,
    // which refuses the upgrade while the manifest is cached and answers
    // 503 once it is refetched. The settle time holds several climbing
    // attempts (250ms doubling), enough to show that only the first refetch
    // is reported, and that the Projects section stays as it was through
    // them: no status line and nothing animating, until the laptop's row
    // dims, the settle time down without a break, and after, through a
    // refresh of the open thread.
    const outageStart = Date.now();
    let dimmedAfter = 0;
    await remote.stop();
    const quiet = await watchProjectsQuiet(page, async () => {
      await expect(laptopRow).toHaveAttribute('data-machine-unreachable', 'true', { timeout: SETTLE_MS + 10_000 });
      dimmedAfter = Date.now() - outageStart;
      await poke();
    });
    // The watch saw the row dim, so it was sampling the section's changes.
    expect(quiet).toEqual({ samples: expect.any(Number), statusLines: [], animations: [], dimmed: 1 });
    expect(dimmedAfter, 'the row dimmed before the settle time').toBeGreaterThanOrEqual(SETTLE_MS);
    const outage = drain();
    expect(outage.http503).toBeGreaterThanOrEqual(2);
    // Each dial is a browser-native console line, so the ladder's cadence
    // is itself user-visible. Six rungs fit in ten seconds; the ceiling
    // catches passive demand collapsing the backoff on every poll, which
    // would put the dials seconds apart for as long as the machine is off.
    expect(outage.http503 + outage.socketFailed).toBeLessThanOrEqual(12);
    expect(outage).toMatchObject({ preparationFailed: 1, ensureConnected: 0, ownerUnknown: 0, errors: [] });

    // The Projects menu is where the laptop's state is said.
    await expectProjectsMenuSays(page, /Offline/);
    // Its thread, opened now, says the laptop is offline.
    await expectLaptopThreadSaysOffline();
    await openLocal();
    drain();

    // A reload boots every store against the offline computer: one line
    // for its first failed dial, none per subscriber or per list read. The
    // laptop has not connected in this window, so its row dims the settle
    // time after the window began watching it, and the section stays quiet
    // meanwhile. The window starts watching while the reload loads, before
    // the clock below starts.
    await page.reload();
    const bootStart = Date.now();
    await openLocal();
    const booted = await watchProjectsQuiet(page, async () => {
      await expect(laptopRow).toHaveAttribute('data-machine-unreachable', 'true', { timeout: SETTLE_MS + 10_000 });
      expect(Date.now() - bootStart, 'the row dimmed well before the settle time').toBeGreaterThanOrEqual(SETTLE_MS - 5_000);
      await poke();
    });
    expect(booted).toMatchObject({ statusLines: [], animations: [], dimmed: 1 });
    const boot = drain();
    expect(boot.http503).toBeGreaterThanOrEqual(1);
    expect(boot).toMatchObject({ preparationFailed: 1, ensureConnected: 0, ownerUnknown: 0, errors: [] });

    // The same after the reload, where this window has never heard the
    // laptop's grant: still offline, never "read-only", and a control that
    // grant switches off says the laptop has to connect.
    await expectLaptopThreadSaysOffline();
    await withRegenerateRow(async (row) => {
      await expect(row).toBeDisabled();
      await expect(row).toHaveAttribute('title', 'Unavailable until this computer connects');
    });
  } finally {
    await page.close();
    await remote?.close();
    await home?.close();
  }
});
