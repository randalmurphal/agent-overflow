// The sync gate for `bindings-app.ts`, the fake behind the generated
// `bindings/agent-overflow/app.js` module.
//
// The unit (happy-dom) project resolves a missing named export to
// `undefined`, so a binding added to the app without a matching mock export
// breaks nothing in any gated suite — the strict ESM loader that catches it
// lives only in the `browser` vitest project, which the gates don't run.
// ForgetTailnetNode drifted exactly that way for four waves. This test moves
// the check into the gated suite: the mock must export every function the
// generated module exports (superset rule, stated at the block comment in
// the mock), and must export nothing the generated module lost, so a deleted
// binding takes its mock line with it.
import { describe, expect, it } from 'vitest';
import generatedBindings from '../../../bindings/agent-overflow/app.ts?raw';

import * as mockBindings from './bindings-app';
import { TransportError } from '../../lib/transport/wsClient';

// Exports that are the mock's own machinery, not fakes of generated bindings.
const mockMachinery = new Set([
  'setBindingMock',
  'setReviewDiffMock',
  'setAttachmentThumbnailMock',
  'getBindingMock',
  'resetBindingMocks',
  '__bindingMocksInternal',
  'unansweredBinding',
]);

function generatedFunctionNames(): Set<string> {
  const source = generatedBindings;
  // The generator emits every binding as a top-level `export function Name(`.
  return new Set(
    [...source.matchAll(/^export function (\w+)\(/gm)].map((m) => m[1]),
  );
}

describe('bindings-app mock stays in sync with the generated bindings', () => {
  const generated = generatedFunctionNames();
  const mocked = new Set(
    Object.entries(mockBindings)
      .filter(([name]) => !mockMachinery.has(name))
      // Class exports are model-shape mirrors (TerminalOpenOptions,
      // WSLDistro, …) that tests `new` against, not RPC fakes — the
      // generated counterparts live in models.ts files, not app.ts.
      .filter(
        ([, value]) =>
          !(
            typeof value === 'function' &&
            value.toString().startsWith('class')
          ),
      )
      .map(([name]) => name),
  );

  it('parses the generated module', () => {
    expect(generated.size).toBeGreaterThan(100);
  });

  it('mocks every generated binding', () => {
    const missing = [...generated].filter((name) => !mocked.has(name)).sort();
    expect(missing, 'add these to bindings-app.ts').toEqual([]);
  });

  it('exports no binding the generated module lost', () => {
    const vanished = [...mocked].filter((name) => !generated.has(name)).sort();
    expect(vanished, 'remove these from bindings-app.ts').toEqual([]);
  });
});

describe('bindings-app passive loads', () => {
  it('answer as a backend with nothing recorded after a reset, until a test installs its own', async () => {
    mockBindings.resetBindingMocks();
    await expect(mockBindings.GetSoundFiles()).resolves.toEqual({ dir: '/tmp/sounds', sounds: [], warnings: [] });
    await expect(mockBindings.GetProviderLoginState('claude')).resolves.toEqual({ provider: 'claude', phase: 'idle' });
    await expect(mockBindings.ListProviderAccounts()).resolves.toEqual([]);
    await expect(mockBindings.GetRateLimitsSnapshots()).resolves.toEqual([]);
    await expect(mockBindings.GetThreadLiveState('t1')).resolves.toMatchObject({ threadId: 't1', queueItems: [], interactive: { approvals: [], userInputs: [] } });
    await expect(mockBindings.GetThreadWorkerSummary('t1')).resolves.toEqual({ threadId: 't1', open: 0, needYou: 0 });
    await expect(mockBindings.ListThreadWakes('t1')).resolves.toEqual({ rows: [], moreReminders: 0 });
    await expect(mockBindings.AutoResumeThread('t1')).resolves.toBeUndefined();
    mockBindings.setBindingMock('GetSoundFiles', async () => 'own');
    await expect(mockBindings.GetSoundFiles()).resolves.toBe('own');
    mockBindings.resetBindingMocks();
    await expect(mockBindings.GetSoundFiles()).resolves.toEqual({ dir: '/tmp/sounds', sounds: [], warnings: [] });
  });

  it('list no MCP servers, as for a provider with no built-in servers', async () => {
    await expect(mockBindings.ListThreadMcpServers('t1')).resolves.toEqual([]);
    await expect(mockBindings.ListWorkspaceMcpServers('claude', '/w')).resolves.toEqual([]);
  });

  it('refuse every highlight read as a session without files:read', async () => {
    for (const read of [
      () => mockBindings.HighlightSchemaVersion(),
      () => mockBindings.HighlightClassNames(),
      () => mockBindings.HighlightCode({ lang: 'ts', source: 'x' }),
      () => mockBindings.HighlightPatch({ path: 'a.ts', patch: '' }),
      () => mockBindings.HighlightPatchWithContext('/w', {}),
      () => mockBindings.HighlightEditPatchWithContext('t1', {}),
    ]) {
      const refusal = await (read() as Promise<unknown>).catch((error: unknown) => error);
      expect(refusal).toBeInstanceOf(TransportError);
      expect(refusal).toMatchObject({ code: 'scope_required', scope: 'files:read' });
    }
  });
});
