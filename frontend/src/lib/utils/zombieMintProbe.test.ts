import { afterEach, describe, expect, it } from 'vitest';
import {
  maybeInstallZombieMintProbe,
  resetZombieMintProbeForTest,
  shouldInstallZombieMintProbe,
} from './zombieMintProbe';
import {
  flushFrontendErrors,
  resetFrontendErrorCaptureForTest,
} from './frontendErrorCapture';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';

type ProbeGlobal = typeof globalThis & {
  __svelteZombieMint?: (report: Record<string, unknown>) => void;
};

describe('zombieMintProbe', () => {
  afterEach(() => {
    resetZombieMintProbeForTest();
    resetFrontendErrorCaptureForTest();
  });

  it('reports probe payloads as diagnostic records', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    maybeInstallZombieMintProbe();

    (globalThis as ProbeGlobal).__svelteZombieMint!({
      kind: 'reconnect',
      readerIsDerived: true,
      isUpdatingEffect: true,
      isDestroyingEffect: false,
      derivedFn: '() => paneWorkspacePath(get(pane))',
      readerFn: '() => full()',
      stack: 'Error: zombie-mint\n    at get (runtime.js:1)\n    at DiffFileBlock (DiffFileBlock.svelte:42)',
    });
    await flushFrontendErrors();

    const lines = (getBindingMock('ReportFrontendErrorBatch')?.mock.calls[0]?.[0] ?? []) as string[];
    expect(lines).toHaveLength(1);
    const record = JSON.parse(lines[0]);
    expect(record.kind).toBe('diagnostic');
    expect(record.message).toContain('zombie-mint reconnect');
    expect(record.message).toContain('reader=derived');
    expect(record.message).toContain('paneWorkspacePath');
    expect(record.stack).toContain('DiffFileBlock.svelte:42');
  });

  it('redacts secrets in probe stacks and messages', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    maybeInstallZombieMintProbe();

    (globalThis as ProbeGlobal).__svelteZombieMint!({
      kind: 'connect-dirty',
      readerIsDerived: true,
      isUpdatingEffect: true,
      isDestroyingEffect: false,
      derivedFn: '() => connect("ws://host/ws?token=supersecret")',
      readerFn: '() => full()',
      stack: 'Error: zombie-mint\n    at get (ws://host/ws?token=supersecret)',
    });
    await flushFrontendErrors();

    const lines = (getBindingMock('ReportFrontendErrorBatch')?.mock.calls[0]?.[0] ?? []) as string[];
    expect(lines).toHaveLength(1);
    const record = JSON.parse(lines[0]);
    expect(record.message).not.toContain('supersecret');
    expect(record.stack).not.toContain('supersecret');
    expect(record.message).toContain('[redacted]');
    expect(record.stack).toContain('[redacted]');
  });

  it('does not overwrite an existing hook', () => {
    const g = globalThis as ProbeGlobal;
    const sentinel = () => {};
    g.__svelteZombieMint = sentinel;
    maybeInstallZombieMintProbe();
    expect(g.__svelteZombieMint).toBe(sentinel);
  });

  it('installs through the build-gated entry point in tests', () => {
    maybeInstallZombieMintProbe();
    expect((globalThis as ProbeGlobal).__svelteZombieMint).toBeTypeOf('function');
  });

  it('keeps normal builds opt-in while allowing the explicit probe flag', () => {
    expect(shouldInstallZombieMintProbe({ MODE: 'production' })).toBe(false);
    expect(shouldInstallZombieMintProbe({ MODE: 'development' })).toBe(false);
    expect(
      shouldInstallZombieMintProbe({
        MODE: 'production',
        VITE_AGENT_OVERFLOW_ZOMBIE_MINT_PROBE: '1',
      }),
    ).toBe(true);
    expect(shouldInstallZombieMintProbe({ MODE: 'test' })).toBe(true);
  });
});
