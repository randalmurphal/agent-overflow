// TS client for the agent test harness: launches the real backend in
// --harness mode, parses the __AO_HARNESS__ bootstrap line, and speaks
// the transport wire (RPC by method name + event push) over one
// WebSocket. Playwright tests use it for backend setup (seed, scenario
// assignment, replay) and deterministic waits (harness:mock /
// harness:replay events) while the browser exercises the real SPA.
//
// This file is also the reference for driving the harness from any
// other client (Playwright MCP sessions, ad-hoc scripts): everything
// goes through the same bootstrap line + WS wire shown here.

import { mkdtemp, rm } from 'node:fs/promises';
import { createInterface } from 'node:readline';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { chromium } from '@playwright/test';

import {
  spawnContained,
  captureProcessIdentity,
  captureProcessTreeProof,
  captureProcessGroupMemberProof,
  captureProcessGroupMemberProofForPID,
  terminateChildTreeAndWaitVerified,
  type ProcessIdentity,
  type ProcessGroupMemberProof,
  type ProcessTreeProof,
} from './harness-process.ts';
import {
  FALLBACK_MEMORY_LIMIT_BYTES,
  HarnessWatchdog,
} from './harness-watchdog.ts';
import { boundedCleanup } from './harness-cleanup.ts';
import { terminateHarness } from './harness-teardown.ts';
import type { HarnessBootstrap, LaunchOptions } from './harness-types.ts';
import { HarnessApp } from './harness-app.ts';

export type { HarnessBootstrap, LaunchOptions } from './harness-types.ts';
export { HarnessApp } from './harness-app.ts';

const BOOTSTRAP_PREFIX = '__AO_HARNESS__:';

// macOS exposes /var and /tmp as root-owned aliases under /private. Normalize
// only those system prefixes; resolving arbitrary descendants would follow a
// caller-controlled symlink and weaken the harness ownership checks.
function normalizeSystemPath(value: string): string {
  const resolved = path.resolve(value);
  if (process.platform !== 'darwin') return resolved;
  if (resolved === '/var' || resolved.startsWith('/var/')) return `/private${resolved}`;
  if (resolved === '/tmp' || resolved.startsWith('/tmp/')) return `/private${resolved}`;
  return resolved;
}

