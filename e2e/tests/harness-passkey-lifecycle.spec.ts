// The passkey lifecycle, end to end, in real browsers: the owner
// registers a credential at the machine, a browser this backend has never
// seen signs in with it and no code to type, that remote session proves
// step-up for a call no standing grant can open, and removing a passkey
// signs nothing out. Browser locking covers reloads, background timing,
// separate tabs, failed verification, and view-only permission preservation.
//
// WHY THIS FILE EXISTS. Every unit test around passkeys stops at a seam:
// the Go tests drive a soft authenticator, the Vitest tests drive a fake
// `navigator.credentials`, and neither can answer the question the
// feature is actually about — whether a REAL browser, holding no state,
// reaches a backend it has never met and ends up attached. The three
// things pairing cannot do (spec §4 "Passkeys") are exactly the three
// things nothing else in the suite exercises.
//
// WHAT IS REAL HERE. The relying party is the canonical domain saved
// through the shipped settings screen; the ceremonies are the shipped
// `transport/passkey.ts` against Chromium's own WebAuthn implementation;
// the registration is the shipped `PasskeysBlock.svelte` button; the
// sign-in is the shipped `TransportStatusBanner` button calling
// `deviceSession.signInWithPasskey`; the step-up retry is the shipped
// interception in `transport/wsClient.ts` running the shipped ceremony in
// `transport/stepUp.ts`. The only substituted part is the AUTHENTICATOR,
// which is Chromium's CDP virtual one — a platform authenticator cannot
// be driven by a test on any operating system, so that substitution is
// the boundary of what `make e2e` can reach at all.
//
// WHY THE DOMAIN IS `ao.e2e.test` OVER TLS, and why that is not a
// convenience. Three constraints intersect and only one shape satisfies
// all three:
//
//   - WebAuthn needs a SECURE CONTEXT, so the page cannot be plain HTTP
//     on a LAN address;
//   - a relying party ID must be a DOMAIN, never an address
//     (`app_passkey.go` argues why), so the page cannot be reached by IP;
//   - the remote leg must arrive at the backend as a NON-LOOPBACK PEER,
//     or host presence satisfies step-up and the passkey path is never
//     the thing under test.
//
// A `*.localhost` name is a secure context over cleartext, which is what
// makes it the harness path `app_passkey.go` names — but the FRONTEND
// treats every `*.localhost` document as local
// (`bootstrap.isLoopbackHostname`), so a page served under one never
// latches a terminal transport state and the sign-in affordance never
// appears. So: an ordinary domain, resolved per browser by
// `--host-resolver-rules` (loopback for the owner, this host's LAN
// address for the remote peer), reached over the listener's own TLS half
// with the certificate error waived at the context. `.test` is the
// reserved TLD for exactly this, so the name can never collide with a
// real one. Every other fact — the RP ID, the origins, the peer, the
// grant set — is then the production one.
//
// WHY IT OWNS ITS BACKEND: the canonical domain and the LAN bind both
// PERSIST to the settings file and rebind the listener, and
// `harness.reset()` undoes neither. It also owns its BROWSERS, because a
// host-resolver rule is a process-wide launch argument and the two legs
// need different ones. The five cases are one choreography on one
// credential, so they run `.serial` and carry state forward — and the
// removal is last, because every case before it needs the credential the
// removal takes away.
//
// WHAT STAYS LIVE-ONLY: a real platform authenticator (Touch ID, Windows
// Hello), a real cross-device CTAP-hybrid QR flow, a real synced
// credential appearing on a second machine, and Safari's and Windows
// Hello's own dialogs. None of those has a CDP surface.
//
// One more, and this one is a limit of the VIRTUAL authenticator rather
// than of the platform: registering a SECOND credential in a browser that
// already holds one. The begin excludes what the account already has, so
// the create must land on another authenticator — and a second virtual
// authenticator with no credential for this relying party makes every
// discoverable `get()` in that browser fail, which is the ceremony that
// precedes the create in the same click. Measured both ways: a decoy
// credential for another RP does not help, and Chromium permits only one
// `internal` authenticator per environment. Case 3 therefore proves the
// step-up mechanism end to end and stops where the browser does.

import {
  chromium,
  expect,
  test,
  type Browser,
  type BrowserContext,
  type Page,
} from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { attachAuthenticator, type Authenticator, type VirtualCredential } from './passkey-helpers.js';
import { instrument, nonLoopbackIPv4, redeemOnScreen, confirmOnHost, type PairingInvite, type Surfaced } from './offhost-helpers.js';

// ---------------------------------------------------------------------
// Wire shapes (internal/app/app_passkey.go, app_access_types.go,
// internal/network/network.go).
// ---------------------------------------------------------------------

interface PasskeySummary {
  id: string;
  label: string;
  createdAtMs: number;
  lastUsedAtMs?: number;
  relyingPartyId: string;
  usable: boolean;
  cloneWarning?: boolean;
  backedUp?: boolean;
  transports?: string[];
}

interface AccessSession {
  id: string;
  binding: string;
  connections?: number;
  scopes?: string[];
}

interface AccessDevice {
  id: string;
  label: string;
  class: string;
  channel?: string;
  sessions?: AccessSession[];
}

