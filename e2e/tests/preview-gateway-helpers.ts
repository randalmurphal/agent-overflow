// The dev-server preview gateway, end to end, from the phone's screen to
// the dev server's own request log (docs/specs/remote-access.md §7, the
// port gateway).
//
// WHY THIS FILE EXISTS. Wave 9 gave an agent's `http://localhost:5173`
// somewhere real to point when the person reading it is not on the
// machine that printed it. Every part of that is a seam between two
// halves that cannot see each other: the markdown rewrite decides a link
// is inert from a list the backend pushed, the Allow button edits a
// setting on another machine, the minted URL is spent by a browser the
// SPA has no handle on, and the bytes that come back were written by a
// process this app did not start. Unit tests cover each half. What only
// the whole chain can answer is whether the sentence on the phone matches
// what the listener does, and whether what the dev server RECEIVED is
// what the person's browser sent.
//
// So every crossing assertion here is read off the fake dev server's own
// record of the request, never off what the UI implies. A green screen
// with a `Host` the upstream would have refused is exactly the failure
// this suite exists to catch.
//
// THE FAKE DEV SERVER IS A LISTENER IN THIS PROCESS, and that is what
// makes the flow real rather than staged. The harness backend runs on
// this same machine, so `internal/devscan`'s /proc walk finds the
// listener on its own — it is not injected, and no scanner is faked. It
// belongs to the Playwright node process, which is nothing the backend
// spawned, so nothing attributes it to a thread: it is a `seen`
// candidate, which is precisely the state the "not shared / Allow port"
// affordance exists for. Allowing it by hand is the flow the suite is
// about.
//
// WHY IT OWNS ITS BACKEND. The LAN bind persists to the settings file and
// REBINDS the listener, and so does the preview port set; `harness.reset()`
// undoes neither. Borrowing the worker fixture's instance would hand the
// next spec a LAN-bound backend holding somebody else's port open.
//
// WHY IT IS A MODULE AND NOT A SPEC. Compact is a layout mode of the one
// app, so the gateway is done only when both projects pass. The two spec
// files are the same suite under two surfaces; where the layouts genuinely
// differ — how a thread is opened, how Settings is reached — the surface
// is a PARAMETER (`PreviewSurface`), never a fork in the flow.

import { createServer, type Server } from 'node:http';
import { connect } from 'node:net';
import { hostname } from 'node:os';
import {
  expect,
  request,
  test,
  type BrowserContext,
  type Locator,
  type Page,
} from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  PAIRED_APP_MOUNT_MS,
  confirmOnHost,
  instrument,
  mintInvite,
  nonLoopbackIPv4,
  redeemOnScreen,
  solePairedDevice,
  type DeviceRevocationResult,
  type Surfaced,
} from './offhost-helpers.js';
import {
  RESULT_LINE,
  claudeScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  textLines,
  toolResultLine,
  toolUseLine,
  type Item,
} from './agent-visibility-helpers.js';

// ---------------------------------------------------------------------
// The fake dev server.
// ---------------------------------------------------------------------

/** One request the dev server actually received, as it received it. */
interface DevServerHit {
  /** The raw request target, byte for byte. */
  url: string;
  host: string | null;
  origin: string | null;
  cookie: string | null;
}

interface FakeDevServer {
  /** The loopback port it bound, which is also the preview listener's. */
  readonly port: number;
  /** Every non-discovery request since the last reset, in arrival order. */
  readonly hits: DevServerHit[];
  reset(): void;
  close(): Promise<void>;
}

/**
 * A dev server on loopback, answering the four things this suite asks of
 * one and recording what it saw.
 *
 * The content type on `/` is load-bearing: `internal/devscan`'s probe
 * counts a 2xx only with a document type, so a JSON answer here would be
 * a port the machine never offers.
 */
