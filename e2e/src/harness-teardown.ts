import type { ChildProcess } from 'node:child_process';
import {
  captureProcessGroupMemberProof,
  captureProcessTreeProof,
  terminateChildTreeAndWaitVerified,
  waitForOwnedTreeExit,
  type ProcessGroupMemberProof,
  type ProcessTreeProof,
  type ProcessIdentity,
} from './harness-process.ts';
import { HarnessWatchdog } from './harness-watchdog.ts';

export interface HarnessTeardownState {
  child: ChildProcess;
  watchdog?: HarnessWatchdog;
  memberProof?: ProcessGroupMemberProof;
  treeProof?: ProcessTreeProof;
  complete: boolean;
  closed: boolean;
  identity?: ProcessIdentity;
  socketOpen: () => boolean;
  shutdown: () => Promise<unknown>;
  closeSocket: () => void;
}

/** Stop one owned backend and report whether its data root can be reclaimed. */
export async function terminateHarness(
  signal: NodeJS.Signals,
  state: HarnessTeardownState,
): Promise<boolean> {
  if (state.complete) return true;
  state.watchdog?.stop();
  const identity = state.identity ?? state.watchdog?.processIdentity;
  if (identity && state.child.exitCode === null && state.child.signalCode === null) {
    try {
      const memberProof = await captureProcessGroupMemberProof(identity);
      if (memberProof) state.memberProof = memberProof;
      state.treeProof = await captureProcessTreeProof(identity);
    } catch (error) {
      console.error(
        `harness teardown could not capture owned process-tree proof: ${(error as Error).message}`,
      );
    }
  }

  if (process.platform === 'win32') {
    if (signal !== 'SIGKILL' && state.socketOpen()) {
      try {
        await state.shutdown();
      } catch (error) {
        console.error(`harness graceful shutdown RPC failed: ${(error as Error).message}`);
      }
    }
    state.closed = true;
    state.closeSocket();
    try {
      await terminateChildTreeAndWaitVerified(
        state.child,
        identity,
        signal,
        state.memberProof,
        state.treeProof,
      );
      state.complete = true;
      return true;
    } catch (error) {
      console.error(
        `harness process ${state.child.pid ?? 'unknown'} survived teardown: ${(error as Error).message}`,
      );
      return false;
    }
  }

  if (state.child.exitCode === null && state.child.signalCode === null) {
    let acknowledged = false;
    if (signal !== 'SIGKILL' && state.socketOpen()) {
      try {
        // The backend owns discovery-file removal and provider teardown.
        // Process-tree termination is the fallback when this call cannot reach it.
        await state.shutdown();
        acknowledged = true;
      } catch (error) {
        console.error(`harness graceful shutdown RPC failed: ${(error as Error).message}`);
      }
    }
    state.closed = true;
    state.closeSocket();
    const exited = await waitForOwnedTreeExit(state.child, acknowledged ? 5_000 : 500);
    if (exited.resolved) {
      state.complete = true;
      return true;
    }
  }

  try {
    await terminateChildTreeAndWaitVerified(
      state.child,
      identity,
      signal,
      state.memberProof,
      state.treeProof,
    );
  } catch (error) {
    console.error(
      `harness process ${state.child.pid ?? 'unknown'} survived teardown: ${(error as Error).message}`,
    );
    state.closed = true;
    state.closeSocket();
    return false;
  }
  state.complete = true;
  state.closed = true;
  state.closeSocket();
  return true;
}