interface AccessAuditEntry {
  atMs: number;
  event: string;
  outcome: string;
  deviceId?: string;
  sessionId?: string;
  detail?: string;
  peer?: string;
}

interface PendingPairing {
  linkId: string;
  redeemed?: boolean;
}

interface AccessOverview {
  devices: AccessDevice[];
  pendingPairings?: PendingPairing[];
  audit?: AccessAuditEntry[];
}

interface NetworkSettings {
  bindAll: boolean;
  canonicalDomain: string;
}

interface SeedResult {
  projects: Array<{ projectId: string; path: string; threadIds: string[] }>;
}

// ---------------------------------------------------------------------
// Budgets. Wall-clock against a named mechanism, never a loop count
// (frontend/AGENTS.md § Testing).
// ---------------------------------------------------------------------

// A page that has just acquired a session AWAITS `redialAfterPairing`,
// bounded at REDIAL_SETTLE_BUDGET_MS (5s), and then issues its whole boot
// fan-out. ~10s is the designed worst case; this is twice it.
const APP_MOUNT_MS = 20_000;

// One registration from a REMOTE session is four round trips and one
// authenticator assertion: begin refused, step-up ceremony (two calls
// either side of a `get()`), begin again with the token.
// `identity.PasskeyCeremonyTTL` is 2 minutes, so this budget is a wedge
// tripwire rather than a race.
const REGISTER_MS = 30_000;

// ---------------------------------------------------------------------
// The rig.
// ---------------------------------------------------------------------

const lanIP = nonLoopbackIPv4();

/**
 * The name this backend answers to for the length of this file. Reserved
 * by RFC 6761 for testing, so it resolves nowhere and can never collide
 * with a real deployment's domain.
 */
const DOMAIN = 'ao.e2e.test';

/**
 * A browser that believes `DOMAIN` lives at `target`. The rule is a
 * launch argument rather than a context option because Chromium resolves
 * names in the browser process, so each leg needs a browser of its own.
 */
async function browserMapping(target: string): Promise<Browser> {
  return chromium.launch({ args: [`--host-resolver-rules=MAP ${DOMAIN} ${target}`] });
}

/**
 * A context that accepts the listener's certificate. The backend answers
 * an SNI it holds no domain certificate for with its own self-signed one
 * (`transport/certsource.go`), so the name never matches — which is the
 * production behaviour, and a browser reaching a domain-certificate-less
 * backend by name sees exactly this. Waiving it at the CONTEXT keeps the
 * document `https:`, and therefore a secure context, which a flag that
 * forced trust on an http origin would not.
 */
async function acceptingContext(browser: Browser): Promise<BrowserContext> {
  return browser.newContext({ ignoreHTTPSErrors: true });
}

/**
 * Navigate to this backend by its NAME. Every navigation mints its own
 * one-time page ticket, for the reason `HarnessApp.open` does
 * (e2e/AGENTS.md); only the authority is rewritten.
 */
async function openOnDomain(harness: HarnessApp, page: Page): Promise<void> {
  const url = new URL(await harness.pageURL());
  url.protocol = 'https:';
  url.hostname = DOMAIN;
  await page.goto(url.toString());
}

async function pairViewerOnDomain(harness: HarnessApp, page: Page, label: string): Promise<void> {
  const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'browser', 'view-only');
  // Rewrite the invitation address and payload to the same listener over HTTPS.
  const url = new URL(invite.url);
  url.protocol = 'https:';
  url.hostname = DOMAIN;
  const payload = JSON.parse(Buffer.from(url.hash.slice('#pair='.length), 'base64url').toString('utf8'));
  payload.endpoint = url.origin;
  url.hash = 'pair=' + Buffer.from(JSON.stringify(payload)).toString('base64url');
  const shown = await redeemOnScreen(page, { ...invite, url: url.toString() }, label);
  await confirmOnHost(harness, shown);
  await expect(page.getByTestId('view-only-indicator')).toBeVisible({ timeout: APP_MOUNT_MS });
}

// The one thing this file's pages surface that the APP did not produce.
//
// `HarnessRegisterPage` authenticates a page against the origin the harness
// BOOTED on (`internal/harnessrpc/ui.go` `sameHarnessOrigin`, which knows
// the loopback spellings and nothing about a canonical domain), and every
// page here is deliberately on another name — so the operator UI bridge is
// refused its registration on both legs and logs the refusal. It reaches
// two captures, the wire one and the console one, and both are filtered by
// NAME rather than by loosening the assertion: everything else either side
// still has to be empty.
const RIG_BRIDGE_NOISE = 'harness bridge: page registration failed';

/** The refusals the app was given, with the rig's own left out. */
function appRefusals(surfaced: Surfaced): string[] {
  return surfaced.refusals.filter((refusal) => !refusal.startsWith('HarnessRegisterPage '));
}

/** The console errors the app produced, with the rig's own left out. */
function appConsoleErrors(surfaced: Surfaced): string[] {
  return surfaced.consoleErrors.filter((entry) => !entry.includes(RIG_BRIDGE_NOISE));
}

