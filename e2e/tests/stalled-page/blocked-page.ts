// A page whose main thread blocks without spinning: each synchronous request
// waits on a response the server holds for HOLD_MS. Shared by the nested run
// that tests/stalled-page-capture.spec.ts drives.
import http from 'node:http';
import type { AddressInfo } from 'node:net';

const HOLD_MS = 500;

// Both loops are bounded to 60 s so the page frees itself if nothing closes
// it. holdMainThread reads page state between requests, which keeps V8
// reaching the interrupt checks a pause needs; holdWithoutChecks does not,
// and a pause asked for more than a few seconds into it never lands.
export const BLOCKED_PAGE_SCRIPT = `
function holdMainThread() {
  for (let i = 0; i < 120 && !location.hash.includes('free'); i += 1) {
    const xhr = new XMLHttpRequest();
    xhr.open('GET', '/hold', false);
    xhr.send();
  }
}
function holdWithoutChecks() {
  for (let i = 0; i < 120; i += 1) {
    const xhr = new XMLHttpRequest();
    xhr.open('GET', '/hold', false);
    xhr.send();
  }
}
window.blockSoon = (checks) =>
  setTimeout(function blockedTimer() { checks ? holdMainThread() : holdWithoutChecks(); }, 0);
`;

export interface BlockedPageServer {
  url: string;
  close(): Promise<void>;
}

export async function startBlockedPageServer(): Promise<BlockedPageServer> {
  const held = new Set<http.ServerResponse>();
  const server = http.createServer((request, response) => {
    if (request.url === '/hold') {
      held.add(response);
      setTimeout(() => {
        held.delete(response);
        response.end('held');
      }, HOLD_MS);
      return;
    }
    if (request.url === '/app.js') {
      response.setHeader('content-type', 'text/javascript');
      response.end(BLOCKED_PAGE_SCRIPT);
      return;
    }
    response.setHeader('content-type', 'text/html');
    response.end('<!doctype html><title>blocked page</title><script src="/app.js"></script><button>never clicked</button>');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}/`,
    close: () =>
      new Promise<void>((resolve, reject) => {
        for (const response of held) response.destroy();
        server.closeAllConnections();
        server.close((err) => (err ? reject(err) : resolve()));
      }),
  };
}
