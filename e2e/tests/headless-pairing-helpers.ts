// The owner console of a separately running harness. The child owns no
// backend: closing SSH/stdin must leave that host alive.
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { expect } from '@playwright/test';
import { spawnContained } from '../src/harness-process.js';
import type { HarnessApp } from '../src/harness.js';
import type { PairingInvite } from './offhost-helpers.js';

export async function headlessPairing(harness: HarnessApp) {
  const child = spawnContained(process.env.AO_HARNESS_BIN ?? fileURLToPath(new URL('../../bin/agent-overflow', import.meta.url)),
    ['pair', '--json', '--class', 'desktop', '--config-root', harness.bootstrap.dataDir],
    { stdio: ['pipe', 'pipe', 'pipe'], memoryLimitBytes: 512 * 1024 * 1024 });
  const lines = createInterface({ input: child.stdout! });
  const iterator = lines[Symbol.asyncIterator]();
  let diagnostic = '';
  child.stderr!.on('data', (chunk: Buffer) => { diagnostic = (diagnostic + chunk.toString()).slice(-4096); });
  const exited = new Promise<number | null>((resolve, reject) => { child.once('exit', resolve); child.once('error', reject); });
  async function record(expected: string): Promise<Record<string, unknown>> {
    const next = await iterator.next();
    if (next.done) throw new Error(`Pairing console closed before ${expected}: ${diagnostic}`);
    const value = JSON.parse(next.value) as { type: string; data?: Record<string, unknown> };
    expect(value.type).toBe(expected);
    return value.data ?? {};
  }
  const close = () => { child.stdin?.end(); lines.close(); if (child.exitCode === null) child.kill('SIGTERM'); };
  try {
    const invite = await record('invitation') as unknown as PairingInvite;
    return {
      invite,
      async confirm(shownOnDevice: string): Promise<void> {
        const status = await record('verification');
        expect(status.verificationNumber).toBe(shownOnDevice);
        child.stdin!.write(shownOnDevice + '\n');
        await record('paired');
        expect(await exited).toBe(0);
      },
      close,
    };
  } catch (error) { close(); throw error; }
}
