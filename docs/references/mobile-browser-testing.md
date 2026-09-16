# Mobile browser validation

Run browser checks independently of the Android native-shell emulator:

```sh
make e2e-mobile-browser
```

This builds the production SPA and mock-provider harness, installs the pinned
Chromium and WebKit binaries, type-checks the suite and runs
[`compact-browser-lock.spec.ts`](../../e2e/tests/compact-browser-lock.spec.ts)
under the harness memory boundary. The same tests are included in `make e2e`.
On a Linux host missing browser libraries, install Playwright's system
dependencies with `cd e2e && pnpm exec playwright install-deps chromium webkit`.

For a built checkout, including repeated fresh-profile runs:

```sh
bin/ao-harness-e2e tests/compact-browser-lock.spec.ts --repeat-each=3
```

## What the automated checks establish

| Layer | Evidence |
|---|---|
| Mobile UI | Chromium with Pixel settings and WebKit with iPhone settings; touch unlock, portrait/landscape geometry, inert covered content, independent tabs, reload, reopening and browser history. |
| Authentication | Real pairing and server verification against disposable passkeys; a corrupted signature is refused, retry succeeds, view-only grants remain unchanged, a network outage preserves the cover and revocation stays locked. |
| Background rules | Both engines receive controlled lifecycle events and elapsed time. A separate Chromium case switches actual tabs, asserts trusted visibility events and checks immediate cover, short-return grace and the five-minute deadline. |
| Native WebAuthn | The existing `harness-passkey-lifecycle.spec.ts` uses Chromium's CDP authenticator through the browser's WebAuthn implementation. |

The cross-engine suite uses Playwright's
[software authenticator](https://playwright.dev/docs/api/class-credentials),
which overrides `navigator.credentials`. Linux WebKit's automation stub also
needs the interface constructor supplied by the fixture. These tests establish
application behavior and backend verification, not Safari's OS authenticator
integration. [Playwright WebKit is not shipping Safari](https://playwright.dev/docs/browsers#webkit).

The fixture uses an exact-authority HTTPS tunnel to an isolated backend on a
non-loopback interface. TLS still terminates at the real backend; only trust
in its disposable certificate is relaxed. No DNS service, host-file edits,
real provider account or Android emulator is needed. Missing non-loopback
interfaces produce a visible skip, which is not validation evidence.

Playwright normally forces pages active, which suppresses background events.
The real-visibility case launches a fresh, contained Chromium process and
attaches to its existing context with `noDefaults: true`. It must not create a
new context, which would restore the focus override. The test asserts event
`isTrusted` so scripted events cannot silently replace that coverage.

## Real-phone check

Before releasing changes to passkey prompts or OS lifecycle behavior, use an
isolated [mock-provider harness](../architecture/agent-harness.md#boot) with a
phone-trusted HTTPS hostname configured through the normal
[remote-access setup](../architecture/remote-access-setup.md). The automated
suite's local certificate exception does not establish phone certificate trust.
Keep the harness, pairing and passkey separate from production access.

Run this in Chrome on Android and Safari on iOS:

1. Pair the browser and enable Browser lock with a test passkey available to
   that phone. Verify that the OS shows the intended credential prompt.
2. Close and reopen the tab. Cancel verification and confirm that content stays
   covered; retry with the platform biometric or PIN and confirm it opens.
3. Switch to another app and return within five minutes. Repeat after more
   than five minutes and confirm that verification is required. Also check
   phone lock/unlock and the app switcher's snapshot.
4. Open a second tab and verify that unlocking it does not unlock the first.
   Rotate the phone and confirm Unlock remains reachable.
5. Lose and restore connectivity while locked. Content must stay covered until
   successful verification. Revoke the browser from the host and confirm that
   its passkey cannot unlock the revoked session.

Record phone model, OS/browser versions, URL route (LAN or Tailscale), credential
provider and results. OS prompts, snapshots, suspension and process eviction
remain real-device evidence; a phone viewport or WebKit run cannot certify them.
