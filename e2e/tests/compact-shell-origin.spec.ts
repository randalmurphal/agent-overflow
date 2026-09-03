// The SPA served from an origin that is not its backend's, against the
// real Go server: pairing, bootstrap, an RPC over the socket, and an
// attachment upload, all cross-origin.
//
// WHY THIS FILE EXISTS. Wave 6f-c made the page's origin and the home
// backend's origin two different things for the first time. Everything
// under it is covered a level down — the URL rewriting in
// `homeEndpoint.test.ts`, the CORS answer in `shellorigin_test.go`, the
// request shapes in `bootstrap.test.ts` / `deviceSession.test.ts` /
// `attachmentTransfer.test.ts` — and every one of those tests answers its
// own question with a stub on the other side of the seam. What only a
// real boot can answer is whether the two halves COMPOSE: whether a
// browser, which enforces CORS itself and which nothing in this repo can
// mock, will actually let this page send `X-AO-Session` to another origin
// and read what comes back.
//
// That is the whole reason the page is served by a SEPARATE HTTP server
// here. A spec that set `__aoHomeEndpoint` on a page the backend served
// would exercise the rewriting and prove nothing about CORS, because
// every request would still be same-origin. The throwaway server on
// another port is what makes the browser enforce the thing under test.
//
// WHAT IS REAL. The bundle is `frontend/dist`, unmodified. The pairing is
// the shipped `PairingScreen` on the shipped `deviceSession.redeemPairing`.
// The socket is the shipped `wsClient`. The upload is the shipped
// `handleDrop` on the shipped `Composer.svelte`, so the bytes ride
// `uploadAttachmentBytes` as it ships. The only thing spoken over the
// wire rather than clicked is the owner's CONFIRMATION, which is a
// host-side RPC the modal issues verbatim.
//
// WHY IT OWNS ITS BACKEND. The admitted extra origin is read from the
// process environment (`transport.ShellOriginExtraEnv`), and the port the
// page is served from is not known until a listener has one — so the
// backend has to be launched AFTER the static server, with that origin in
// its environment. Borrowing the worker fixture's instance cannot express
// that ordering.
//
// FIVE CASES, and the four after the first exist because the first
// cannot see everything. Chromium does not surface a preflight to the
// page, so the OPTIONS the upload needs — and the ones the bundle routes
// need — are asked of the running server directly. A backend that
// answered `*` would pass every positive case perfectly, so the last one
// points a page at an origin this backend was never told about and
// requires the manifest fetch to be refused. And the bundle case is the
// phone's update channel proved with no phone: a paired page on the
// other origin downloading the archive, unzipping it and checking every
// digest against the manifest.

import { createServer, type Server } from 'node:http';
import { readFile } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import * as path from 'node:path';

