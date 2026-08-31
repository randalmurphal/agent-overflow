import { describe, expect, it } from 'vitest';
import {
  connectionRefusalMessage,
  isTerminalConnectionStatus,
} from './connectionRefusal';
import type { TerminalTransportStatus } from './wsClient';

// The two states the reconnect ladder stops on. Listed here so a third
// one added to the union fails the type check on this line before it
// reaches a surface that would render nothing for it.
const TERMINAL: TerminalTransportStatus[] = ['unauthorized', 'pairing-required'];

describe('connectionRefusalMessage', () => {
  it('answers every terminal state with a sentence', () => {
    for (const status of TERMINAL) {
      const message = connectionRefusalMessage(status);
      expect(message.length, `no sentence for ${status}`).toBeGreaterThan(0);
      expect(message.trim()).toBe(message);
    }
  });

  // Same banner, same slot: two states that read alike would leave the
  // person doing the wrong thing about half the time.
  it('gives the two states different remedies', () => {
    expect(connectionRefusalMessage('unauthorized')).not.toBe(
      connectionRefusalMessage('pairing-required'),
    );
    // Each names the action that works for it and not the other's.
    expect(connectionRefusalMessage('unauthorized')).toContain('share link');
    expect(connectionRefusalMessage('pairing-required')).toContain('Pair this device');
  });

  // frontend/AGENTS.md: no visible in-app explanatory text for internal
  // mechanics. Peer locality, cookies, tickets and upgrades are not
  // things a person can act on.
  it('names no internal mechanism', () => {
    for (const status of TERMINAL) {
      const message = connectionRefusalMessage(status).toLowerCase();
      for (const word of ['loopback', 'cookie', 'ticket', 'websocket', 'upgrade', 'socket', '404']) {
        expect(message, `${status} mentions ${word}`).not.toContain(word);
      }
    }
  });
});

describe('isTerminalConnectionStatus', () => {
  it('accepts exactly the states the ladder stops on', () => {
    for (const status of TERMINAL) {
      expect(isTerminalConnectionStatus(status)).toBe(true);
    }
    for (const status of ['connected', 'reconnecting', 'disconnected', '', 'toString']) {
      expect(isTerminalConnectionStatus(status), status).toBe(false);
    }
  });
});
