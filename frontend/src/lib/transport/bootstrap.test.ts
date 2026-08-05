// Locality of the document origin decides whether a refused bootstrap
// credential is terminal (see wsClient's credentialDead latch): a page
// served over the network can only be re-credentialled by re-opening a
// share link, while a loopback page is handed a live token by the shell
// that launched it. Getting this predicate wrong in either direction is
// a user-visible bug — a false "remote" tells a desktop user to reopen a
// share link that does not exist, and a false "loopback" leaves a phone
// retrying a dead token forever.

import { describe, expect, it } from 'vitest';
import { isLoopbackHostname, pageServedOverLoopback } from './bootstrap';

describe('isLoopbackHostname', () => {
  it('accepts every host that names this machine', () => {
    // The embedded webview loads 127.0.0.1 (cmd/agent-overflow-windows
    // builds exactly that URL); dev servers and the --connect stub use
    // localhost; ::1 arrives without brackets from location.hostname.
    for (const host of [
      '127.0.0.1',
      '127.0.0.53',
      '127.1.2.3',
      'localhost',
      'LOCALHOST',
      'app.localhost',
      '::1',
      '[::1]',
      '  localhost  ',
    ]) {
      expect(isLoopbackHostname(host), host).toBe(true);
    }
  });

  it('rejects hosts that came off the network', () => {
    for (const host of [
      '192.168.1.24', // the LAN share URL (internal/network)
      '10.0.0.5',
      '172.17.0.2',
      'desktop.tailnet.ts.net',
      'example.com',
      // Near-misses that must not read as loopback: a name that merely
      // contains the string, and an address outside 127.0.0.0/8.
      'notlocalhost',
      'localhost.evil.com',
      '128.0.0.1',
      '1.127.0.0',
      '',
    ]) {
      expect(isLoopbackHostname(host), host).toBe(false);
    }
  });
});

describe('pageServedOverLoopback', () => {
  // happy-dom serves tests from http://localhost/.
  it('reads the current document origin', () => {
    expect(pageServedOverLoopback()).toBe(true);
  });
});
