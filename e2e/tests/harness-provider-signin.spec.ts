// Signing a provider account in FROM a paired device, which is the whole
// point of the flow: the person holding the phone is the person who can
// finish the sign-in, and the machine running the backend may have no
// browser to open at all.
//
// WHY THIS FILE EXISTS. Every earlier version of this surface was one
// blocking RPC that opened a browser on the BACKEND'S machine and waited.
// From a phone that is not a degraded experience, it is an impossible one:
// the link lands on a screen nobody is looking at, and the call times out
// before anyone could have finished. Nothing about that is visible from a
// unit test — the store can be driven to every state by hand — so the
// regression guard has to be a real remote page against a real backend.
//
// WHAT IS REAL HERE. The page is the shipped app on a LAN origin, so
// `canUseHostOpenExternalURL()` is false and the client asks for the
// REMOTE method by itself; nothing in the spec selects it. The RPCs are
// the shipped `StartProviderLogin` / `SubmitProviderLoginCode`, the state
// arrives on the shipped `provider:login` channel, and the provider
// processes are `ao-mockprovider` speaking both sign-in wires
// (`cmd/ao-mockprovider/login.go`, `codex_login.go`).
//
// WHAT IS NOT, and cannot be: a real OAuth completion. Nobody approves
// anything on chatgpt.com or claude.ai here. What the mock reproduces is
// the wire on THIS side of the browser — the links and codes AO shows,
// the answers it decodes, and the credential it adopts — which is every
// part of the flow this repository owns. A genuine issuance stays a
// live-only check.
//
// WHY IT OWNS ITS BACKEND: the LAN bind persists to the settings file and
// rebinds the listener, and `harness.reset()` undoes neither. It also
// ADOPTS provider accounts, which is process-wide state no other spec
// should inherit. The two cases are one choreography on one paired
// device, so they run `.serial`.

