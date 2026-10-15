import { expect } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';

export interface TransferRow { id: string; threadId: string; kind: string; phase: string; error?: string; targetThreadId: string }

/**
 * Waits for the source host's transfer of threadId (of kind, when given) to
 * complete, failing early with either host's error. A transfer is a durable
 * host-to-host protocol whose phases each sync to disk, so under disk load it
 * outlasts the default expect timeout; the frontend's own draft move waits up
 * to five minutes (frontend/src/lib/stores/draftProjectTransfer.ts).
 */
export async function expectTransferComplete(source: HarnessApp, destination: HarnessApp, threadId: string, kind?: string): Promise<TransferRow> {
  let result: TransferRow | undefined;
  await expect.poll(async () => {
    const rows = await source.rpc<TransferRow[]>('GetThreadTransfers');
    result = rows.find((row) => row.threadId === threadId && (!kind || row.kind === kind));
    if (result?.phase === 'complete') return 'complete';
    const recipient = (await destination.rpc<TransferRow[]>('GetThreadTransfers')).find((row) => row.id === result?.id);
    return result?.error || recipient?.error || result?.phase;
  }, { timeout: 40_000, intervals: [250] }).toBe('complete');
  return result!;
}

// The real Codex app-server resolves a durable transcript through thread/read.
// The mock has no native index: supply that one response for an inert fixture.
export function codexTransferScenario(id?: string, path?: string): object {
  return {
    version: 1, name: 'codex-conversation-transfer', provider: 'codex',
    onStart: [{ delayMs: 1 }], turns: [], afterTurns: 'exit',
    codex: { responses: id && path ? {
      'thread/read': JSON.stringify({
        jsonrpc: '2.0', id: '${REQUEST_ID}',
        result: { thread: { id, path, status: { type: 'idle', activeFlags: [] } } },
      }).replace('"${REQUEST_ID}"', '${REQUEST_ID}'),
    } : {} },
  };
}
