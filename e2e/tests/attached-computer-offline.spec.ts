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
// animation through the retries, the computer's rows dim only after thirty
// seconds down, and the Projects menu names its state.
import { test, expect, type Page } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import type { SeedResult } from './fixtures.js';
import { expectProjectsMenuSays, watchProjectsQuiet } from './sidebar-quiet-helpers.js';

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

test('an offline paired computer logs once per outage and a draft pane is never routed', async ({ page }) => {
  test.setTimeout(240_000);
  page.setDefaultTimeout(15_000);
  let home: HarnessApp | undefined;
  let remote: HarnessApp | undefined;
  const drain = collect(page);
  try {
    home = await launchHarness();
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
      await expect(page.getByTestId('thread-title-regenerate')).not.toHaveAttribute('title', 'Not granted to this device');
    };
    // The tray's own refresh trigger for the open thread.
    const poke = async () => {
      await home!.rpc('HarnessEmit', 'provider:background_tasks_changed', { threadId });
      await expect.poll(() => home!.countEvents('provider:background_tasks_changed')).toBeGreaterThan(0);
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
    await poke();
    expect(drain()).toEqual({ preparationFailed: 0, ensureConnected: 0, ownerUnknown: 0, http503: 0, socketFailed: 0, errors: [] });
    await newDraftIn('laptop-project');
    await expect(page.getByTestId('workspace-diff-counts')).toHaveText(/^\+1\s*-1$/);
    await poke();
    expect(drain()).toEqual({ preparationFailed: 0, ensureConnected: 0, ownerUnknown: 0, http503: 0, socketFailed: 0, errors: [] });
    await openLocal();
    drain();

    // The laptop goes away. The ladder keeps dialing through the proxy,
    // which refuses the upgrade while the manifest is cached and answers
    // 503 once it is refetched. Ten seconds holds several climbing attempts
    // (250ms doubling), enough to show that only the first refetch is
    // reported, and that the Projects section stays as it was through
    // them: no status line, nothing animating, and the laptop's row not yet
    // dimmed.
    const outageStart = Date.now();
    await remote.stop();
    const quiet = await watchProjectsQuiet(page, 10_000);
    expect(quiet).toEqual({ samples: expect.any(Number), statusLines: [], animations: [], dimmed: 0 });
    expect(quiet.samples).toBeGreaterThanOrEqual(15);
    const outage = drain();
    expect(outage.http503).toBeGreaterThanOrEqual(2);
    // Each dial is a browser-native console line, so the ladder's cadence
    // is itself user-visible. Six rungs fit in ten seconds; the ceiling
    // catches passive demand collapsing the backoff on every poll, which
    // would put the dials seconds apart for as long as the machine is off.
    expect(outage.http503 + outage.socketFailed).toBeLessThanOrEqual(12);
    expect(outage).toMatchObject({ preparationFailed: 1, ensureConnected: 0, ownerUnknown: 0, errors: [] });

    // Thirty seconds down without a break, the laptop's row dims. The
    // Projects menu is where the laptop's state is said.
    await expect(laptopRow).toHaveAttribute('data-machine-unreachable', 'true', { timeout: 40_000 });
    expect(Date.now() - outageStart).toBeGreaterThanOrEqual(30_000);
    await expectProjectsMenuSays(page, /Offline/);
    expect(await watchProjectsQuiet(page, 1_000)).toMatchObject({ statusLines: [], animations: [] });
    // Its thread, opened now, says the laptop is offline.
    await expectLaptopThreadSaysOffline();
    await openLocal();
    drain();

    // A reload boots every store against the offline computer: one line
    // for its first failed dial, none per subscriber or per list read. The
    // laptop has not connected in this window, so its row dims thirty
    // seconds after the window began watching it, and the section stays
    // quiet meanwhile.
    await page.reload();
    const bootStart = Date.now();
    await openLocal();
    const booted = await watchProjectsQuiet(page, 10_000);
    expect(booted).toMatchObject({ statusLines: [], animations: [], dimmed: 0 });
    await expect(laptopRow).toHaveAttribute('data-machine-unreachable', 'true', { timeout: 40_000 });
    expect(Date.now() - bootStart).toBeGreaterThanOrEqual(25_000);
    await poke();
    const boot = drain();
    expect(boot.http503).toBeGreaterThanOrEqual(1);
    expect(boot).toMatchObject({ preparationFailed: 1, ensureConnected: 0, ownerUnknown: 0, errors: [] });

    // The same after the reload, where this window has never heard the
    // laptop's grant: still offline, never "read-only", and a control that
    // grant switches off says the laptop has to connect.
    await expectLaptopThreadSaysOffline();
    const regenerate = page.getByTestId('thread-title-regenerate');
    await expect(regenerate).toBeDisabled();
    await expect(regenerate).toHaveAttribute('title', 'Unavailable until this computer connects');
  } finally {
    await page.close();
    await remote?.close();
    await home?.close();
  }
});
