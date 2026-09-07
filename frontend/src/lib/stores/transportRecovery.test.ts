import { afterEach, expect, it, vi } from 'vitest';

const fixture = vi.hoisted(() => {
  function client() {
    const replay = new Set<(phase: 'start' | 'complete' | 'cancel') => void>();
    const status = new Set<(state: { status: string }) => void>();
    return {
      replay, status,
      onReplay(fn: (phase: 'start' | 'complete' | 'cancel') => void) {
        replay.add(fn); return () => replay.delete(fn);
      },
      onStatusChange(fn: (state: { status: string }) => void) {
        status.add(fn); fn({ status: 'connected' }); return () => status.delete(fn);
      },
    };
  }
  return { home: client(), remote: client() };
});
vi.mock('../transport/backends', () => ({
  attachedBackends: () => [{ id: '', client: fixture.home }, { id: 'remote', client: fixture.remote }],
  onBackendsChanged: () => () => {},
}));

import { holdBackendRecovery, onBackendRecovery } from './transportRecovery';
import { itemEventQueued, itemEventsSettled, resetItemEventSettlement } from './itemEventSettlement';

const offs: Array<() => void> = [];
afterEach(() => {
  resetItemEventSettlement();
  for (const off of offs.splice(0)) off();
  for (const client of [fixture.home, fixture.remote]) for (const fn of client.replay) fn('cancel');
});

it('finishes replay presentation after queued item mutations reach the panes', async () => {
  const events: string[] = [];
  offs.push(onBackendRecovery((id, phase) => events.push(`${id}:${phase}`)));
  replay(fixture.home, 'start');
  itemEventQueued();
  itemEventQueued();
  replay(fixture.home, 'complete');
  itemEventsSettled(1);
  await Promise.resolve();
  expect(events).toEqual([':start']);
  itemEventQueued(); // New live work must not extend the captured replay fence.
  itemEventsSettled(1);
  await Promise.resolve();
  expect(events).toEqual([':start', ':complete']);
});
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => { resolve = done; });
  return { promise, resolve };
}
function replay(client: typeof fixture.home, phase: 'start' | 'complete' | 'cancel') {
  for (const fn of client.replay) fn(phase);
}

it('waits for gap snapshots and keeps unrelated backends independent', async () => {
  const events: string[] = [];
  offs.push(onBackendRecovery((id, phase) => events.push(`${id}:${phase}`)));
  const first = deferred();
  const second = deferred();
  replay(fixture.home, 'start');
  holdBackendRecovery('', first.promise);
  replay(fixture.home, 'complete');
  // A trailing resync arriving during the first read is part of recovery.
  holdBackendRecovery('', second.promise);
  replay(fixture.remote, 'start');
  replay(fixture.remote, 'complete');
  expect(events).toEqual([':start', 'remote:start', 'remote:complete']);
  first.resolve();
  await first.promise;
  expect(events).not.toContain(':complete');
  second.resolve();
  await vi.waitFor(() => expect(events.at(-1)).toBe(':complete'));
});

it('cancels pending snapshots on disconnect without completing a later recovery', async () => {
  const events: string[] = [];
  offs.push(onBackendRecovery((id, phase) => events.push(`${id}:${phase}`)));
  const pending = deferred();
  replay(fixture.home, 'start');
  holdBackendRecovery('', pending.promise);
  replay(fixture.home, 'complete');
  for (const fn of fixture.home.status) fn({ status: 'reconnecting' });
  replay(fixture.home, 'start');
  pending.resolve();
  await pending.promise;
  await Promise.resolve();
  expect(events).toEqual([':start', ':cancel', ':start']);
  replay(fixture.home, 'complete');
  expect(events.at(-1)).toBe(':complete');
});
