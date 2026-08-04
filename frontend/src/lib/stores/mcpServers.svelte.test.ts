import { describe, expect, it } from 'vitest';
import { needsEphemeralRefresh } from './mcpServers.svelte';
import { ThreadMCPServer } from './bindings';

function row(over: Partial<ThreadMCPServer>): ThreadMCPServer {
  return new ThreadMCPServer({
    provider: 'claude',
    name: 'srv',
    status: 'unknown',
    disabled: false,
    source: 'config',
    ...over,
  });
}

describe('needsEphemeralRefresh', () => {
  it('never chains on session-sourced listings', () => {
    const rows = [row({ source: 'session', status: 'connected' })];
    expect(needsEphemeralRefresh(rows)).toBe(false);
  });

  it('skips when membership is empty or fully disabled — config enumerates everything', () => {
    expect(needsEphemeralRefresh([])).toBe(false);
    expect(needsEphemeralRefresh([row({ disabled: true, status: 'disabled' })])).toBe(false);
  });

  it('chains only to resolve unknown or stale enabled rows', () => {
    expect(needsEphemeralRefresh([row({})])).toBe(true);
    expect(needsEphemeralRefresh([row({ status: 'connected', stale: true })])).toBe(true);
    expect(needsEphemeralRefresh([row({ status: 'connected' })])).toBe(false);
    // A known failure is an answer, not a gap — no spawn to re-ask.
    expect(needsEphemeralRefresh([row({ status: 'failed' })])).toBe(false);
  });

  it('one unresolved row among resolved ones still chains', () => {
    const rows = [row({ status: 'connected' }), row({ name: 'other' })];
    expect(needsEphemeralRefresh(rows)).toBe(true);
  });
});
