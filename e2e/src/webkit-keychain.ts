// macOS WebKit wraps a non-extractable CryptoKey a page stores in IndexedDB
// (a paired device's key) with a master key it keeps in the default keychain.
// In the user's login keychain that item trusts only the WebKit build that
// created it, so every Playwright WebKit update raises a keychain prompt, and
// an unanswered prompt blocks the page. The Security framework finds the
// default keychain under $HOME, so a HOME holding its own unlocked keychain
// keeps WebKit off the user's keychain. Chromium has --use-mock-keychain.
import { execFile } from 'node:child_process';
import { mkdir, mkdtemp, realpath, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { promisify } from 'node:util';

const run = promisify(execFile);

export interface WebKitKeychainHome {
  /** The environment a WebKit launch takes; undefined off macOS. */
  env: Record<string, string> | undefined;
  close: () => Promise<void>;
}

export async function webkitKeychainHome(): Promise<WebKitKeychainHome> {
  if (process.platform !== 'darwin') return { env: undefined, close: async () => {} };
  // Resolved, as security prints the keychain path (macOS /var).
  const home = await realpath(await mkdtemp(path.join(tmpdir(), 'ao-webkit-home-')));
  const close = () => rm(home, { recursive: true, force: true });
  try {
    const keychain = path.join(home, 'Library', 'Keychains', 'login.keychain-db');
    await mkdir(path.dirname(keychain), { recursive: true });
    const env: Record<string, string> = { HOME: home };
    for (const [name, value] of Object.entries(process.env)) {
      if (value !== undefined && name !== 'HOME') env[name] = value;
    }
    // Created unlocked with an empty password; the settings call drops the
    // idle lock so it stays unlocked for the worker's lifetime.
    await run('/usr/bin/security', ['create-keychain', '-p', '', keychain], { env });
    await run('/usr/bin/security', ['set-keychain-settings', keychain], { env });
    const { stdout } = await run('/usr/bin/security', ['default-keychain'], { env });
    if (!stdout.includes(keychain)) throw new Error(`the WebKit home's default keychain is ${stdout.trim()}, not ${keychain}`);
    return { env, close };
  } catch (error) {
    await close();
    throw error;
  }
}
