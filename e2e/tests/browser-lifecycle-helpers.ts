import { chromium, type Browser } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { captureProcessIdentity, captureProcessTreeProof, spawnContained, terminateChildTreeAndWaitVerified, type ProcessIdentity } from '../src/harness-process.js';
import { FALLBACK_MEMORY_LIMIT_BYTES } from '../src/harness-watchdog.js';

/** A fresh browser whose real visibility events are not overridden by automation. */
export async function launchLifecycleBrowser(proxy: string) {
  const profile = await mkdtemp(join(tmpdir(), 'ao-browser-lifecycle-'));
  const child = spawnContained(chromium.executablePath(), [
    '--headless=new', '--no-sandbox', '--disable-dev-shm-usage',
    '--remote-debugging-port=0', `--user-data-dir=${profile}`, `--proxy-server=${proxy}`, 'about:blank',
  ], { detached: true, stdio: ['ignore', 'ignore', 'pipe'], memoryLimitBytes: FALLBACK_MEMORY_LIMIT_BYTES });
  const lines = createInterface({ input: child.stderr! });
  const logs: string[] = [];
  let identity: ProcessIdentity | undefined;
  let browser: Browser | undefined;
  const close = async () => {
    const errors: unknown[] = [];
    try { await browser?.close(); } catch (error) { errors.push(error); }
    try {
      const tree = identity && await captureProcessTreeProof(identity);
      await terminateChildTreeAndWaitVerified(child, identity, 'SIGTERM', undefined, tree || undefined);
    } catch (error) { errors.push(error); }
    lines.close();
    try { await rm(profile, { recursive: true, force: true }); } catch (error) { errors.push(error); }
    if (errors.length) throw new AggregateError(errors, 'Lifecycle browser cleanup failed');
  };
  try {
    const endpoint = new Promise<string>((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error(`Chromium did not publish its endpoint: ${logs.join('\n')}`)), 10_000);
      child.once('error', error => { clearTimeout(timeout); reject(error); });
      child.once('exit', () => { clearTimeout(timeout); reject(new Error(`Chromium exited: ${logs.join('\n')}`)); });
      lines.on('line', (line: string) => {
        logs.push(line);
        if (logs.length > 20) logs.shift();
        const match = /DevTools listening on (ws:\/\/127\.0\.0\.1:\d+\/.*)/.exec(line);
        if (match) { clearTimeout(timeout); resolve(match[1]); }
      });
    });
    const [url] = await Promise.all([endpoint, captureProcessIdentity(child.pid).then(proof => { identity = proof; })]);
    // Only the existing default context retains noDefaults. newContext() would
    // re-enable focus emulation and suppress the events this test must observe.
    browser = await chromium.connectOverCDP(url, { noDefaults: true });
    const context = browser.contexts()[0];
    const page = context.pages()[0];
    const cdp = await context.newCDPSession(page);
    await cdp.send('Security.setIgnoreCertificateErrors', { ignore: true });
    await page.setViewportSize({ width: 390, height: 844 });
    await cdp.send('Emulation.setTouchEmulationEnabled', { enabled: true });
    return { context, page, close };
  } catch (error) {
    try { await close(); } catch (cleanup) { throw new AggregateError([error, cleanup], 'Lifecycle browser failed'); }
    throw error;
  }
}
