import { writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import type { ChildProcess } from 'node:child_process';
import {
  availableMemoryBytes,
  captureProcessIdentity,
  captureProcessGroupMemberProof,
  captureProcessTreeProof,
  processTreeRSS,
  terminateChildTreeAndWaitVerified,
  verifyProcessIdentity,
  waitForOwnedTreeExit,
  type ProcessIdentity,
  type ProcessGroupMemberProof,
  type ProcessTreeProof,
} from './harness-process.ts';

export const FALLBACK_MEMORY_LIMIT_BYTES = 2 * 2 ** 30;
// Preserve enough headroom to absorb a burst between 100 ms samples on hosts
// where an aggregate cgroup or Job Object is unavailable.
export const HOST_AVAILABLE_FLOOR_BYTES = 2 * 2 ** 30;
const WATCHDOG_INTERVAL_MS = 100;

export interface HarnessWatchdogOptions {
  child: ChildProcess;
  dataRoot: string;
  dataDir: string;
  isClosed: () => boolean;
  markClosed: () => void;
  shutdown: () => Promise<unknown>;
  closeSocket: () => void;
  socketOpen: () => boolean;
}

export class HarnessWatchdog {
  private readonly options: HarnessWatchdogOptions;
  private timer?: ReturnType<typeof setInterval>;
  private busy = false;
  private failure?: Error;
  private identity?: ProcessIdentity;
  private memberProof?: ProcessGroupMemberProof;
  private treeProof?: ProcessTreeProof;

  constructor(options: HarnessWatchdogOptions) {
    this.options = options;
  }

  get processIdentity(): ProcessIdentity | undefined {
    return this.identity;
  }

  get processTreeProof(): ProcessTreeProof | undefined {
    return this.treeProof;
  }

  async start(memoryLimitBytes: number): Promise<void> {
    this.identity = await captureProcessIdentity(this.options.child.pid);
    this.memberProof = await captureProcessGroupMemberProof(this.identity);
    this.treeProof = await captureProcessTreeProof(this.identity);
    this.timer = setInterval(() => {
      void this.sample(memoryLimitBytes);
    }, WATCHDOG_INTERVAL_MS);
    this.timer.unref?.();
  }

  stop(): void {
    if (!this.timer) return;
    clearInterval(this.timer);
    this.timer = undefined;
  }

  private async sample(memoryLimitBytes: number): Promise<void> {
    if (this.options.isClosed() || this.busy || this.failure) return;
    this.busy = true;
    try {
      const identity = this.identity;
      if (!identity || !(await verifyProcessIdentity(identity))) {
        throw new Error(
          `harness watchdog: backend identity changed for pid ${this.options.child.pid ?? 'unknown'}`,
        );
      }
      const [rssBytes, availableBytes] = await Promise.all([
        processTreeRSS(identity),
        availableMemoryBytes(),
      ]);
      if (rssBytes > memoryLimitBytes) {
        await this.trip('memory-ceiling', rssBytes, memoryLimitBytes, availableBytes);
      } else if (availableBytes < HOST_AVAILABLE_FLOOR_BYTES) {
        await this.trip('available-floor', rssBytes, memoryLimitBytes, availableBytes);
      }
    } catch (error) {
      await this.trip('watchdog-error', 0, memoryLimitBytes, 0, error as Error);
    } finally {
      this.busy = false;
    }
  }

  private async trip(
    reason: string,
    rssBytes: number,
    memoryLimitBytes: number,
    availableBytes: number,
    cause?: Error,
  ): Promise<void> {
    if (this.failure) return;
    const error = cause ?? new Error(`harness watchdog: ${reason}`);
    this.failure = error;
    this.stop();
    const evidencePath = path.join(this.options.dataDir, 'logs', 'harness-watchdog.json');
    if (this.identity && this.options.child.exitCode === null && this.options.child.signalCode === null) {
      const proof = await captureProcessGroupMemberProof(this.identity);
      if (proof) this.memberProof = proof;
      try {
        this.treeProof = await captureProcessTreeProof(this.identity);
      } catch (proofError) {
        console.error(`harness watchdog: process-tree proof capture failed: ${(proofError as Error).message}`);
      }
    }
    const evidence = {
      version: 1,
      reason,
      pid: this.options.child.pid,
      dataRoot: this.options.dataRoot,
      dataDir: this.options.dataDir,
      rssBytes,
      memoryLimitBytes,
      availableBytes,
      availableFloorBytes: HOST_AVAILABLE_FLOOR_BYTES,
      at: new Date().toISOString(),
      error: cause?.message,
    };
    try {
      await writeFile(evidencePath, JSON.stringify(evidence, null, 2) + '\n', { mode: 0o600 });
    } catch (writeError) {
      console.error(`harness watchdog: evidence write failed: ${(writeError as Error).message}`);
    }
    if (this.options.socketOpen()) {
      try {
        await this.options.shutdown();
      } catch (shutdownError) {
        console.error(
          `harness watchdog: authenticated shutdown failed: ${(shutdownError as Error).message}`,
        );
      }
    }
    this.options.markClosed();
    this.options.closeSocket();
    try {
      if (process.platform === 'win32') {
        await terminateChildTreeAndWaitVerified(
          this.options.child,
          this.identity,
          'SIGKILL',
          this.memberProof,
          this.treeProof,
        );
      } else {
        const exited = await waitForOwnedTreeExit(this.options.child, 5_000);
        if (!exited.resolved) {
          await terminateChildTreeAndWaitVerified(
            this.options.child,
            this.identity,
            'SIGKILL',
            this.memberProof,
            this.treeProof,
          );
        }
      }
    } catch (shutdownError) {
      console.error(`harness watchdog: verified teardown failed: ${(shutdownError as Error).message}`);
    }
    const detail = cause ? `: ${cause.message}` : '';
    console.error(
      `harness watchdog tripped: ${reason}${detail}; rss=${rssBytes} ceiling=${memoryLimitBytes} available=${availableBytes}; evidence ${evidencePath}`,
    );
  }
}
