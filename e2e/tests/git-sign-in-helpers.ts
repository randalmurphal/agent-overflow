// Fixtures of the sign-in marker specs: a local HTTP remote in this worker
// that answers every request 401, a seeded repository with one thread, and
// a bare remote under the data root that needs no login.
import { mkdir } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import path from 'node:path';

import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';
import { harnessGit } from './worktree-removal-helpers.js';

export interface LoginWall {
  url: string;
  requests: () => number;
  close: () => Promise<void>;
}

/** A remote that wants a login for everything, counting what it was asked. */
export async function loginWall(): Promise<LoginWall> {
  let requests = 0;
  const server: Server = createServer((_req, res) => {
    requests += 1;
    res.writeHead(401, { 'WWW-Authenticate': 'Basic realm="git"' });
    res.end();
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}/secret-group/repo.git`,
    requests: () => requests,
    close: () => new Promise<void>((resolve, reject) => server.close((err) => (err ? reject(err) : resolve()))),
  };
}

/** Seeds project `name` with one thread whose origin is `origin`. */
export async function seedThreadWithOrigin(harness: HarnessApp, name: string, origin: string): Promise<{ root: string; title: string }> {
  const title = `${name} thread`;
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name,
      repo: { commits: [{ message: 'init', files: { 'notes.md': 'base\n' } }] },
      threads: [{ title, provider: 'claude', turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }] }],
    }],
  });
  const root = seed.projects[0].path;
  harnessGit(harness, root, 'remote', 'add', 'origin', origin);
  return { root, title };
}

/**
 * Points origin of `root` at a new bare repository under the data root, as
 * signing in would make it answer. The marker is clicked only after this:
 * a clicked fetch may ask, and against the login wall a developer machine's
 * credential helper could open a real window.
 */
export async function pointOriginAtOpenRemote(harness: HarnessApp, root: string, name: string): Promise<void> {
  const bare = path.join(harness.bootstrap.dataRoot, 'sign-in-remote', `${name}.git`);
  await mkdir(bare, { recursive: true });
  harnessGit(harness, bare, 'init', '--bare', '--quiet');
  harnessGit(harness, root, 'remote', 'set-url', 'origin', bare);
}
