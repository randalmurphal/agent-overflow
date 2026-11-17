// The owner console of a separately running harness. The child owns no
// backend: closing SSH/stdin must leave that host alive.
import { createInterface } from 'node:readline';
import { expect, test } from '@playwright/test';
import { productBinary, spawnProduct } from '../src/product-process.js';
import { launchStep, processDiagnostics } from '../src/harness-launch-step.js';
import type { HarnessApp } from '../src/harness.js';
import type { PairingInvite } from './offhost-helpers.js';

/**
 * How long either computer may take to show the other after `confirm`.
 * Each side reaches the other through its device client's credential
 * exchanges (the activation probe, the reciprocal pairing's renewal), and
 * one exchange that stalls holds the next for the client's request timeout
 * (deviceclient pinTimeout, 10s), so the default expect timeout is below
 * what the app promises.
 */
const CONNECTED_AFTER_CONFIRM = { timeout: 20_000 };

/** The line any Go step on the pairing path writes when it ran long (internal/logging/slow_step.go). */
const SLOW_STEP_MARKER = ': slow step=';

/** A process on either side of a pairing whose stderr the spec holds. */
export interface PairingSide {
  stderrLines(): readonly string[];
}

/**
 * Runs `assertion` with the CONNECTED_AFTER_CONFIRM wait. When it fails,
 * each side's stderr tail as it stood then is attached under its key, and
 * the slow pairing steps the sides logged are quoted in the failure, so
 * a stall names the exchange that held it. A step still in flight logs when
 * it ends, which the tail attached when a backend stops carries.
 */
export async function connectedAfterConfirm(
  sides: Record<string, PairingSide>,
  assertion: (wait: { timeout: number }) => Promise<void>,
): Promise<void> {
  try {
    await assertion(CONNECTED_AFTER_CONFIRM);
  } catch (error) {
    const info = test.info();
    const slow: string[] = [];
    for (const [name, side] of Object.entries(sides)) {
      const lines = side.stderrLines();
      await info.attach(`${name} stderr at pairing timeout`, { body: lines.join('\n'), contentType: 'text/plain' });
      slow.push(...lines.filter((line) => line.includes(SLOW_STEP_MARKER)).map((line) => `${name}: ${line}`));
    }
    throw new Error(
      `${(error as Error).message}\n\nslow pairing steps:${slow.length > 0 ? `\n${slow.join('\n')}` : ' none logged'}`,
      { cause: error },
    );
  }
}

/** The bound on each line the pairing console prints, and on its exit. */
const PAIRING_CONSOLE_STEP_MS = 10_000;

export async function headlessPairing(harness: HarnessApp) {
  const child = spawnProduct(productBinary('agent-overflow'),
    ['pair', '--json', '--class', 'desktop', '--config-root', harness.bootstrap.dataDir],
    { stdio: ['pipe', 'pipe', 'pipe'], memoryLimitBytes: 512 * 1024 * 1024 });
  const lines = createInterface({ input: child.stdout! });
  const iterator = lines[Symbol.asyncIterator]();
  let diagnostic = '';
  child.stderr!.on('data', (chunk: Buffer) => { diagnostic = (diagnostic + chunk.toString()).slice(-4096); });
  const exited = new Promise<number | null>((resolve, reject) => { child.once('exit', resolve); child.once('error', reject); });
  const diagnose = processDiagnostics('pairing console', child, () => diagnostic.split('\n'));
  async function record(expected: string): Promise<Record<string, unknown>> {
    const next = await launchStep(`pairing console ${expected} line`, PAIRING_CONSOLE_STEP_MS, () => iterator.next(), diagnose);
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
        expect(await launchStep('pairing console exit', PAIRING_CONSOLE_STEP_MS, () => exited, diagnose)).toBe(0);
      },
      close,
    };
  } catch (error) { close(); throw error; }
}

/**
 * Pairs `other` to `owner` as the owner's own device through AddBackend,
 * names it, and returns its backend id on the owner.
 */
export async function pairOwnDevice(owner: HarnessApp, other: HarnessApp, name: string): Promise<string> {
  const pairing = await headlessPairing(other);
  try {
    const attachment = await owner.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
    await pairing.confirm(attachment.verificationNumber);
    await owner.rpc('RenameBackend', attachment.id, name);
    return attachment.id;
  } finally {
    pairing.close();
  }
}
