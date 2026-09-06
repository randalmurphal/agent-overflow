import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import type { Page } from '@playwright/test';
import { spawnContained } from '../src/harness-process.js';

/** Owns only a frontend controller. Both execution hosts are independent
 * harness processes, so stopping either cannot stop this screen. */
export async function launchFrontendClient(profiles: string, config: string, computer: string, port = 0) {
  const child = spawnContained(fileURLToPath(new URL('../../bin/ao-frontendclient-test', import.meta.url)), [], {
    stdio: ['pipe', 'pipe', 'pipe'], memoryLimitBytes: 512 * 1024 * 1024,
    env: { ...process.env, AO_TEST_FRONTEND_FIXTURE: '1', AO_TEST_FRONTEND_PROFILES: profiles,
      AO_TEST_FRONTEND_CONFIG: config, AO_TEST_FRONTEND_COMPUTER: computer, AO_TEST_FRONTEND_PORT: String(port),
      AO_TEST_FRONTEND_ASSETS: fileURLToPath(new URL('../../frontend/dist', import.meta.url)) },
  });
  const lines = createInterface({ input: child.stdout! });
  const iterator = lines[Symbol.asyncIterator]();
  let diagnostic = '';
  child.stderr!.on('data', (chunk: Buffer) => { diagnostic = (diagnostic + chunk.toString()).slice(-4096); });
  const exited = new Promise<void>((resolve, reject) => { child.once('exit', () => resolve()); child.once('error', reject); });
  const close = async () => {
    child.stdin?.end(); lines.close();
    if (child.exitCode !== null || child.signalCode !== null) return;
    child.kill('SIGTERM');
    const kill = setTimeout(() => child.kill('SIGKILL'), 5000);
    try { await exited; } finally { clearTimeout(kill); }
  };
  try {
    let timeout: ReturnType<typeof setTimeout> | undefined;
    const next = await Promise.race([
      iterator.next(),
      new Promise<never>((_, reject) => {
        timeout = setTimeout(() => reject(new Error(`Frontend startup timed out: ${diagnostic}`)), 10_000);
      }),
      exited.then(() => { throw new Error(`Frontend exited during startup: ${diagnostic}`); }),
    ]).finally(() => clearTimeout(timeout));
    if (next.done) throw new Error(`Frontend closed before startup: ${diagnostic}`);
    const { origin, token } = JSON.parse(next.value) as { origin: string; token: string };
    return {
      origin,
      async open(page: Page): Promise<void> {
        const response = await fetch(`${origin}/pageurl`, { headers: { Authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(10_000) });
        if (!response.ok) throw new Error(`Frontend page ticket failed: ${response.status}`);
        await page.goto((await response.text()).trim());
      },
      close,
    };
  } catch (error) { await close(); throw error; }
}