async function startFakeDevServer(): Promise<FakeDevServer> {
  const hits: DevServerHit[] = [];
  let port = 0;
  const server: Server = createServer((req, res) => {
    const hit: DevServerHit = {
      url: req.url ?? '',
      host: req.headers.host ?? null,
      origin: req.headers.origin ?? null,
      cookie: req.headers.cookie ?? null,
    };
    const path = (req.url ?? '').split('?', 1)[0];
    // Reopening the app legitimately triggers discovery while a later case
    // asserts browser traffic. Exclude only Go's probe at /; a proxied browser
    // request keeps its own User-Agent and must still be recorded at any path.
    if (!(path === '/' && req.headers['user-agent'] === 'Go-http-client/1.1')) hits.push(hit);
    if (path === '/' || path === '/app/') {
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
      res.end('<!doctype html><title>dev</title><h1>dev</h1>');
      return;
    }
    if (path === '/echo') {
      res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8' });
      res.end(JSON.stringify(hit));
      return;
    }
    if (path === '/go') {
      // Absolute and naming the upstream, which is what a dev server
      // serving its app under a base path answers at `/`. The proxy has
      // to rewrite it or the browser is sent to a localhost that is a
      // different machine.
      res.writeHead(302, { Location: `http://localhost:${port}/app/` });
      res.end();
      return;
    }
    res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' });
    res.end('no such route\n');
  });

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('the fake dev server did not bind a TCP port');
  }
  port = address.port;

  return {
    port,
    hits,
    reset: () => {
      hits.length = 0;
    },
    close: async () => {
      // Keep-alive sockets the proxy pooled outlive `close()` on their
      // own, and the suite's afterAll would then hang on them.
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

/**
 * Whether anything is accepting on `host:port`, answered by a real dial.
 *
 * A retired preview listener is a REFUSED connection, not a slow one, and
 * that difference is the assertion: a timeout would pass just as happily
 * against a listener that was merely busy.
 */
async function dialOutcome(host: string, port: number): Promise<string> {
  return new Promise<string>((resolve) => {
    const socket = connect({ host, port });
    socket.setTimeout(5_000);
    const settle = (outcome: string) => {
      socket.destroy();
      resolve(outcome);
    };
    socket.once('connect', () => settle('connected'));
    socket.once('timeout', () => settle('timeout'));
    socket.once('error', (err: NodeJS.ErrnoException) => settle(err.code ?? 'error'));
  });
}

// ---------------------------------------------------------------------
// The turn the transcript is made of.
// ---------------------------------------------------------------------

const PROJECT_NAME = 'preview-gateway';
const THREAD_TITLE = 'Preview gateway';

/** The path the prose link names, query included. */
const PROSE_PATH = '/app/?tab=2';

/**
 * One turn carrying both surfaces the rewrite has to reach: a Bash row
 * whose output announces the dev server (which is what
 * `internal/triage/dev_server_url.go` turns into the row's
 * `devServerUrl`, and so into the chip), and assistant prose with a
 * markdown link to a path on the same port.
 *
 * The banner is spelled the way Vite spells it, with the URL preceded by
 * whitespace: triage refuses a URL sitting directly against `(`, `"` or
 * `=`, because that shape is a reference rather than an announcement.
 */
function previewTurnScenario(port: number): unknown {
  const banner = `\n  VITE ready in 214 ms\n\n  ➜  Local:   http://localhost:${port}/\n`;
  const prose = `The dev server is up. Open [the app](http://localhost:${port}${PROSE_PATH}) when you want to look.`;
  return claudeScenario('preview-gateway', [
    emit([
      toolUseLine('msg-run', 'tu-run', 'Bash', { command: 'npm run dev' }),
      toolResultLine('tu-run', banner),
      ...textLines('msg-prose', prose),
      RESULT_LINE,
    ]),
  ]);
}

// ---------------------------------------------------------------------
// What differs between the two layouts, and nothing else.
// ---------------------------------------------------------------------

/**
 * The layout-dependent moves, so the flow below has none.
 *
 * Settings is an OVERLAY in both layouts (a sibling of the pane host, not
 * a surface that replaces it), so closing it reveals whatever was
 * underneath. What differs is reaching it: the compact settings button
 * lives in the sidebar, and the sidebar is inert while the thread screen
 * is showing.
 */
export interface PreviewSurface {
  /** Names the project this surface belongs to, for the suite title. */
  readonly name: string;
  /** Show the thread whose transcript the cases read. */
  openThread(page: Page, title: string): Promise<void>;
  /** Open Settings on the Remote access page. */
  openSettingsRemote(page: Page): Promise<void>;
  /** Close Settings and come back to the thread. */
  returnToThread(page: Page, title: string): Promise<void>;
}

/**
 * Mount the Bash row.
 *
 * A completed turn's tool rows are folded into a COLLAPSED activity run,
 * so the command row and its chip are not in the document until the run is
 * opened. Every chip assertion in this suite goes through here first,
 * including the ones asserting the chip is ABSENT: an absence read off an
 * unmounted row is an assertion about nothing.
 */
async function openCommandRow(page: Page): Promise<void> {
  if ((await page.getByTestId('command-output-row').count()) === 0) {
    await expect(page.getByTestId('activity-run-header')).toHaveCount(1);
    await page.getByTestId('activity-run-header').click();
  }
  await expect(page.getByTestId('command-output-row')).toHaveCount(1);
}

async function openRemoteAccessPage(page: Page): Promise<void> {
  await page.getByTestId('sidebar-settings-button').click();
  await expect(page.getByRole('tablist', { name: 'Settings Sections' })).toBeVisible();
  await page.getByRole('tab', { name: 'Computers', exact: true }).click();
  await page.getByTestId('home-computer').getByRole('button', { name: 'Access & sharing', exact: true }).click();
  // Selecting drills into the page; on compact the rail (and its tab)
  // leaves the screen, so the arrival assertion is the page header.
  await expect(page.getByTestId('settings-page-header')).toContainText('Access & sharing');
  await page.getByText('Advanced network settings', { exact: true }).click();
}

async function closeSettings(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Close Settings' }).click();
  await expect(page.getByTestId('settings-overlay')).toHaveCount(0);
}

export const DESKTOP_SURFACE: PreviewSurface = {
  name: 'desktop',
  async openThread(page, title) {
    await page.getByTestId('thread-row').filter({ hasText: title }).click();
    await expect(page.getByTestId('chat-header-title')).toHaveText(title);
  },
  openSettingsRemote: openRemoteAccessPage,
  async returnToThread(page, title) {
    await closeSettings(page);
    await expect(page.getByTestId('chat-header-title')).toHaveText(title);
  },
};

export const COMPACT_SURFACE: PreviewSurface = {
  name: 'compact',
  async openThread(page, title) {
    // The thread screen and the list screen are one mounted tree with a
    // visibility flip, and a page that boots into an already-open pane
    // arrives on the thread screen with no row to click.
    if ((await page.locator('html').getAttribute('data-compact-screen')) !== 'thread') {
      await page.getByTestId('thread-row').filter({ hasText: title }).click();
    }
    await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
    await expect(page.getByTestId('chat-header-title')).toHaveText(title);
  },
  async openSettingsRemote(page) {
    // The settings button lives in the sidebar, which is inert while the
    // thread screen is showing.
    if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
      await page.getByTestId('compact-back').click();
      await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
    }
    await openRemoteAccessPage(page);
  },
  async returnToThread(page, title) {
    await closeSettings(page);
    await COMPACT_SURFACE.openThread(page, title);
  },
};

// ---------------------------------------------------------------------
// Page instrumentation this suite adds on top of `instrument`.
// ---------------------------------------------------------------------

/** What the page tried to open, and what its clicks did. */
interface Opened {
  /** Every URL `window.open` was called with, in order. */
  urls: string[];
  /** Every click that reached `window` having been preventDefault'd. */
  swallowed(page: Page): Promise<number>;
}

/**
 * Stub `window.open` into a ledger kept in NODE, so a navigation cannot
 * lose what an earlier document tried to open — the same reason
 * `instrument` collects toasts that way. The click counter stays on the
 * page, because it is only ever read within one document.
 */
export async function recordOpens(page: Page): Promise<Opened> {
  const urls: string[] = [];
  await page.exposeFunction('__aoRecordWindowOpen', (url: string) => {
    urls.push(url);
  });
  await page.addInitScript(() => {
    const win = window as unknown as {
      __aoRecordWindowOpen?: (url: string) => void;
      __aoSwallowedClicks?: number;
    };
    win.__aoSwallowedClicks = 0;
    // `window` is the last hop of the bubble path, so the app's own
    // document-level delegate has already run and its preventDefault is
    // visible here. That is the settled fact a swallowed click leaves.
    window.addEventListener('click', (event) => {
      if (event.defaultPrevented) win.__aoSwallowedClicks = (win.__aoSwallowedClicks ?? 0) + 1;
    });
    window.open = ((url?: string | URL) => {
      win.__aoRecordWindowOpen?.(String(url ?? ''));
      return null;
    }) as typeof window.open;
  });
  return {
    urls,
    swallowed: (target: Page) =>
      target.evaluate(
        () => (window as unknown as { __aoSwallowedClicks?: number }).__aoSwallowedClicks ?? 0,
      ),
  };
}

// ---------------------------------------------------------------------
// The suite.
// ---------------------------------------------------------------------

const lanIP = nonLoopbackIPv4();

/** The label the phone enrolls under, carried across the whole flow. */
const PHONE_LABEL = 'Preview phone';

/** The refusal a request with no live grant gets, verbatim. */
const PREVIEW_ENDED = 'This preview session ended. Open the link again from Agent Overflow.';

/**
 * The `CommandOutputMeta` triage wrote for one row. It rides the heavy
 * PAYLOAD rather than the item, which is also where `CommandOutput.svelte`
 * reads it from.
 */
function commandMetaOf(item: Item): { devServerUrl?: string } {
  try {
    return JSON.parse(item.payloadMeta ?? '{}') as { devServerUrl?: string };
  } catch {
    return {};
  }
}

/**
 * Register the whole flow for one surface. Both spec files are a call to
 * this and nothing else, so the two projects prove the same thing.
 */
export function definePreviewGatewaySuite(surface: PreviewSurface): void {
  test.describe.serial(`dev-server preview gateway (${surface.name})`, () => {
    // Not green-washed: a host with no non-loopback interface genuinely
    // has no preview address, and the whole gateway is about serving one.
    // A skip is visible in the report; a vacuous pass is not.
    test.skip(
      lanIP === null,
      'no non-loopback IPv4 interface on this host, so this machine has no preview address',
    );

    let harness: HarnessApp;
    let devServer: FakeDevServer;
    let phoneContext: BrowserContext;
    let phone: Page;
    let surfaced: Surfaced;
    let opened: Opened;
    let threadId = '';
    /** The URL the anchor's click minted, carried from case 3 into case 4. */
    let mintedURL = '';
    /** The cookie the ticket bought, as `name=value`. */
    let grantCookie = '';

    /** The preview anchor the markdown rewrite produced. */
    const anchor = (): Locator => phone.locator('a[data-preview-port]');

    test.beforeAll(async ({ browser }) => {
      devServer = await startFakeDevServer();
      harness = await launchHarness();

      // The production LAN-bind path: it rebinds and installs the WS
      // origin allow-list in one step, and it is also what gives this
      // machine a preview address at all.
      const network = await harness.rpc<{ bindAll: boolean }>('SetNetworkSettings', {
        bindAll: true,
      });
      expect(network.bindAll).toBe(true);

      // One turn, run over the wire before anybody is watching: the
      // transcript is persisted history, so nothing here depends on a
      // live tick.
      await harness.rpc('HarnessSetScenario', { scenario: previewTurnScenario(devServer.port) });
      threadId = await seedAgentThread(harness, PROJECT_NAME, THREAD_TITLE);
      await startMock(harness, threadId);
      await harness.rpc('SendMessage', threadId, 'start the dev server', null);
      await harness.waitForEvent('provider:turn_completed');

      // The fixture's own precondition. The chip is gated on the row's
      // `devServerUrl`, which triage derives from the banner and writes to
      // the command_output PAYLOAD's meta, not the item's: a fixture whose
      // banner stopped parsing would leave three cases asserting nothing.
      const items: Item[] = await listItems(harness, threadId);
      const bashRows = items.filter((item) => item.toolName === 'Bash');
      expect(bashRows, 'the turn must leave a Bash row for the chip to hang on').not.toEqual([]);
      expect(
        bashRows.map((item) => commandMetaOf(item).devServerUrl).filter((url) => url !== undefined),
        'triage must have read the dev-server banner out of the command output',
      ).toEqual([`http://localhost:${devServer.port}/`]);

      phoneContext = await browser.newContext();
      phone = await phoneContext.newPage();
      surfaced = await instrument(phone);
      opened = await recordOpens(phone);

      // A full-access browser device, paired through the real screen.
      const invite = await mintInvite(harness, 'full');
      const shown = await redeemOnScreen(phone, invite, PHONE_LABEL);
      await confirmOnHost(harness, shown);
      await expect(phone.getByTestId('thread-row')).toHaveCount(1, {
        timeout: PAIRED_APP_MOUNT_MS,
      });
    });

    test.afterAll(async () => {
      await phoneContext?.close();
      // Leave the instance as we found it: both of these persist to the
      // settings file, which outlives the listener.
      await harness
        ?.rpc('DisallowPreviewPort', devServer?.port ?? 0)
        .catch(() => undefined);
      await harness?.rpc('SetNetworkSettings', { bindAll: false }).catch(() => undefined);
      await harness?.close();
      await devServer?.close();
    });

    // -----------------------------------------------------------------
    // 1. The transcript on the phone.
    // -----------------------------------------------------------------
    test('a localhost link on another machine renders inert, names that machine, and swallows its click', async () => {
      await surface.openThread(phone, THREAD_TITLE);
      await openCommandRow(phone);

      const link = anchor();
      await expect(link).toHaveCount(1);
      await expect(link).toHaveAttribute('data-preview-state', 'not-shared');
      await expect(link).toHaveAttribute('data-preview-port', String(devServer.port));
      await expect(link).toHaveAttribute('data-preview-path', PROSE_PATH);
      await expect(link).toHaveAttribute('data-preview-thread', threadId);
      // The machine is named from the hello frame's `backendName`, which
      // is this host's own name (internal/appidentity).
      await expect(link).toHaveAttribute('data-preview-machine', hostname());
      await expect(link).toHaveAttribute('data-preview-via', `via ${hostname()}`);
      // The href stays what the agent wrote, so copying it says what the
      // agent said.
      await expect(link).toHaveAttribute(
        'href',
        `http://localhost:${devServer.port}${PROSE_PATH}`,
      );

      // The inline action beside it, which case 2 presses.
      await expect(
        phone.locator(`button[data-preview-allow="${devServer.port}"]`),
      ).toHaveCount(1);

      // Not shared is not the same as not reachable: the machine has an
      // address, it just is not sharing this port.
      await expect(phone.getByTestId('dev-server-chip')).toHaveCount(0);

      const swallowedBefore = await opened.swallowed(phone);
      await link.click();
      // The click reaching `window` preventDefault'd is the settled fact
      // that the delegate ran and decided. Everything the open path does
      // is issued synchronously inside that same dispatch, so a mint that
      // was going to happen has already been sent by the time this holds.
      await expect
        .poll(() => opened.swallowed(phone), {
          message: 'a preview anchor must swallow its click in every state',
        })
        .toBe(swallowedBefore + 1);
      expect(
        surfaced.rpcReplies.length,
        'the wire capture must have observed this session, or the emptiness below means nothing',
      ).toBeGreaterThan(5);
      expect(
        surfaced.rpcReplies.filter((method) => method === 'MintPreviewURL'),
        'a link that is not shared must not mint anything',
      ).toEqual([]);
      expect(opened.urls, 'a link that is not shared must open nothing').toEqual([]);
    });

    // -----------------------------------------------------------------
    // 2. Allow, from the inline action.
    // -----------------------------------------------------------------
    test('allowing the port from the transcript makes the link live, the chip appear, and the setting visible', async () => {
      await phone.locator(`button[data-preview-allow="${devServer.port}"]`).click();

      // The next `devserver:list` frame is the answer; nothing is applied
      // optimistically, because the machine decides whether it can open a
      // listener on that port at all.
      await expect(anchor()).toHaveAttribute('data-preview-state', 'open', {
        timeout: PAIRED_APP_MOUNT_MS,
      });

      // Which is also the settle that closes case 1: an allow round trip
      // is the same delegate, the same socket and the same store as a
      // mint, plus a push frame. A mint the swallowed click had issued
      // would have been answered well inside it.
      expect(
        opened.urls,
        'the click on the inert link must still have opened nothing',
      ).toEqual([]);

      // The command row's chip reads the machine's own list off host, and
      // says whose machine it is.
      await openCommandRow(phone);
      const chip = phone.getByTestId('dev-server-chip');
      await expect(chip).toHaveCount(1);
      await expect(chip).toHaveAttribute('data-machine', hostname());
      await expect(chip).toHaveAttribute('data-url', `http://localhost:${devServer.port}/`);

      // And the same port is now a row in Settings, with the one control
      // that can take it back out of the persisted set.
      await surface.openSettingsRemote(phone);
      await expect(phone.getByTestId('preview-ports-no-address')).toHaveCount(0);
      const row = phone.getByTestId('preview-port-row').filter({ hasText: String(devServer.port) });
      await expect(row).toHaveCount(1);
      await expect(row.getByTestId('preview-port-remove')).toBeVisible();
      await surface.returnToThread(phone, THREAD_TITLE);
    });

    // -----------------------------------------------------------------
    // 3. Opening it, and what the dev server then received.
    // -----------------------------------------------------------------
    test('the click mints a single-use URL, and the proxy hands the dev server the request the browser made', async () => {
      devServer.reset();
      opened.urls.length = 0;
      await anchor().click();
      await expect
        .poll(() => opened.urls.length, { message: 'an open preview link must open a URL' })
        .toBe(1);

      mintedURL = opened.urls[0];
      const minted = new URL(mintedURL);
      expect(minted.protocol, 'the preview origin is TLS on every path').toBe('https:');
      expect(minted.host).toBe(`${lanIP}:${devServer.port}`);
      expect(minted.pathname).toBe('/app/');
      expect(minted.searchParams.get('tab'), 'the query is carried through the mint').toBe('2');
      expect(minted.searchParams.getAll('ao_preview')).toHaveLength(1);
      expect([...minted.searchParams.keys()].sort()).toEqual(['ao_preview', 'tab']);

      // The browser half, driven for real. The certificate is this
      // install's own self-signed one, which nothing can verify.
      const viewer = await request.newContext({ ignoreHTTPSErrors: true });
      const spender = await request.newContext({ ignoreHTTPSErrors: true });
      const appURL = phone.url();
      try {
        const exchange = await viewer.get(mintedURL, { maxRedirects: 0 });
        expect(exchange.status(), 'the first hit spends the ticket').toBe(302);
        expect(
          exchange.headers()['location'],
          'the redirect drops the ticket and keeps the path and query',
        ).toBe(PROSE_PATH);
        const setCookies = exchange
          .headersArray()
          .filter((header) => header.name.toLowerCase() === 'set-cookie')
          .map((header) => header.value);
        expect(setCookies).toHaveLength(1);
        const cookie = setCookies[0];
        expect(cookie).toMatch(new RegExp(`^ao_preview_${devServer.port}=[^;]+;`));
        expect(cookie).toContain('HttpOnly');
        expect(cookie).toContain('Secure');
        expect(cookie).toContain('SameSite=Strict');
        grantCookie = cookie.split(';', 1)[0];
        expect(
          devServer.hits,
          "the ticket exchange is the gateway's own answer and never reaches the dev server",
        ).toEqual([]);

        // An external preview has its own browser grant. Let the app's last
        // remote connection disappear across a complete 3s discovery tick;
        // the request below must still reach the dev server. Fake-clock unit
        // tests cover the ticket/grant expiry boundaries without this delay.
        await phone.goto('about:blank');
        await phone.waitForTimeout(4_000);

        // The path and query, byte for byte, and the Host the upstream
        // insists on. `%2F` stays encoded: a re-encoded path is an HMR
        // upgrade that hangs with no response at all.
        const echo = await viewer.get(
          `https://${lanIP}:${devServer.port}/echo?x=1&y=%2Fz`,
          { headers: { Cookie: grantCookie } },
        );
        expect(echo.status()).toBe(200);
        const seen = (await echo.json()) as DevServerHit;
        expect(seen.url).toBe('/echo?x=1&y=%2Fz');
        expect(seen.host).toBe(`localhost:${devServer.port}`);
        expect(
          seen.cookie ?? '',
          'the preview cookie is the credential for this gateway and has no business past it',
        ).not.toContain('ao_preview_');

        // Origin is rewritten to the upstream's own, never stripped: its
        // presence is what keeps the HMR token check mandatory.
        const withOrigin = await viewer.get(`https://${lanIP}:${devServer.port}/echo`, {
          headers: { Cookie: grantCookie, Origin: `https://${lanIP}:${devServer.port}` },
        });
        expect(withOrigin.status()).toBe(200);
        expect(((await withOrigin.json()) as DevServerHit).origin).toBe(
          `http://localhost:${devServer.port}`,
        );

        // A Location naming the upstream comes back relative, so it
        // resolves against whichever address the browser used.
        const redirected = await viewer.get(`https://${lanIP}:${devServer.port}/go`, {
          headers: { Cookie: grantCookie },
          maxRedirects: 0,
        });
        expect(redirected.status()).toBe(302);
        expect(redirected.headers()['location']).toBe('/app/');

        // Single use. A context that never held the cookie presents the
        // same minted URL and gets the one plain page.
        const replayed = await spender.get(mintedURL, { maxRedirects: 0 });
        expect(replayed.status(), 'a spent ticket buys nothing').toBe(401);
        expect(await replayed.text()).toContain(PREVIEW_ENDED);

        // Every crossing above is asserted on what the dev server
        // recorded, so the record itself has to be non-empty.
        expect(
          devServer.hits.map((hit) => hit.url),
          'the dev server must have seen exactly the requests that were forwarded',
        ).toEqual(['/echo?x=1&y=%2Fz', '/echo', '/go']);
      } finally {
        await viewer.dispose();
        await spender.dispose();
        await phone.goto(appURL);
        await expect(phone.getByTestId('thread-row')).toHaveCount(1, {
          timeout: PAIRED_APP_MOUNT_MS,
        });
        await surface.openThread(phone, THREAD_TITLE);
      }
    });

    // -----------------------------------------------------------------
    // 4. Revoking the device ends its previews on the next request.
    // -----------------------------------------------------------------
    test('revoking the device ends the preview it was holding', async () => {
      const device = await solePairedDevice(harness);
      const revoked = await harness.rpc<DeviceRevocationResult>('RevokeAccessDevice', device.id);
      expect(revoked.deviceMoved, 'the device must have been live to revoke').toBe(true);

      devServer.reset();
      const viewer = await request.newContext({ ignoreHTTPSErrors: true });
      try {
        const refused = await viewer.get(`https://${lanIP}:${devServer.port}/echo`, {
          headers: { Cookie: grantCookie },
        });
        expect(
          refused.status(),
          'the principal is re-checked on every request, never cached',
        ).toBe(401);
        expect(await refused.text()).toContain(PREVIEW_ENDED);
      } finally {
        await viewer.dispose();
      }
      expect(
        devServer.hits,
        'a request the gateway refused must never reach the dev server',
      ).toEqual([]);

      // Restore and re-pair the SAME context: the stored key thumbprint
      // adopts the existing row, which is the live phone the last case
      // needs.
      await harness.rpc('RestoreAccessDevice', device.id);
      const invite = await mintInvite(harness, 'full');
      const shown = await redeemOnScreen(phone, invite, PHONE_LABEL);
      await confirmOnHost(harness, shown);
      await expect(phone.getByTestId('thread-row')).toHaveCount(1, {
        timeout: PAIRED_APP_MOUNT_MS,
      });
    });

    // -----------------------------------------------------------------
    // 5. Stop sharing takes the listener down.
    // -----------------------------------------------------------------
    test('stopping the share makes the link inert again and closes the listener', async () => {
      await surface.openThread(phone, THREAD_TITLE);
      await expect(anchor()).toHaveAttribute('data-preview-state', 'open', {
        timeout: PAIRED_APP_MOUNT_MS,
      });
      expect(
        await dialOutcome(lanIP!, devServer.port),
        'the precondition: the preview listener is up before it is taken down',
      ).toBe('connected');

      await surface.openSettingsRemote(phone);
      await phone
        .getByTestId('preview-port-row')
        .filter({ hasText: String(devServer.port) })
        .getByTestId('preview-port-remove')
        .click();
      await expect(
        phone.getByTestId('preview-port-row').filter({ hasText: String(devServer.port) }),
      ).toHaveCount(0);
      await surface.returnToThread(phone, THREAD_TITLE);

      await expect(anchor()).toHaveAttribute('data-preview-state', 'not-shared', {
        timeout: PAIRED_APP_MOUNT_MS,
      });
      await openCommandRow(phone);
      await expect(phone.getByTestId('dev-server-chip')).toHaveCount(0);
      // Refused, not slow: the listener is gone, and a timeout here would
      // pass just as happily against one that was merely busy.
      expect(await dialOutcome(lanIP!, devServer.port)).toBe('ECONNREFUSED');
    });

    // -----------------------------------------------------------------
    // 6. The owner's own screen, where localhost means what it says.
    // -----------------------------------------------------------------
    test('on the machine itself the link is left plain and the chip names no machine', async ({
      browser,
    }) => {
      const host = await browser.newPage();
      try {
        await harness.open(host);
        // Through the surface, because every page this backend hands out
        // shares one ui_state bucket: this one boots into the pane the
        // phone opened, which under compact is already the thread screen
        // with no row left to click.
        await surface.openThread(host, THREAD_TITLE);

        const link = host.locator(`a[href="http://localhost:${devServer.port}${PROSE_PATH}"]`);
        await expect(link).toHaveCount(1);
        // The rewrite is off entirely here, rather than resolving to
        // `open`: this page holds `host` on the thread's own machine, so
        // `localhost` already means what it says.
        await expect(link).not.toHaveAttribute('data-preview-state', /.*/);
        await expect(host.locator('a[data-preview-port]')).toHaveCount(0);
        await expect(host.locator('[data-preview-allow]')).toHaveCount(0);

        // The chip is the loopback probe's here, and the probe reaches
        // the dev server this process is running.
        await openCommandRow(host);
        const chip = host.getByTestId('dev-server-chip');
        await expect(chip).toHaveCount(1);
        await expect(chip).not.toHaveAttribute('data-machine', /.*/);
      } finally {
        await host.close();
      }
    });
  });
}