import { expect, test, type Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';

// ---------------------------------------------------------------------
// Wire shapes, mirroring internal/app/app_access_types.go.
// ---------------------------------------------------------------------

interface PairingInvite {
  linkId: string;
  url: string;
}

interface PendingPairing {
  linkId: string;
  redeemed?: boolean;
  verificationNumber?: string;
}

interface AccessOverview {
  pendingPairings?: PendingPairing[];
}

interface SeedResult {
  projects: Array<{ projectId: string; path: string; threadIds: string[] }>;
}

interface AttachmentRow {
  id: string;
  filename: string;
  mimeType: string;
  size: number;
}

// The same 300x200 fixture `attachment-transfer.spec.ts` uses, and for
// the same reason: 494 bytes is well under the composer's compression
// threshold, so the bytes that cross are the bytes handed in.
const PNG_BASE64 =
  'iVBORw0KGgoAAAANSUhEUgAAASwAAADICAIAAADdvUsCAAABtUlEQVR42u3TAQkAAAjAMLOYxXCGM5QxRBgsweGRPc' +
  'ChkABMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYE' +
  'TAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBEwIJgRMCCYETAgmBBOqAC' +
  'YEEwImBBMCJgQTAiYEEwImBBMCJgQTAiYEEwImBBMCJgQTAiYEEwImBBMCJgQTAiYEEwImBBMCJgQTAiYEEwImhNcT' +
  'TiVwyIRgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQj' +
  'AhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIWBCMCFgQjAhYEIwIZhQ' +
  'AjAhmBAwIZgQMCGYEDAhmBAwIZgQMCGYEDAhmBAwIZgQMCGYEDAhmBAwIZgQMCGYEDAhmBAwIZgQMCGYEDAhmBAwIb' +
  'y2ay5ZumneNVwAAAAASUVORK5CYII=';
const PNG_BYTES = Buffer.from(PNG_BASE64, 'base64');
const FILENAME = 'cross-origin.png';

// The pairing screen probes every 3s, holds the confirmed frame 700ms,
// then awaits `redialAfterPairing` (bounded at 5s). ~9s is the designed
// worst case; this is roughly twice it.
const PAIRED_APP_MOUNT_MS = 20_000;

const MIME: Record<string, string> = {
  '.html': 'text/html; charset=utf-8',
  // `bundle-id.txt` is in the served tree, and a type this map does not
  // know is `application/octet-stream` — which a browser DOWNLOADS rather
  // than renders, so a navigation to it never becomes a page.
  '.txt': 'text/plain; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json',
  '.svg': 'image/svg+xml',
  '.woff2': 'font/woff2',
  '.woff': 'font/woff',
  '.png': 'image/png',
};

/**
 * Serve `frontend/dist` on an ephemeral port, and nothing else.
 *
 * Deliberately the plainest possible static server: no CSP, no cookies,
 * no credentials of any kind. It stands in for the Capacitor WebView's
 * local asset origin, whose whole contribution to this flow is being a
 * DIFFERENT origin from the backend's. Unknown paths fall back to
 * index.html, which is what any SPA host does.
 */
async function serveBundle(dist: string): Promise<{ origin: string; close: () => Promise<void> }> {
  const server: Server = createServer((req, res) => {
    void (async () => {
      const url = new URL(req.url ?? '/', 'http://127.0.0.1');
      // Resolved against dist and then checked to still be inside it, so
      // a traversal in the request path serves index.html rather than
      // whatever is above the directory.
      const wanted = path.resolve(dist, '.' + url.pathname);
      const file = wanted.startsWith(dist + path.sep) ? wanted : path.join(dist, 'index.html');
      try {
        const body = await readFile(file);
        res.writeHead(200, { 'content-type': MIME[path.extname(file)] ?? 'application/octet-stream' });
        res.end(body);
      } catch {
        const body = await readFile(path.join(dist, 'index.html'));
        res.writeHead(200, { 'content-type': MIME['.html'] });
        res.end(body);
      }
    })();
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    origin: `http://127.0.0.1:${port}`,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

/** The `#pair=` fragment off a minted link, which is all this page needs. */
function fragmentOf(invite: PairingInvite): string {
  const at = invite.url.indexOf('#pair=');
  expect(at, 'a pairing link must carry its payload in the fragment').toBeGreaterThan(-1);
  return invite.url.slice(at);
}

/** The owner's half: find the redeemed link, compare the number, confirm. */
async function confirmOnHost(harness: HarnessApp, shownOnDevice: string): Promise<void> {
  let redeemed: PendingPairing | undefined;
  await expect
    .poll(async () => {
      const overview = await harness.rpc<AccessOverview>('GetAccessOverview');
      redeemed = (overview.pendingPairings ?? []).find((p) => p.redeemed);
      return redeemed?.verificationNumber ?? '';
    }, { message: 'the redemption must reach the backend as a pairing awaiting confirmation' })
    // The comparison IS the gate: the number is an HMAC over the key the
    // device actually presented, so a redemption that never crossed could
    // not produce one this side matches.
    .toBe(shownOnDevice);
  await harness.rpc('ConfirmDevicePairing', redeemed!.linkId);
}

/**
 * Drop one image on the composer the way a browser does: a real
 * `DataTransfer` carrying a real `File`. Everything after this line is
 * the app's own upload path.
 */
async function dropImage(page: Page): Promise<void> {
  await page.getByTestId('composer-root').evaluate(
    (root, payload) => {
      const binary = atob(payload.base64);
      const bytes = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
      const transfer = new DataTransfer();
      transfer.items.add(new File([bytes], payload.filename, { type: 'image/png' }));
      root.dispatchEvent(
        new DragEvent('drop', { dataTransfer: transfer, bubbles: true, cancelable: true }),
      );
    },
    { base64: PNG_BASE64, filename: FILENAME },
  );
}

test.describe.serial('the SPA served from its own origin', () => {
  let bundle: Awaited<ReturnType<typeof serveBundle>>;
  let harness: HarnessApp;
  let backendOrigin: string;
  let threadId: string;

  test.beforeAll(async () => {
    const dist = path.resolve(import.meta.dirname, '..', '..', 'frontend', 'dist');
    bundle = await serveBundle(dist);
    // The backend learns the page's origin at LAUNCH, because the port
    // did not exist before the line above. This is the harness-only door
    // the constant `transport.ShellOrigin` cannot cover: a shipped shell
    // has one fixed origin, and a test server has whatever it was given.
    harness = await launchHarness({ env: { AO_SHELL_ORIGIN_EXTRA: bundle.origin } });
    backendOrigin = new URL(harness.url).origin;
    expect(backendOrigin, 'the page and the backend must be different origins for this to test anything')
      .not.toBe(bundle.origin);

    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        {
          name: 'cross-origin-app',
          repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
          threads: [
            {
              title: 'Cross origin thread',
              turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'hi' }] }],
            },
          ],
        },
      ],
    });
    threadId = seed.projects[0].threadIds[0];
  });

  test.afterAll(async () => {
    await harness?.close();
    await bundle?.close();
  });

  test('pairs, boots, calls and uploads across the origin boundary', async ({ browser }) => {
    const context = await browser.newContext();
    const page = await context.newPage();

    // Every ANSWER the backend gave this page, with the one header that
    // decides whether the page was allowed to read it. The assertions
    // below are about what actually crossed and what came back on it,
    // rather than about what the UI implies.
    interface Answer {
      method: string;
      url: string;
      status: number;
      allowOrigin: string;
      allowMethods: string;
      allowHeaders: string;
    }
    const answers: Answer[] = [];
    page.on('response', (res) => {
      if (!res.url().startsWith(backendOrigin)) return;
      const h = res.headers();
      answers.push({
        method: res.request().method(),
        url: res.url(),
        status: res.status(),
        allowOrigin: h['access-control-allow-origin'] ?? '',
        allowMethods: h['access-control-allow-methods'] ?? '',
        allowHeaders: h['access-control-allow-headers'] ?? '',
      });
    });
    // A CORS answer the browser rejects surfaces as a failed request and
    // nothing else, so failures are collected rather than inferred from a
    // timeout three assertions later. `ERR_ABORTED` is excluded on
    // purpose: it is the CLIENT cancelling — `redialAfterPairing` closes
    // the dial the pairing screen had already started — and reading it as
    // a refusal would make this spec fail for a transport that worked.
    const refused: string[] = [];
    page.on('requestfailed', (req) => {
      const why = req.failure()?.errorText ?? 'failed';
      if (!req.url().startsWith(backendOrigin) || why === 'net::ERR_ABORTED') return;
      refused.push(`${req.method()} ${req.url()}: ${why}`);
    });
    const seen = (method: string, path: string): Answer[] =>
      answers.filter((a) => a.method === method && a.url.startsWith(backendOrigin + path));

    // The shell's door, and the e2e's. One global, so what this spec
    // exercises is what the shell uses (lib/transport/homeEndpoint.ts).
    await page.addInitScript((endpoint) => {
      (window as unknown as { __aoHomeEndpoint: string }).__aoHomeEndpoint = endpoint;
    }, backendOrigin);

    const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'browser', 'full');
    await page.goto(bundle.origin + '/' + fragmentOf(invite));

    // --- Pairing, cross-origin -----------------------------------------
    // POST /auth/pair to another origin, carrying X-AO-Device-Key and a
    // JSON content type: a preflight the backend has to answer, then a
    // response body the browser has to let the page read. Both halves
    // fail as "pairing did not go through" if the CORS answer is wrong.
    await expect(page.getByRole('heading', { name: 'Pair this device' })).toBeVisible();
    await page.getByLabel('Device name').fill('Cross-origin shell');
    await page.getByRole('button', { name: 'Pair' }).click();
    const shown = page.getByLabel('Verification number');
    await expect(shown).toBeVisible();
    await confirmOnHost(harness, ((await shown.textContent()) ?? '').trim());

    // --- Bootstrap and the socket ---------------------------------------
    // The app mounts only after the manifest resolved from the OTHER
    // origin and the socket opened to it. The sidebar row is the proof
    // that a list RPC crossed and came back.
    const row = page.getByTestId('thread-row').filter({ hasText: 'Cross origin thread' });
    await expect(row).toBeVisible({ timeout: PAIRED_APP_MOUNT_MS });
    expect(refused, 'no request to the backend may be refused by the browser').toEqual([]);

    // The pairing POST carries `Content-Type: application/json` and a
    // device-key header, so it is not a simple request: the browser sent
    // a preflight of its own and refused to send this at all until the
    // preflight was answered. That the POST HAPPENED and was readable is
    // therefore the preflight's proof — Chromium does not surface the
    // preflight itself to the page, and asserting on one this side would
    // be asserting on the test's own plumbing.
    const pair = seen('POST', '/auth/pair');
    expect(pair, 'the pairing redemption must have crossed to the backend origin').toHaveLength(1);
    expect(pair[0].allowOrigin).toBe(bundle.origin);

    // The manifest came from the backend origin, and the page was allowed
    // to read it. Naming the origin exactly is the assertion: a `*` would
    // also satisfy the browser here and is what this backend must never
    // answer.
    const manifest = seen('GET', '/bootstrap.json');
    expect(manifest.length, 'the manifest must be fetched from the backend origin').toBeGreaterThan(0);
    expect(manifest[0].allowOrigin).toBe(bundle.origin);
    expect(
      manifest.every((m) => m.allowOrigin !== '*'),
      'the allowed origin is one origin, never a wildcard',
    ).toBe(true);

    // --- One RPC, driven from the UI -------------------------------------
    await row.click();
    await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
    await expect(page.getByTestId('chat-header-title')).toHaveText('Cross origin thread');
    await expect(page.getByTestId('composer-root')).toBeVisible();

    // --- An attachment upload --------------------------------------------
    // PUT with a Blob body to another origin: not a simple request, so it
    // is preflighted, and the created row is read out of a cross-origin
    // response body. The chip appears only once both have happened.
    await dropImage(page);
    await expect(page.getByTestId('attachment-thumb')).toBeVisible();

    const stored = await harness.rpc<AttachmentRow[]>('ListAttachments', threadId);
    expect(stored).toHaveLength(1);
    expect(stored[0].filename).toBe(FILENAME);
    expect(stored[0].size).toBe(PNG_BYTES.length);

    const upload = seen('PUT', '/attachments/upload');
    expect(upload, 'the bytes must have crossed to the backend origin').toHaveLength(1);
    expect(upload[0].allowOrigin).toBe(bundle.origin);
    expect(refused, 'no request to the backend may be refused by the browser').toEqual([]);

    await context.close();
  });

  test('the preflight the upload needs is answered by the running server', async () => {
    // The upload route's pattern is method-scoped (`PUT /attachments/upload`),
    // so the OPTIONS the browser sends ahead of it reaches a route that
    // exists only to answer preflights. The browser does not surface that
    // exchange to the page, so it is asked here directly — against the
    // same running server the case above drove, which is what makes this
    // an integration check rather than a second copy of the Go unit test.
    const preflight = await fetch(backendOrigin + '/attachments/upload', {
      method: 'OPTIONS',
      headers: {
        origin: bundle.origin,
        'access-control-request-method': 'PUT',
        'access-control-request-headers': 'content-type, x-ao-session',
      },
    });
    expect(preflight.status).toBe(204);
    expect(preflight.headers.get('access-control-allow-origin')).toBe(bundle.origin);
    expect(preflight.headers.get('access-control-allow-methods')).toContain('PUT');
    expect((preflight.headers.get('access-control-allow-headers') ?? '').toLowerCase())
      .toContain('x-ao-session');
    // The two rules that keep this one door narrow.
    expect(preflight.headers.get('access-control-allow-credentials')).toBeNull();
    expect(preflight.headers.get('vary')?.toLowerCase()).toContain('origin');
  });

  test('the bundle routes answer their own preflights', async () => {
    // Same reason as the upload's: both patterns are method-qualified,
    // so each needs its own OPTIONS route or the mux answers 405 and the
    // browser never sends the GET. A session header makes both requests
    // non-simple, so the preflight is not optional.
    for (const route of ['/bundle/manifest.json', '/bundle/archive.zip']) {
      const preflight = await fetch(backendOrigin + route, {
        method: 'OPTIONS',
        headers: {
          origin: bundle.origin,
          'access-control-request-method': 'GET',
          'access-control-request-headers': 'x-ao-session, x-ao-device-key',
        },
      });
      expect(preflight.status, route).toBe(204);
      expect(preflight.headers.get('access-control-allow-origin'), route).toBe(bundle.origin);
      expect(preflight.headers.get('access-control-allow-methods'), route).toContain('GET');
      expect((preflight.headers.get('access-control-allow-headers') ?? '').toLowerCase(), route)
        .toContain('x-ao-session');
      expect(preflight.headers.get('access-control-allow-credentials'), route).toBeNull();
      expect(preflight.headers.get('vary')?.toLowerCase(), route).toContain('origin');
    }
  });

  test('a paired page downloads this backend\'s bundle and the archive matches its manifest', async ({
    browser,
  }) => {
    // The seam a phone updates through, proved with no phone.
    //
    // What only this can answer is the same thing the case above answers
    // for the upload: whether a browser will let a page on ANOTHER origin
    // present `X-AO-Session` to these two routes and read megabytes back.
    // Everything else — the id rule, the zip/manifest agreement, the
    // refusal without a session — is proved in `internal/bundle` and
    // `internal/transport/bundleroutes_test.go`, against no browser at
    // all.
    //
    // The page is `bundle-id.txt`, which is a real file in the served
    // tree rather than the SPA: this test wants an ORIGIN to fetch from,
    // not a running app, and that file is also the fact being compared —
    // what a phone booted on this bundle would answer when asked what it
    // is running (`frontend/scripts/bundleId.ts`).
    //
    // The device pairs over the wire from here rather than through the
    // screen, because what is under test is two GETs and not a second
    // copy of the pairing flow.
    const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'phone', 'full');
    const payload = JSON.parse(
      Buffer.from(fragmentOf(invite).slice('#pair='.length), 'base64url').toString('utf8'),
    ) as { token: string };
    const deviceKey = 'e2e-bundle-reader-device';
    const redeemed = await fetch(backendOrigin + '/auth/pair', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        token: payload.token,
        keyThumbprint: deviceKey,
        label: 'Bundle reader',
        platform: 'playwright',
      }),
    });
    expect(redeemed.ok, '/auth/pair must answer a redemption naming a live link').toBe(true);
    const grant = (await redeemed.json()) as { credential: string; pairingId: string };
    await harness.rpc('ConfirmDevicePairing', grant.pairingId);

    const context = await browser.newContext();
    const page = await context.newPage();
    // An init script rather than a script tag: it runs in the page's own
    // world before the document loads, so it does not depend on the
    // document having a head to append to. The UMD build assigns
    // `self.fflate` when there is no module system, which is the page.
    await page.addInitScript({
      content: await readFile(
        path.resolve(import.meta.dirname, '..', 'node_modules', 'fflate', 'umd', 'index.js'),
        'utf8',
      ),
    });
    // The CORS answer itself is read from the network side, not from the
    // page: `Access-Control-Allow-Origin` is not a safelisted response
    // header, so `Response.headers.get` in a page answers '' for it no
    // matter what crossed. That the fetches SUCCEED is the browser's own
    // verdict on the answer; this is how the answer's exact value —
    // never `*` — is checked.
    const allowOrigins = new Map<string, string>();
    page.on('response', (res) => {
      if (!res.url().startsWith(backendOrigin)) return;
      allowOrigins.set(
        new URL(res.url()).pathname,
        res.headers()['access-control-allow-origin'] ?? '',
      );
    });
    await page.goto(bundle.origin + '/bundle-id.txt');
    const servedId = ((await page.locator('body').textContent()) ?? '').trim();
    expect(servedId, 'the built bundle must record its own id').toMatch(/^[0-9a-f]{64}$/);

    // Both fetches, the unzip and every digest happen inside the page:
    // the point is that the BROWSER allowed them.
    const read = await page.evaluate(
      async ({ origin, credential, key }) => {
        const headers = { 'X-AO-Session': credential, 'X-AO-Device-Key': key };
        const withoutSession = await fetch(origin + '/bundle/manifest.json');
        const manifestRes = await fetch(origin + '/bundle/manifest.json', { headers });
        const manifest = (await manifestRes.json()) as {
          id: string;
          version: string;
          minShellBuild: number;
          files: Array<{ path: string; sha256: string; size: number }>;
        };
        const archiveRes = await fetch(origin + '/bundle/archive.zip', { headers });
        const archive = new Uint8Array(await archiveRes.arrayBuffer());
        const unzip = (window as unknown as {
          fflate: { unzipSync: (data: Uint8Array) => Record<string, Uint8Array> };
        }).fflate.unzipSync;
        const entries = unzip(archive);
        const hex = async (bytes: Uint8Array): Promise<string> => {
          const digest = await crypto.subtle.digest('SHA-256', bytes as BufferSource);
          return [...new Uint8Array(digest)]
            .map((b) => b.toString(16).padStart(2, '0'))
            .join('');
        };
        const unpacked: Array<{ path: string; sha256: string; size: number }> = [];
        for (const [entryPath, bytes] of Object.entries(entries)) {
          unpacked.push({ path: entryPath, sha256: await hex(bytes), size: bytes.length });
        }
        return {
          refusedStatus: withoutSession.status,
          manifestStatus: manifestRes.status,
          archiveStatus: archiveRes.status,
          archiveType: archiveRes.headers.get('content-type') ?? '',
          archiveCache: archiveRes.headers.get('cache-control') ?? '',
          archiveBytes: archive.length,
          manifest,
          unpacked,
        };
      },
      { origin: backendOrigin, credential: grant.credential, key: deviceKey },
    );

    // A page on the shell's origin with no session is refused, and told
    // nothing about whether the path exists.
    expect(read.refusedStatus, 'the bundle routes admit a paired session and nothing else').toBe(404);

    // Both answers were readable, and named this origin exactly. A `*`
    // would satisfy the browser and is what this backend must not answer.
    expect(read.manifestStatus).toBe(200);
    expect(read.archiveStatus).toBe(200);
    expect(read.archiveType).toBe('application/zip');
    expect(allowOrigins.get('/bundle/manifest.json')).toBe(bundle.origin);
    expect(allowOrigins.get('/bundle/archive.zip')).toBe(bundle.origin);
    expect(read.archiveCache).toBe('no-store');
    expect(read.archiveBytes).toBeGreaterThan(0);

    // The manifest describes the bundle this very page was served from.
    // That is the Go rule and the Vite rule agreeing over the real tree,
    // end to end, and it is why there is no third implementation of the
    // hash in this file.
    expect(read.manifest.id, 'the served manifest must describe the served bundle').toBe(servedId);
    expect(read.manifest.files.length).toBeGreaterThan(0);
    expect(read.manifest.minShellBuild).toBeGreaterThan(0);

    // Every manifest file arrived, with the bytes the manifest promised,
    // and nothing arrived that the manifest did not name — which is the
    // whole of what `stage()` will check again on the device.
    const byPath = new Map(read.unpacked.map((f) => [f.path, f]));
    for (const file of read.manifest.files) {
      const got = byPath.get(file.path);
      expect(got, `the archive must carry ${file.path}`).toBeTruthy();
      expect(got!.sha256, `${file.path} must arrive with the digest the manifest names`)
        .toBe(file.sha256);
      expect(got!.size, `${file.path} must arrive at the size the manifest names`).toBe(file.size);
    }
    expect(
      read.unpacked.length,
      'the archive must carry the manifest and nothing else',
    ).toBe(read.manifest.files.length);

    // The one file a bundle never carries: it is written after the tree
    // is hashed, so an archive containing it would describe a tree the
    // build never produced.
    expect(byPath.has('bundle-id.txt')).toBe(false);
    expect(read.unpacked.some((f) => f.path.endsWith('.map'))).toBe(false);

    await context.close();
  });

  test('a page origin the backend was not told about is refused', async ({ browser }) => {
    // The negative that keeps the case above meaningful. Same bundle,
    // same backend, an endpoint this backend never admitted — the
    // manifest fetch is blocked by the browser and the app never boots.
    // Without this, a backend that answered `*` would pass everything.
    const context = await browser.newContext();
    const page = await context.newPage();
    const refused: string[] = [];
    page.on('requestfailed', (req) => {
      const why = req.failure()?.errorText ?? 'failed';
      // Same exclusion as the case above, and here it matters more: a
      // cancelled request would let this pass without the browser having
      // refused anything.
      if (!req.url().startsWith(backendOrigin) || why === 'net::ERR_ABORTED') return;
      refused.push(`${req.url()}: ${why}`);
    });
    await page.addInitScript((endpoint) => {
      (window as unknown as { __aoHomeEndpoint: string }).__aoHomeEndpoint = endpoint;
    }, backendOrigin);

    // 127.0.0.1 and localhost are different ORIGINS to a browser while
    // naming one machine, which is exactly the distinction the check is
    // about — and it keeps the request reaching a listener that is
    // actually there, so what fails is the CORS answer and not the
    // connection.
    const foreign = bundle.origin.replace('127.0.0.1', 'localhost');
    await page.goto(foreign + '/');

    await expect
      .poll(() => refused.filter((r) => r.includes('/bootstrap.json')).length, {
        message: 'a page origin this backend does not serve must not be able to read its manifest',
        timeout: 15_000,
      })
      .toBeGreaterThan(0);

    await context.close();
  });
});