/** Launch a harness-mode backend and connect to its event wire. */
export async function launchHarness(opts: LaunchOptions = {}): Promise<HarnessApp> {
  const repoRoot = path.resolve(import.meta.dirname, '..', '..');
  const binary =
    opts.binary ?? process.env.AO_HARNESS_BIN ?? path.join(repoRoot, 'bin', 'agent-overflow');
  const mockProvider = opts.mockProvider ?? process.env.AO_MOCKPROVIDER;
  const ownsDataDir = !opts.dataDir;

  if (process.env.AO_E2E_CONTAINED !== '1' && process.env.AO_E2E_FUNCTIONAL_MANAGED !== '1') {
    throw new Error(
      'launchHarness requires the fixed ao-harness-e2e launcher; run `pnpm test`, `pnpm harness:flow`, or `bin/ao-harness-e2e`',
    );
  }
  const requestedDataDir = opts.dataDir ?? (await mkdtemp(path.join(tmpdir(), 'ao-harness-')));
  const dataDir = normalizeSystemPath(requestedDataDir);

  const args = ['--harness', '--data-dir', dataDir];
  if (mockProvider) args.push('--mock-provider', mockProvider);

  const child = spawnContained(binary, args, {
    memoryLimitBytes: opts.memoryLimitBytes ?? FALLBACK_MEMORY_LIMIT_BYTES,
    env: {
      ...process.env,
      // Reuse the browser `make install` already provisioned for this suite;
      // isolated harness roots must not each download Chrome-for-Testing.
      AO_BROWSER_BINARY: chromium.executablePath(),
      ...opts.env,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
    // Give the backend and every provider/helper it starts an owned
    // process group. Teardown must never leave a descendant writing to
    // the run's database after the browser fixture has moved on.
    detached: true,
  });
  let launchIdentity: ProcessIdentity | undefined;
  let launchMemberProof: ProcessGroupMemberProof | undefined;
  let launchTreeProof: ProcessTreeProof | undefined;
  const stderrTail: string[] = [];
  createInterface({ input: child.stderr! }).on('line', (line) => {
    stderrTail.push(line);
    if (stderrTail.length > 200) stderrTail.shift();
    if (process.env.AO_HARNESS_DEBUG) console.error('[backend]', line);
  });
  // Attach to stdout immediately. Fast-failing fixtures can print and exit
  // while the launch identity is still being authenticated on macOS.
  const bootstrapPromise = new Promise<HarnessBootstrap>((resolve, reject) => {
    const timeoutMs = opts.timeoutMs ?? 30_000;
    const timer = setTimeout(() => {
      void terminateHarness('SIGKILL', {
        child,
        watchdog: undefined,
        memberProof: launchMemberProof,
        treeProof: launchTreeProof,
        identity: launchIdentity,
        complete: false,
        closed: false,
        socketOpen: () => false,
        shutdown: async () => undefined,
        closeSocket: () => undefined,
      });
      reject(
        new Error(
          `harness did not print its bootstrap line within ${timeoutMs}ms\n` +
            `binary: ${binary}\nstderr:\n${stderrTail.join('\n')}`,
        ),
      );
    }, timeoutMs);
    createInterface({ input: child.stdout! }).on('line', (line) => {
      const at = line.indexOf(BOOTSTRAP_PREFIX);
      if (at === -1) return;
      clearTimeout(timer);
      try {
        resolve(JSON.parse(line.slice(at + BOOTSTRAP_PREFIX.length)) as HarnessBootstrap);
      } catch (err) {
        reject(new Error(`unparseable harness bootstrap line: ${line} (${err})`));
      }
    });
    child.on('exit', (code) => {
      clearTimeout(timer);
      reject(
        new Error(
          `harness exited with code ${code} before printing its bootstrap line\n` +
            `binary: ${binary}\nstderr:\n${stderrTail.join('\n')}`,
        ),
      );
    });
    if (child.exitCode !== null || child.signalCode !== null) {
      clearTimeout(timer);
      reject(
        new Error(
          `harness exited with code ${child.exitCode ?? 'signal'} before printing its bootstrap line\n` +
            `binary: ${binary}\nstderr:\n${stderrTail.join('\n')}`,
        ),
      );
    }
  });
  // Mark early rejection handled while identity capture is in flight. Awaiting
  // the original promise below still receives the same rejection.
  void bootstrapPromise.catch(() => undefined);
  try {
    launchIdentity = await captureProcessIdentity(child.pid);
  } catch (error) {
    if (child.exitCode !== null || child.signalCode !== null) {
      // The child can fail during runtime startup before the wrapper's exec
      // identity is observable. Continue through the normal bootstrap error
      // path so its stderr and owned data directory are still handled.
      launchIdentity = undefined;
    } else {
      let proof: ProcessGroupMemberProof | undefined;
      try {
        proof = await captureProcessGroupMemberProofForPID(child.pid);
      } catch (proofError) {
        throw new Error(
          `harness launch could not authenticate child and could not inspect its process group; preserving the owned root: ${(error as Error).message}; ${(proofError as Error).message}`,
        );
      }
      if (!proof) {
        throw new Error(
          `harness launch could not authenticate child; preserving the owned root because no surviving process-group proof exists: ${(error as Error).message}`,
        );
      }
      try {
        await terminateChildTreeAndWaitVerified(child, undefined, 'SIGKILL', proof);
      } catch (cleanupError) {
        throw new Error(
          `harness launch could not authenticate child and cleanup failed; preserving the owned root: ${(error as Error).message}; ${(cleanupError as Error).message}`,
        );
      }
      if (ownsDataDir) {
        try {
          await boundedCleanup(`remove harness data dir ${dataDir}`, rm(dataDir, { recursive: true, force: true }));
        } catch (cleanupError) {
          console.error(`harness data-dir cleanup failed after identity error: ${(cleanupError as Error).message}`);
        }
      }
      throw new Error(`harness launch could not authenticate child: ${(error as Error).message}`);
    }
  }
  if (launchIdentity) {
    try {
      launchMemberProof = await captureProcessGroupMemberProof(launchIdentity);
      launchTreeProof = await captureProcessTreeProof(launchIdentity);
    } catch (error) {
      console.error(`harness launch could not capture owned process-tree proof: ${(error as Error).message}`);
    }
  }

  let app: HarnessApp | undefined;
  try {
    const bootstrap = await bootstrapPromise;
    if (bootstrap.pid !== child.pid) {
      throw new Error(`harness bootstrap pid ${bootstrap.pid} does not match spawned pid ${child.pid}`);
    }
    if (normalizeSystemPath(bootstrap.dataRoot) !== dataDir) {
      throw new Error(
        `harness bootstrap data root ${bootstrap.dataRoot} does not match requested root ${dataDir}`,
      );
    }
    if (bootstrap.startupError) {
      throw new Error(`harness backend failed to start: ${bootstrap.startupError}`);
    }
    if (launchIdentity) {
      const currentProof = await captureProcessGroupMemberProof(launchIdentity);
      if (currentProof) launchMemberProof = currentProof;
      try {
        launchTreeProof = await captureProcessTreeProof(launchIdentity);
      } catch (proofError) {
        console.error(
          `harness launch could not refresh owned process-tree proof: ${(proofError as Error).message}`,
        );
      }
    }

    app = new HarnessApp(
      child,
      bootstrap,
      ownsDataDir ? dataDir : undefined,
      launchMemberProof,
      launchTreeProof,
      launchIdentity,
    );
    await app.connect();
    await app.startWatchdog(opts.memoryLimitBytes ?? FALLBACK_MEMORY_LIMIT_BYTES);
    return app;
  } catch (error) {
    let safeToRemove = false;
    try {
      if (app) {
        await app.close();
      } else {
        await terminateHarness('SIGKILL', {
          child,
          watchdog: undefined,
          memberProof: launchMemberProof,
          treeProof: launchTreeProof,
          identity: launchIdentity,
          complete: false,
          closed: false,
          socketOpen: () => false,
          shutdown: async () => undefined,
          closeSocket: () => undefined,
        });
      }
      safeToRemove = true;
    } finally {
      if (ownsDataDir && safeToRemove) {
        try {
          await boundedCleanup(`remove harness data dir ${dataDir}`, rm(dataDir, { recursive: true, force: true }));
        } catch (cleanupError) {
          console.error(`harness data-dir cleanup failed: ${(cleanupError as Error).message}`);
        }
      }
    }
    throw error;
  }
}