import { expect, test, type BrowserContext, type Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { instrument, mintLink, nonLoopbackIPv4, type Surfaced } from './offhost-helpers.js';

// ---------------------------------------------------------------------
// Wire shapes.
// ---------------------------------------------------------------------

/** `provideraccountapp.LoginState`. */
interface ProviderLoginState {
  provider: string;
  phase: string;
  method?: string;
  authorizeUrl?: string;
  userCode?: string;
  error?: string;
}

/** `control.MockInfo`, narrowed to what picking a live mock needs. */
interface MockInfo {
  mockId: string;
  registration: { protocol: string };
  exited: boolean;
}

interface PendingPairing {
  linkId: string;
  redeemed?: boolean;
  verificationNumber?: string;
}

interface AccessOverview {
  pendingPairings?: PendingPairing[];
}

// ---------------------------------------------------------------------
// Budgets. Wall-clock against a named mechanism, never a loop count.
// ---------------------------------------------------------------------

// Same shape as the remote-device spec's: the pairing screen probes every
// 3s, holds the confirmed frame for 700ms, then awaits `redialAfterPairing`
// (5s). ~9s designed worst case, so roughly twice it.
const PAIRED_APP_MOUNT_MS = 20_000;

// A sign-in spawns a provider process, cuts an ephemeral home, and (on the
// success path) runs the whole adoption epilogue including an account
// probe — a second process. Generous because every step is real work, and
// a wedge is what the budget is here to catch.
const SIGNIN_STEP_MS = 20_000;

// The code the mock's fake approval page "shows" the person
// (cmd/ao-mockprovider/login.go). A callback carrying anything else is
// refused, which is how the burned-link path is driven.
const MOCK_CLAUDE_CODE = 'mock-auth-code';
const MOCK_CODEX_CODE = 'MOCK-CODE';

const lanIP = nonLoopbackIPv4();

/** The `state` half of a mock sign-in link, which the paste has to carry. */
function linkState(url: string): string {
  const state = new URL(url).searchParams.get('state');
  expect(state, `the sign-in link must carry its exchange state: ${url}`).toBeTruthy();
  return state!;
}

test.describe.serial('provider sign-in from a paired device', () => {
  // Not green-washed: a host with no non-loopback interface genuinely
  // cannot produce the peer this spec is about.
  test.skip(
    lanIP === null,
    'no non-loopback IPv4 interface on this host, so no off-host peer can be produced',
  );

  let harness: HarnessApp;
  let phoneContext: BrowserContext;
  let phone: Page;
  let surfaced: Surfaced;

  test.beforeAll(async ({ browser }) => {
    harness = await launchHarness();
    const network = await harness.rpc<{ bindAll: boolean }>('SetNetworkSettings', {
      bindAll: true,
    });
    expect(network.bindAll).toBe(true);

    phoneContext = await browser.newContext();
    phone = await phoneContext.newPage();
    surfaced = await instrument(phone);

    // Pair through the shipped screen, so the session the app then mounts
    // with is the one a real phone holds.
    const invite = await harness.rpc<{ url: string }>('MintDevicePairing', 'browser', 'full');
    expect(
      new URL(invite.url.split('#')[0]).hostname,
      'the pairing link must point at this host by a LAN address',
    ).toBe(lanIP);
    mintLink(invite);

    await phone.goto(invite.url);
    await expect(phone.getByRole('heading', { name: 'Pair this device' })).toBeVisible();
    await phone.getByLabel('Device name').fill('Sign-in phone');
    await phone.getByRole('button', { name: 'Pair' }).click();
    const shown = ((await phone.getByLabel('Verification number').textContent()) ?? '').trim();

    let redeemed: PendingPairing | undefined;
    await expect
      .poll(async () => {
        const overview = await harness.rpc<AccessOverview>('GetAccessOverview');
        redeemed = (overview.pendingPairings ?? []).find((p) => p.redeemed);
        return redeemed?.verificationNumber ?? '';
      })
      .toBe(shown);
    await harness.rpc('ConfirmDevicePairing', redeemed!.linkId);

    // Settings is where both cases run, and reaching it proves the app
    // mounted on the far side of the redial. Each provider has its own
    // page, so the case picks the one it signs into.
    await phone.getByTestId('sidebar-settings-button').click({ timeout: PAIRED_APP_MOUNT_MS });
    await expect(phone.getByRole('tablist', { name: 'Settings Sections' })).toBeVisible();
  });

  test.afterAll(async () => {
    await phoneContext?.close();
    await harness?.rpc('SetNetworkSettings', { bindAll: false }).catch(() => undefined);
    await harness?.close();
  });

  // -------------------------------------------------------------------
  // 1. Codex: a device code, read off this screen and typed on another.
  // -------------------------------------------------------------------
  test('a Codex sign-in shows a device code here and completes when the other screen does', async () => {
    await openProviderPage(phone, 'Codex');
    const section = phone.getByTestId('provider-accounts-codex');
    await expect(section).toBeVisible();
    await section.getByRole('button', { name: 'Log in to another account' }).click();

    const flow = phone.getByTestId('provider-login-flow-codex');
    await expect(flow).toBeVisible({ timeout: SIGNIN_STEP_MS });

    // The client asked for the remote method on its own, because this page
    // is not on the backend's machine. That is the branch under test: a
    // browser method here would have opened a link on a screen nobody is
    // looking at.
    await expect(flow).toHaveAttribute('data-method', 'remote');
    await expect(flow).toHaveAttribute('data-phase', 'awaiting_code', {
      timeout: SIGNIN_STEP_MS,
    });

    // Both halves have to be on screen: the code alone names nowhere to
    // type it, and the page alone asks for a code that is not shown.
    await expect(phone.getByTestId('provider-login-code')).toHaveText(MOCK_CODEX_CODE);
    await expect(phone.getByTestId('provider-login-url')).toHaveValue(
      'https://mock.agent-overflow.test/device',
    );

    // The other screen finishes. There is no client-driven step for this on
    // the real wire either — the app-server announces it — so the harness
    // drives the mock's completion through the control channel.
    const mockId = await liveCodexMock(harness);
    await harness.rpc('HarnessMockCommand', mockId, { type: 'login_complete' });

    // The panel clears because the sign-in SUCCEEDED, which the backend's
    // own retained state is the honest witness for: a panel can also clear
    // by being cancelled.
    await expect(flow).toHaveCount(0, { timeout: SIGNIN_STEP_MS });
    const settled = await harness.rpc<ProviderLoginState>('GetProviderLoginState', 'codex');
    expect(settled.phase, 'the backend must record the sign-in as succeeded').toBe('succeeded');

    // And the account is real enough to be listed.
    await expect(section.locator('[data-testid^="provider-account-"]')).not.toHaveCount(0);
  });

  // -------------------------------------------------------------------
  // 2. Claude: a link to open elsewhere and a code pasted back — including
  //    the burned-link path, which is the one every user hits by typo.
  // -------------------------------------------------------------------
  test('a Claude sign-in replaces the link a bad code burned, then completes on a good one', async () => {
    await openProviderPage(phone, 'Claude Code');
    const section = phone.getByTestId('provider-accounts-claude');
    await expect(section).toBeVisible();
    await section.getByRole('button', { name: 'Log in to another account' }).click();

    const flow = phone.getByTestId('provider-login-flow-claude');
    await expect(flow).toBeVisible({ timeout: SIGNIN_STEP_MS });
    await expect(flow).toHaveAttribute('data-method', 'remote');
    await expect(flow).toHaveAttribute('data-phase', 'awaiting_code', {
      timeout: SIGNIN_STEP_MS,
    });

    // Claude's remote flow shows a link and asks for a code BACK; it has no
    // device code of its own, and rendering one would be the other
    // provider's shape.
    await expect(phone.getByTestId('provider-login-code')).toHaveCount(0);
    const firstURL = (await phone.getByTestId('provider-login-url').inputValue()).trim();
    const firstState = linkState(firstURL);

    // A wrong code. Upstream burns the whole flow on one rejected callback,
    // so the only recovery is a fresh link — and the user must be TOLD, or
    // they will keep pasting a code the previous page gave them.
    const codeInput = phone.getByTestId('provider-login-code-input');
    await codeInput.fill(`not-the-code#${firstState}`);
    await phone.getByRole('button', { name: 'Submit code' }).click();

    await expect(phone.getByTestId('provider-login-notice')).toContainText(
      'That link stopped working, so here is a new one.',
      { timeout: SIGNIN_STEP_MS },
    );
    await expect
      .poll(
        async () => (await phone.getByTestId('provider-login-url').inputValue()).trim(),
        { message: 'a burned flow must be replaced by a link carrying a new exchange' },
      )
      .not.toBe(firstURL);

    // The replacement link's own code completes it.
    const secondState = linkState(
      (await phone.getByTestId('provider-login-url').inputValue()).trim(),
    );
    expect(secondState, 'the replacement must not reuse the burned exchange').not.toBe(firstState);
    await codeInput.fill(`${MOCK_CLAUDE_CODE}#${secondState}`);
    await phone.getByRole('button', { name: 'Submit code' }).click();

    await expect(flow).toHaveCount(0, { timeout: SIGNIN_STEP_MS });
    const settled = await harness.rpc<ProviderLoginState>('GetProviderLoginState', 'claude');
    expect(settled.phase, 'the backend must record the sign-in as succeeded').toBe('succeeded');

    // The credential the second exchange wrote was adopted. Without this a
    // flow that reported success over an epilogue that dropped the login
    // would read as green.
    await expect(section.locator('[data-testid^="provider-account-"]')).not.toHaveCount(0);
  });

  // -------------------------------------------------------------------
  // 3. The channel itself.
  // -------------------------------------------------------------------
  test('every transition reached this device on the provider:login channel', () => {
    // The screen assertions above cannot tell a channel that was DELIVERED
    // from one this device polled its way around, and `provider:login` is
    // audience-scoped rather than loopback-only precisely so a paired admin
    // device receives it. An absence here is that policy row regressing.
    expect(
      surfaced.eventChannels.filter((channel) => channel === 'provider:login').length,
      'a remote sign-in is driven entirely by pushed state, so several frames must have arrived',
    ).toBeGreaterThan(1);

    // Nothing was surfaced for two sign-ins that worked.
    expect(surfaced.errorToasts, 'a completed sign-in must surface no error toast').toEqual([]);
    expect(surfaced.consoleErrors, 'a completed sign-in must log no console error').toEqual([]);
  });
});

/**
 * Show one provider's Settings page by its nav-rail label. Each provider
 * has a page of its own, so the two cases below sign in from two
 * different ones.
 */
async function openProviderPage(page: Page, label: 'Claude Code' | 'Codex'): Promise<void> {
  await page.getByRole('tab', { name: label, exact: true }).click();
  await expect(page.getByRole('tab', { name: label, exact: true })).toHaveAttribute(
    'aria-selected',
    'true',
  );
}

/**
 * The mock serving the live sign-in: the one Codex process that has not
 * exited. A sign-in registers on the control channel because it is a spawn
 * something outside it has to steer; account probes do not, and this spec
 * starts no threads, so nothing else here is a Codex mock. Liveness is still
 * the discriminator, because each earlier leg left its own closed one behind.
 */
async function liveCodexMock(harness: HarnessApp): Promise<string> {
  let mockId = '';
  await expect
    .poll(
      async () => {
        const mocks = await harness.rpc<MockInfo[]>('HarnessListMocks');
        const live = mocks.filter((m) => m.registration.protocol === 'codex' && !m.exited);
        mockId = live.at(-1)?.mockId ?? '';
        return live.length;
      },
      { message: 'the sign-in must reach a live Codex mock', timeout: SIGNIN_STEP_MS },
    )
    .toBeGreaterThan(0);
  return mockId;
}
