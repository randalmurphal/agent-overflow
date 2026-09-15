// A paired computer that is off is ordinary state, and the console says so
// once per outage rather than once per attempt, subscriber or list refresh.
// A "+ New" draft pane's synthetic thread id is never routed: with two
// computers attached no entity index can resolve it, and the tray must not
// ask. Covers wsClient's outage logging, computerRows' fan-out skip and the
// ActivityRail draft guard against the real backend proxy.
import { test, expect, type Page } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import type { SeedResult } from './fixtures.js';

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
  test.setTimeout(120_000);
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
      name: 'laptop-project', repo: {}, threads: [{ title: 'Laptop conversation', provider: 'claude',
        turns: [{ userText: 'Remote hello', items: [{ kind: 'assistant_text', summary: 'Remote hi.' }] }] }],
    }] });
    const localRow = page.getByTestId('thread-row').filter({ hasText: 'Local conversation' }).first();
    const laptopRow = page.getByTestId('thread-row').filter({ hasText: 'Laptop conversation' }).first();
    const openLocal = async () => {
      await localRow.click();
      await expect(page.getByTestId('message-timeline-scroll')).toBeVisible();
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
    // nobody, so nothing is refused.
    await page.getByTestId('project-item-new-thread').first().click();
    await expect(page.getByLabel('Message Input')).toBeVisible();
    await poke();
    expect(drain()).toEqual({ preparationFailed: 0, ensureConnected: 0, ownerUnknown: 0, http503: 0, socketFailed: 0, errors: [] });
    await openLocal();
    drain();

    // The laptop goes away. The sidebar dims its thread once the socket
    // is gone; the ladder then keeps dialing through the proxy, which
    // refuses the upgrade while the manifest is cached and answers 503
    // once it is refetched. Ten seconds holds several climbing attempts
    // (250ms doubling), enough to show that only the first refetch is
    // reported.
    await remote.stop();
    await expect(laptopRow).toHaveAttribute('data-machine-unreachable', 'true');
    await page.waitForTimeout(10_000);
    const outage = drain();
    expect(outage.http503).toBeGreaterThanOrEqual(2);
    expect(outage).toMatchObject({ preparationFailed: 1, ensureConnected: 0, ownerUnknown: 0, errors: [] });

    // A reload boots every store against the offline computer: one line
    // for its first failed dial, none per subscriber or per list read.
    await page.reload();
    await openLocal();
    await expect(laptopRow).toHaveAttribute('data-machine-unreachable', 'true');
    await poke();
    const boot = drain();
    expect(boot.http503).toBeGreaterThanOrEqual(1);
    expect(boot).toMatchObject({ preparationFailed: 1, ensureConnected: 0, ownerUnknown: 0, errors: [] });
  } finally {
    await page.close();
    await remote?.close();
    await home?.close();
  }
});