/**
 * Settings → Allow device access, from a mounted app, with the two folds
 * this file reaches into opened: the domain editor sits under "Advanced
 * network settings" (NetworkSection.svelte) and the passkeys block under
 * "Security & passkeys" (DevicesSection.svelte). A closed `<details>`
 * lays out none of its content, so a locator inside one is not visible
 * and cannot be clicked. Each fold is asserted OPEN rather than merely
 * clicked, so a summary that toggled the wrong way fails here and not
 * three assertions later.
 */
async function openDeviceAccessSettings(page: Page): Promise<void> {
  await page.getByTestId('sidebar-settings-button').click();
  const tab = page.getByRole('tab', { name: 'Allow device access', exact: true });
  await tab.click();
  await expect(tab).toHaveAttribute('aria-selected', 'true');
  await page.getByText('Advanced network settings', { exact: true }).click();
  await expect(page.getByTestId('network-domain-editor')).toBeVisible();
  await page.getByText('Security & passkeys', { exact: true }).click();
  await expect(page.getByTestId('passkeys-block')).toBeVisible();
}

test.describe.serial('passkey lifecycle', () => {
  // Not green-washed: a host with no non-loopback interface genuinely
  // cannot produce the remote peer half of this spec, and saying so is
  // the honest outcome. A skip is visible in the report; a vacuous pass
  // is not.
  test.skip(
    lanIP === null,
    'no non-loopback IPv4 interface on this host, so no off-host peer can be produced',
  );

  let harness: HarnessApp;

  let setupBrowser: Browser;

  let ownerBrowser: Browser;
  let ownerContext: BrowserContext;
  let ownerPage: Page;
  let ownerSurfaced: Surfaced;

  let remoteBrowser: Browser;
  let remoteContext: BrowserContext;
  let remotePage: Page;
  let remoteAuthenticator: Authenticator;
  let remoteSurfaced: Surfaced;

  /** What the owner's authenticator produced, carried to the remote leg. */
  let registered: VirtualCredential[] = [];

  test.beforeAll(async () => {
    harness = await launchHarness();

    // One visible thread, so "the remote browser attached" is a rendered
    // row from a real RPC rather than the absence of a banner. A draft
    // row is hidden from the sidebar, so it carries a turn.
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        {
          name: 'passkey-lifecycle',
          repo: {},
          threads: [
            {
              title: 'Kitchen sink',
              turns: [{ userText: 'first', items: [{ kind: 'assistant_text', summary: 'one' }] }],
            },
          ],
        },
      ],
    });
    expect(seed.projects[0].threadIds, 'the fixture must seed one visible thread').toHaveLength(1);

    // The domain is saved through the SHIPPED settings screen, on an
    // ordinary loopback page — which is the only place it can be saved
    // before it exists: the Host guard 404s a name this backend does not
    // yet answer to, so a page loaded under the new name could not have
    // set it. Its own browser, with no host-resolver rule and no
    // authenticator, closed the moment the two writes have landed.
    setupBrowser = await chromium.launch();
    const setup = await setupBrowser.newPage();
    await harness.open(setup);
    await expect(setup.getByTestId('sidebar-settings-button')).toBeVisible({
      timeout: APP_MOUNT_MS,
    });
    await openDeviceAccessSettings(setup);
    await setup.getByTestId('network-canonical-domain').fill(DOMAIN);
    await setup.getByTestId('network-domain-save').click();
    await expect
      .poll(
        async () => (await harness.rpc<NetworkSettings>('GetNetworkSettings')).canonicalDomain,
        { message: 'saving the domain must reach the backend' },
      )
      .toBe(DOMAIN);

    // The LAN bind, through the same screen. It writes the whole record
    // (NetworkSection.writeRequest), so the domain just saved rides along
    // rather than being erased.
    await setup.getByLabel('Toggle remote access').click();
    await expect
      .poll(async () => await harness.rpc<NetworkSettings>('GetNetworkSettings'), {
        message: 'the LAN bind must reach the backend with the domain intact',
      })
      .toMatchObject({ bindAll: true, canonicalDomain: DOMAIN });
    await setupBrowser.close();

    ownerBrowser = await browserMapping('127.0.0.1');
    ownerContext = await acceptingContext(ownerBrowser);
    ownerPage = await ownerContext.newPage();
    ownerSurfaced = await instrument(ownerPage);

    remoteBrowser = await browserMapping(lanIP!);
  });

  test.afterAll(async () => {
    await setupBrowser?.close();
    await ownerBrowser?.close();
    await remoteBrowser?.close();
    // Leave the instance as we found it: both preferences persist to the
    // settings file and outlive the listener.
    await harness
      ?.rpc('SetNetworkSettings', { bindAll: false, canonicalDomain: '' })
      .catch(() => undefined);
    await harness?.close();
  });

  // -------------------------------------------------------------------
  // 1. Registration, at the machine.
  // -------------------------------------------------------------------
  test('the owner registers a passkey from their own screen, and nothing asks for a second proof', async () => {
    await openOnDomain(harness, ownerPage);
    await expect(ownerPage.getByTestId('sidebar-settings-button')).toBeVisible({
      timeout: APP_MOUNT_MS,
    });
    const ownerAuthenticator = await attachAuthenticator(ownerContext, ownerPage);
    await openDeviceAccessSettings(ownerPage);

    // The precondition: this backend has a domain, so the block offers
    // the control rather than explaining why it cannot. Asserting the
    // EMPTY state first is what makes the count below mean something.
    await expect(ownerPage.getByTestId('passkeys-block')).toBeVisible();
    await expect(ownerPage.getByText('No passkey is registered for this backend.')).toBeVisible();

    await ownerPage.getByRole('button', { name: 'Add a passkey' }).click();
    await expect(ownerPage.getByTestId('passkey-row')).toHaveCount(1, { timeout: REGISTER_MS });

    // What the backend stored, read from the host rather than off the
    // screen that just wrote it.
    const stored = await harness.rpc<PasskeySummary[]>('ListPasskeys');
    expect(stored).toHaveLength(1);
    expect(
      stored[0],
      'a credential is bound to the exact relying party string, so the stored name is the whole of whether it can ever be used again',
    ).toMatchObject({ relyingPartyId: DOMAIN, usable: true });
    expect(stored[0].cloneWarning ?? false).toBe(false);

    // And what the authenticator holds, which is the fixture the next
    // case signs in with.
    registered = await ownerAuthenticator.credentials();
    expect(registered, 'the ceremony must have produced exactly one credential').toHaveLength(1);
    expect(
      registered[0],
      'sign-in names no account, so a credential that is not client-side discoverable could never be offered',
    ).toMatchObject({ isResidentCredential: true, rpId: DOMAIN });

    // The claim this case exists for beyond "it worked": at the machine,
    // host presence satisfies step-up, so REGISTERING must cost no
    // ceremony and raise no prompt. A begin that was refused here would
    // mean the owner's own screen had been made to prove itself.
    expect(
      ownerSurfaced.rpcReplies.length,
      'the wire capture must have observed this session, or the absences below mean nothing',
    ).toBeGreaterThan(5);
    expect(
      ownerSurfaced.rpcReplies.filter((name) => name.startsWith('BeginPasskeyStepUp')),
      'host presence is a step-up proof, so no ceremony may run on the owner screen',
    ).toEqual([]);
    expect(appRefusals(ownerSurfaced), 'the owner screen must be refused nothing').toEqual([]);
    expect(ownerSurfaced.errorToasts, 'a registration that worked surfaces no error').toEqual([]);
    expect(appConsoleErrors(ownerSurfaced)).toEqual([]);
  });

  // -------------------------------------------------------------------
  // 2. Sign-in, from a browser this backend has never seen.
  // -------------------------------------------------------------------
  test('a browser holding nothing signs in with the passkey and no code to type', async () => {
    remoteContext = await acceptingContext(remoteBrowser);
    remotePage = await remoteContext.newPage();
    remoteSurfaced = await instrument(remotePage);

    await openOnDomain(harness, remotePage);

    // The state a passkey is FOR. The manifest serves (the page ticket is
    // good), and the socket is what this backend will not open for an
    // off-host peer naming no session — so the ladder stops and the
    // banner is the whole recovery story.
    const banner = remotePage.getByTestId('transport-status-banner');
    await expect(banner).toBeVisible({ timeout: APP_MOUNT_MS });
    await expect(banner).toHaveAttribute('data-status', 'pairing-required');

    // The credential moves the way a synced passkey does: the same key
    // material, discoverable, on a machine that has never met this
    // backend. Everything else about this context is empty — no
    // localStorage, so no device key and no stored session; no cookie
    // jar, so no page credential.
    remoteAuthenticator = await attachAuthenticator(remoteContext, remotePage);
    for (const credential of registered) await remoteAuthenticator.adopt(credential);

    expect(
      await harness.rpc<PasskeySummary[]>('ListPasskeys').then((rows) => rows[0].lastUsedAtMs ?? 0),
      'the credential must be unused before this case uses it',
    ).toBe(0);

    // A fresh window, and the reason it is drawn HERE: the boot that just
    // ran was refused wholesale, so the page surfaced a real error per
    // store and those are what a latched page is SUPPOSED to show. What
    // this case claims is about the page on the other side of the
    // sign-in, so the window opens at the click.
    remoteSurfaced.errorToasts.length = 0;
    remoteSurfaced.consoleErrors.length = 0;

    await remotePage.getByTestId('transport-status-passkey').click();

    // Attached, with a whole app rather than an attached socket over an
    // empty one: the sidebar row is a real `ListThreads` answer, so it
    // fails if the page kept the stores its latched boot could not load
    // (TransportStatusBanner.svelte boots again for exactly that).
    await expect(remotePage.getByTestId('thread-row')).toHaveCount(1, { timeout: APP_MOUNT_MS });
    await expect(banner).toHaveCount(0);
    await expect(remotePage.getByTestId('view-only-indicator')).toHaveCount(0);

    // On the host side: a device row nobody paired, holding a session
    // with the full grant set — which is what a registered passkey is
    // (identity.FinishPasskeySignIn), and why narrowing a device is still
    // a pairing link.
    const overview = await harness.rpc<AccessOverview>('GetAccessOverview');
    const signedIn = overview.devices.filter((d) => d.channel !== 'local');
    expect(signedIn, 'the sign-in must enroll exactly one device row').toHaveLength(1);
    const session = (signedIn[0].sessions ?? [])[0];
    expect(session?.connections ?? 0, 'that session must be carrying the live socket').toBeGreaterThan(0);
    expect(session?.scopes ?? [], 'a passkey sign-in grants what a full pairing does').toContain(
      'access:admin',
    );

    // The audit row is where "off-host" stops being an assumption: the
    // peer is the kernel's answer about the connection that presented the
    // assertion, and it is the LAN address rather than loopback.
    const audit = (overview.audit ?? []).filter((entry) => entry.event === 'passkey-signed-in');
    expect(audit, 'a sign-in is an audited event').toHaveLength(1);
    expect(audit[0].outcome).toBe('allowed');
    expect(
      audit[0].peer ?? '',
      'the whole point of this leg is that the peer is not this machine',
    ).toContain(lanIP!);

    // And the credential now says it was used, which is the fact the
    // settings list renders.
    const stored = await harness.rpc<PasskeySummary[]>('ListPasskeys');
    expect(stored[0].lastUsedAtMs ?? 0).toBeGreaterThan(0);

    expect(
      remoteSurfaced.errorToasts,
      'the app the sign-in lands in is a working one, so nothing after the click may surface an error',
    ).toEqual([]);
    expect(appConsoleErrors(remoteSurfaced)).toEqual([]);
  });

  // -------------------------------------------------------------------
  // 3. Step-up, proven by the passkey and by nothing else.
  // -------------------------------------------------------------------
  test('the remote session satisfies a step-up gate no standing grant can open', async () => {
    // Registering another credential is the step-up-gated call driven
    // here, because no standing grant opens it: `BeginPasskeyRegistration`
    // ISSUES a way in, so `access:admin` alone is refused. Nothing at the
    // call site asks for the ceremony — the transport runs it for
    // whatever the backend refuses — which is what case 4 then proves on
    // a surface that has never heard of a passkey.
    await openDeviceAccessSettings(remotePage);
    await expect(remotePage.getByTestId('passkeys-block')).toBeVisible();
    await expect(remotePage.getByTestId('passkey-row')).toHaveCount(1);

    // A fresh window over the wire, so the frames below are this act's.
    remoteSurfaced.rpcReplies.length = 0;
    remoteSurfaced.refusals.length = 0;
    remoteSurfaced.errorToasts.length = 0;

    await remotePage.getByRole('button', { name: 'Add a passkey' }).click();

    // The whole mechanism, read off the WIRE rather than off the outcome:
    // the call was refused for want of a proof no standing grant supplies,
    // a ceremony ran and this backend verified it, and the same call was
    // then accepted. Two replies to one button press is the retry, and
    // waiting for the second is what makes the refusal count below mean
    // something.
    await expect
      .poll(
        () => remoteSurfaced.rpcReplies.filter((name) => name === 'BeginPasskeyRegistration').length,
        {
          timeout: REGISTER_MS,
          message: 'the refused call must be retried once, with the proof armed',
        },
      )
      .toBe(2);
    expect(
      remoteSurfaced.rpcReplies.filter((name) => name === 'FinishPasskeyStepUp'),
      'the proof is a ceremony this backend verified, not a flag the client set',
    ).toEqual(['FinishPasskeyStepUp']);
    expect(
      appRefusals(remoteSurfaced),
      'exactly one refusal: the begin is gated, the finish rides its ceremony handle, and a second refusal would mean the retry went out unarmed',
    ).toEqual(['BeginPasskeyRegistration step_up_required:']);

    // The host's own record of it, and the reason `FinishPasskeyStepUp`
    // passes no peer: the attribution that identifies a call on an open
    // socket is the session, not an address.
    const overview = await harness.rpc<AccessOverview>('GetAccessOverview');
    const stepUps = (overview.audit ?? []).filter((entry) => entry.event === 'passkey-step-up');
    expect(stepUps, 'one gated call, one audited proof').toHaveLength(1);
    expect(stepUps[0].outcome).toBe('allowed');
    expect(
      stepUps[0].sessionId ?? '',
      'a step-up token is bound to the session that asked for it',
    ).not.toBe('');

    // WHERE THE RIG STOPS, and it stops here rather than at the product.
    // The accepted begin hands the browser an `excludeCredentials` list
    // naming the credential this browser already holds, so the create has
    // to land on a DIFFERENT authenticator — and Chromium's virtual
    // environment cannot supply one without breaking the ceremony that
    // precedes it in the same click: a second virtual authenticator with
    // no credential for this relying party makes every discoverable
    // `get()` in that browser fail (measured; a decoy credential for
    // another RP does not help, and only one `internal` authenticator may
    // exist per environment). So the second credential cannot be created
    // here, and the shipped block reports that with its own copy.
    //
    // Registering a SECOND credential from one browser is therefore
    // live-only, alongside the platform authenticators in the header. The
    // proof this case exists for — refusal, ceremony, accepted retry — is
    // complete above.
    await expect
      .poll(() => remoteSurfaced.errorToasts.length, { timeout: REGISTER_MS })
      .toBe(1);
    expect(
      remoteSurfaced.errorToasts[0].startsWith('Failed to add a passkey:'),
      'the failure that stops here must be the browser create, reported by the shipped block',
    ).toBe(true);
    expect(
      await harness.rpc<PasskeySummary[]>('ListPasskeys'),
      'nothing was registered, because nothing was created',
    ).toHaveLength(1);
  });

  // -------------------------------------------------------------------
  // 4. The same gate, on a surface that has never heard of a passkey.
  // -------------------------------------------------------------------
  test('a gated call on an ordinary surface prompts and lands, with nothing wired at its call site', async () => {
    // WHY A SECOND STEP-UP CASE. Case 3's call is the one the passkey
    // block makes, so it could pass while every OTHER gated surface —
    // minting a pairing link, MCP config writes, provider custom env,
    // worktree-setup recipes, the host-tier settings keys — stayed
    // unreachable from a phone. That was the shipped state until the
    // ceremony moved behind one interception in the transport, and it was
    // invisible from the owner's own screen, where host presence
    // satisfies the gate and no ceremony ever runs.
    //
    // `MintDevicePairingOnNetwork` is the surface driven here because it
    // is a DIFFERENT scope (`access:admin` rather than the passkey
    // block's own), it is `//ao:stepup`, and `PairDeviceModal.svelte`
    // calls it plainly — there is no passkey code anywhere in that
    // component. This browser is passkey-signed and NOT an own device,
    // and `ownPairingAdmin` refuses personal enrollment from any caller
    // that is neither local nor a member — so the modal must not offer
    // "My device" here at all (it used to, and the option always failed
    // with a toast: the 2026-09-08 ruling). It offers ordinary Full
    // access / View only, both of which this backend honours; view-only
    // is driven below because it is the same step-up gate with the
    // smaller grant.
    const beforeAudit = ((await harness.rpc<AccessOverview>('GetAccessOverview')).audit ?? [])
      .filter((entry) => entry.event === 'passkey-step-up').length;

    // A fresh window over the wire, so the frames below are this act's.
    remoteSurfaced.rpcReplies.length = 0;
    remoteSurfaced.refusals.length = 0;
    remoteSurfaced.errorToasts.length = 0;

    await remotePage.getByRole('button', { name: 'Allow a device to connect' }).click();
    await expect(remotePage.getByRole('radio', { name: 'Full access' })).toBeVisible();
    await expect(
      remotePage.getByRole('radio', { name: 'My device' }),
      'a non-member is not offered the personal join the backend would refuse',
    ).toHaveCount(0);
    await expect(remotePage.getByText('“My device” pairing is available from your own devices, or at the computer.')).toBeVisible();
    await remotePage.getByRole('radio', { name: 'View only' }).check();
    await remotePage.getByRole('button', { name: 'Phone or tablet' }).click();

    // The mint LANDED: the modal is showing the link it answered with,
    // which it only reaches on a resolved call.
    const link = remotePage.getByLabel('Pairing link');
    await expect(link).toBeVisible({ timeout: REGISTER_MS });
    expect(await link.inputValue(), 'a pairing link carries its payload in the fragment').toContain(
      '#pair=',
    );

    // The mechanism, off the wire: refused for want of a proof, one
    // ceremony verified by this backend, the same call accepted.
    expect(
      remoteSurfaced.rpcReplies.filter((name) => name === 'MintDevicePairingOnNetwork'),
      'one button press, two replies: the refusal and the retry that carried the proof',
    ).toEqual(['MintDevicePairingOnNetwork', 'MintDevicePairingOnNetwork']);
    expect(
      remoteSurfaced.rpcReplies.filter((name) => name === 'FinishPasskeyStepUp'),
      'the proof is a ceremony this backend verified, not a flag the client set',
    ).toEqual(['FinishPasskeyStepUp']);
    expect(
      appRefusals(remoteSurfaced),
      'exactly one refusal: a second would mean the retry went out unarmed',
    ).toEqual(['MintDevicePairingOnNetwork step_up_required:']);
    expect(
      remoteSurfaced.errorToasts,
      'a call that went through on the retry surfaces nothing',
    ).toEqual([]);

    // The host's own record: the link exists, and the proof that admitted
    // it was audited as its own event.
    const overview = await harness.rpc<AccessOverview>('GetAccessOverview');
    expect(
      overview.pendingPairings ?? [],
      'the mint reached the store, not just the screen',
    ).toHaveLength(1);
    expect(
      (overview.audit ?? []).filter((entry) => entry.event === 'passkey-step-up').length,
      'one gated call, one more audited proof',
    ).toBe(beforeAudit + 1);

    // Leave the instance as case 5 expects it: the link is spent here
    // rather than left pending, and cancelling it is not step-up gated —
    // taking access away never is. The row is asserted PRESENT first, so
    // its absence afterwards is the cancel rather than a section that
    // never showed it.
    // Scoped to the dialog: the pending row behind it offers the same
    // control, which is the point of the row rather than an ambiguity to
    // work around.
    await expect(remotePage.getByTestId('pending-pairing')).toHaveCount(1);
    await remotePage
      .getByRole('dialog', { name: 'Allow a device to connect' })
      .getByRole('button', { name: 'Cancel link' })
      .click();
    await expect(remotePage.getByTestId('pending-pairing')).toHaveCount(0);
  });

  // -------------------------------------------------------------------
  // 5. Removal, which is not a revocation.
  // -------------------------------------------------------------------
  test('browser lock verifies each tab, preserves the session, and follows background timing', async () => {
    const savedSession = await remotePage.evaluate(() => JSON.parse(localStorage.getItem('agent-overflow:deviceSession')!));
    const control = remotePage.getByRole('switch', { name: 'Require a passkey to open' });
    await expect(control).toBeVisible();
    await control.click();
    await expect(control).toHaveAttribute('aria-checked', 'true');
    await expect(remotePage.getByTestId('app-lock')).toHaveCount(0);

    const second = await remoteContext.newPage();
    try {
      await second.goto(remotePage.url());
      await expect(second.getByTestId('app-lock')).toBeVisible();
      const authenticator = await attachAuthenticator(remoteContext, second);
      for (const credential of await remoteAuthenticator.credentials()) await authenticator.adopt(credential);
      await remotePage.clock.setFixedTime(Date.now());
      await remotePage.reload();
      await expect(remotePage.getByTestId('app-lock')).toBeVisible();
      await expect(remotePage.locator('#app')).toHaveAttribute('inert', '');
      await second.getByRole('button', { name: 'Unlock', exact: true }).click();
      await expect(second.getByTestId('app-lock')).toHaveCount(0);
      await expect(remotePage.getByTestId('app-lock')).toBeVisible();
      await remotePage.keyboard.press('Tab');
      await expect(remotePage.getByRole('button', { name: 'Unlock', exact: true })).toBeFocused();
      await remotePage.keyboard.press('Enter');
      await expect(remotePage.getByTestId('app-lock')).toHaveCount(0);
    } finally {
      await second.close();
    }

    // Headless Chromium does not background tabs on bringToFront. Deliver the
    // browser visibility event with a controlled wall clock; app handlers are real.
    const start = Date.now();
    await remotePage.clock.setFixedTime(start);
    const visibility = (hidden: boolean) => remotePage.evaluate((value) => {
      Object.defineProperty(document, 'hidden', { configurable: true, value });
      document.dispatchEvent(new Event('visibilitychange'));
    }, hidden);
    await visibility(true);
    await expect(remotePage.getByTestId('app-lock')).toBeVisible();
    await remotePage.clock.setFixedTime(start + 4_000);
    await visibility(false);
    await expect(remotePage.getByTestId('app-lock')).toHaveCount(0);
    await visibility(true);
    await remotePage.clock.setFixedTime(start + 304_000);
    await visibility(false);
    await expect(remotePage.getByTestId('app-lock')).toBeVisible();
    await remotePage.clock.setFixedTime(Date.now());

    // Cancellation of the browser API must leave the real lock closed.
    await remotePage.evaluate(() => {
      Object.defineProperty(navigator.credentials, 'get', {
        configurable: true,
        value: () => Promise.reject(new DOMException('Canceled', 'NotAllowedError')),
      });
    });
    await remotePage.getByRole('button', { name: 'Unlock', exact: true }).click();
    await expect(remotePage.getByRole('alert').filter({ hasText: 'Could not verify your passkey' })).toBeVisible();
    await expect(remotePage.getByTestId('app-lock')).toBeVisible();
    await remotePage.evaluate(() => Reflect.deleteProperty(navigator.credentials, 'get'));
    await remotePage.getByRole('button', { name: 'Unlock', exact: true }).click();
    await expect(remotePage.getByTestId('app-lock')).toHaveCount(0);
    await remotePage.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })));
    await expect(remotePage.getByTestId('app-lock')).toBeVisible();
    await remotePage.getByRole('button', { name: 'Unlock', exact: true }).click();
    await expect(remotePage.getByTestId('app-lock')).toHaveCount(0);

    const after = await remotePage.evaluate(() => JSON.parse(localStorage.getItem('agent-overflow:deviceSession')!));
    expect(after.sessionId).toBe(savedSession.sessionId);
    expect(after.scopes).toEqual(savedSession.scopes);
    // A paired browser routed through loopback must still verify its passkey.
    // Host presence is accepted for administrative step-up on this connection.
    const proxied = await acceptingContext(ownerBrowser);
    try {
      const page = await proxied.newPage();
      await pairViewerOnDomain(harness, page, 'Loopback viewer');
      const authenticator = await attachAuthenticator(proxied, page);
      for (const credential of await remoteAuthenticator.credentials()) await authenticator.adopt(credential);
      await page.getByTestId('sidebar-settings-button').click();
      await page.getByRole('tab', { name: 'Allow device access', exact: true }).click();
      const toggle = page.getByRole('switch', { name: 'Require a passkey to open' });
      await toggle.click();
      await expect(toggle).toHaveAttribute('aria-checked', 'true');
      await page.reload();
      await expect(page.getByTestId('app-lock')).toBeVisible();
      await page.evaluate(() => Object.defineProperty(navigator.credentials, 'get', {
        configurable: true,
        value: () => Promise.reject(new DOMException('Canceled', 'NotAllowedError')),
      }));
      await page.getByRole('button', { name: 'Unlock', exact: true }).click();
      await expect(page.getByRole('alert').filter({ hasText: 'Could not verify your passkey' })).toBeVisible();
      await expect(page.getByTestId('app-lock')).toBeVisible();
      await page.evaluate(() => Reflect.deleteProperty(navigator.credentials, 'get'));
      await page.getByRole('button', { name: 'Unlock', exact: true }).click();
      await expect(page.getByTestId('app-lock')).toHaveCount(0);
    } finally {
      await proxied.close();
    }
    // Reload preserves settings state; open the relevant folds only if closed.
    if (!await control.isVisible()) await openDeviceAccessSettings(remotePage);
    await control.click();
    await expect(control).toHaveAttribute('aria-checked', 'false');
    await remotePage.reload();
    await expect(remotePage.getByTestId('thread-row')).toHaveCount(1, { timeout: APP_MOUNT_MS });
    await expect(remotePage.getByTestId('app-lock')).toHaveCount(0);
    await openDeviceAccessSettings(remotePage);
  });

  test('a compact view-only browser unlocks without acquiring full access', async () => {
    const context = await remoteBrowser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    const page = await context.newPage();
    try {
      await pairViewerOnDomain(harness, page, 'Locked viewer');
      const authenticator = await attachAuthenticator(context, page);
      for (const credential of await remoteAuthenticator.credentials()) await authenticator.adopt(credential);
      await page.getByTestId('sidebar-settings-button').click();
      await page.getByRole('tab', { name: 'Allow device access', exact: true }).click();
      const control = page.getByRole('switch', { name: 'Require a passkey to open' });
      await control.click();
      await expect(control).toHaveAttribute('aria-checked', 'true');
      const before = await page.evaluate(() => JSON.parse(localStorage.getItem('agent-overflow:deviceSession')!));
      expect(before.scopes).not.toContain('threads:operate');
      await page.reload();
      await expect(page.getByTestId('app-lock')).toBeVisible();
      await page.getByRole('button', { name: 'Unlock', exact: true }).click();
      await expect(page.getByTestId('app-lock')).toHaveCount(0);
      await expect(page.getByTestId('view-only-indicator')).toBeVisible();
      const after = await page.evaluate(() => JSON.parse(localStorage.getItem('agent-overflow:deviceSession')!));
      expect(after.sessionId).toBe(before.sessionId);
      expect(after.scopes).toEqual(before.scopes);
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
    } finally {
      await context.close();
    }
  });

  test('removing a passkey takes the credential away and signs no device out', async () => {
    const before = await harness.rpc<AccessOverview>('GetAccessOverview');
    const device = before.devices.filter((d) => d.channel !== 'local')[0];
    expect(device, 'the remote device row is the subject of the claim below').toBeTruthy();

    // The sentence beside the control, because the control sits next to
    // device revokes that DO sign a device out.
    await expect(
      remotePage.getByText('Removing one does not sign any device out'),
    ).toBeVisible();

    // A fresh window: case 3 stopped at a browser limit and the block
    // said so, and this act's claim is about what happens after the
    // removal. Case 4 left nothing pending.
    remoteSurfaced.errorToasts.length = 0;
    remoteSurfaced.consoleErrors.length = 0;

    // The credential being removed is the one that signed THIS session
    // in, which is the strongest form of the claim: the session survives
    // losing the credential it arrived with.
    const row = remotePage.getByTestId('passkey-row').first();
    await row.getByRole('button', { name: 'Remove' }).click();
    await row.getByRole('button', { name: 'Confirm remove' }).click();
    await expect(remotePage.getByTestId('passkey-row')).toHaveCount(0);
    await expect(
      remotePage.getByText('No passkey is registered for this backend.'),
    ).toBeVisible();

    expect(
      await harness.rpc<PasskeySummary[]>('ListPasskeys'),
      'the removal reached the store, not just the screen',
    ).toHaveLength(0);

    // The claim: the session that just removed a credential is still the
    // session, still attached, still able to call. `DeletePasskey`
    // carries no //ao:stepup and ends no session — a device is ended by
    // revoking the device.
    await expect(remotePage.getByTestId('transport-status-banner')).toHaveCount(0);
    const after = await harness.rpc<AccessOverview>('GetAccessOverview');
    const same = after.devices.find((d) => d.id === device.id);
    expect(same, 'the device row must survive a passkey removal').toBeTruthy();
    expect(
      (same?.sessions ?? [])[0]?.connections ?? 0,
      'the socket must still be attached after the removal',
    ).toBeGreaterThan(0);

    // Audited as a removal, and nothing else was. Both revocation events
    // are named, because a passkey removal that ended a device would show
    // up as either one and the claim is about neither happening.
    const audit = after.audit ?? [];
    expect(audit.filter((entry) => entry.event === 'passkey-removed')).toHaveLength(1);
    expect(
      audit.filter(
        (entry) => entry.event === 'session-revoked' || entry.event === 'device-revoked',
      ),
      'no revocation may fall out of removing a credential',
    ).toEqual([]);

    expect(remoteSurfaced.errorToasts).toEqual([]);
    expect(appConsoleErrors(remoteSurfaced)).toEqual([]);
  });
});
