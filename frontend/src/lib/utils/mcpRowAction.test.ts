import { describe, expect, it } from 'vitest';

import { ThreadMCPServer } from '../stores/bindings';
import { mcpRowAction } from './mcpRowAction';

function row(over: Partial<ThreadMCPServer> = {}): ThreadMCPServer {
  return new ThreadMCPServer({
    provider: 'codex',
    name: 'atlassian',
    status: 'connected',
    disabled: false,
    source: 'session',
    ...over,
  });
}

describe('mcpRowAction', () => {
  it('needs-auth always offers Sign in, even on a reconnectable session row', () => {
    expect(mcpRowAction(row({ status: 'needs-auth' }), true).kind).toBe('sign-in');
    expect(mcpRowAction(row({ status: 'needs-auth' }), false).kind).toBe('sign-in');
  });

  it('failed with an OAuth credential offers Sign in again over Reconnect', () => {
    // The incident shape: startup failed with invalid_grant while
    // authStatus still reads oAuth (credential present but revoked).
    // Same kind as needs-auth — one behavior, two labels.
    const spec = mcpRowAction(row({ status: 'failed', authStatus: 'oAuth' }), true);
    expect(spec.kind).toBe('sign-in');
    expect(spec.label).toBe('Sign in again');
  });

  it('failed without an OAuth credential falls through to Reconnect / Refresh', () => {
    expect(mcpRowAction(row({ status: 'failed', authStatus: 'unsupported' }), true).kind).toBe(
      'reconnect',
    );
    expect(mcpRowAction(row({ status: 'failed' }), false).kind).toBe('refresh');
    expect(mcpRowAction(row({ status: 'failed', authStatus: 'bearerToken' }), false).kind).toBe(
      'refresh',
    );
  });

  it('healthy rows offer Reconnect on own-session rows, Refresh otherwise', () => {
    expect(mcpRowAction(row({ status: 'connected' }), true).kind).toBe('reconnect');
    expect(mcpRowAction(row({ status: 'connected', source: 'config' }), false).kind).toBe(
      'refresh',
    );
    expect(mcpRowAction(row({ status: 'unknown', source: 'config' }), false).kind).toBe('refresh');
  });

  it('titles name the server', () => {
    expect(mcpRowAction(row({ status: 'failed', authStatus: 'oAuth' }), false).title).toContain(
      'atlassian',
    );
  });
});
