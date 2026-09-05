# Remote access: Mac host and Android phone

The Mac runs the backend and providers. The Android APK is a client of that
backend; an Android system image, Docker image, or public server is unnecessary.
Agent Overflow joins Tailscale as its **own node**, normally `agent-overflow`.
The Mac's separate Tailscale app does not have to be connected for that node to
serve the phone. The phone's Tailscale app must be connected to the same tailnet.

## Set up the host

1. Install the macOS release with `scripts/install.sh`, as described in the
   root README. Launch **Agent Overflow**, and confirm the local app works.
2. In the [Tailscale DNS settings](https://login.tailscale.com/admin/dns),
   enable MagicDNS and HTTPS certificates. HTTPS is required for the Android
   shell. Tailscale's confirmation explains that certificate names appear in
   public certificate-transparency logs; the app itself stays tailnet-only.
3. In Agent Overflow → Settings → Remote access → Tailnet, enable the node
   and save. Leave the coordination-server field empty for ordinary Tailscale.
   Open the sign-in link shown there and approve the node on your tailnet.
4. Wait for `Running` and the `https://agent-overflow.…ts.net/` address.
   If Tailscale assigned a suffix to avoid a duplicate name, use the name shown
   by the app. No LAN binding, canonical domain, certificate file, Tailscale
   Serve, Funnel, router forwarding, or public firewall opening is needed.
5. Keep Agent Overflow running and the Mac awake when using the phone. The
   app's keep-awake setting can help; quitting the backend or sleeping the
   computer makes it unreachable. For a dedicated unattended installation,
   see [serve-mode.md](serve-mode.md) before installing a service. Do not run
   a desktop backend and a service against the same data directory together.

Official references: [tsnet](https://tailscale.com/docs/reference/tsnet-server-api)
and [HTTPS setup](https://tailscale.com/docs/how-to/set-up-https-certificates).

## Install and pair the phone

1. Download `agent-overflow-android.apk` from the same GitHub release as the
   desktop. On the Pixel, open the download and allow that browser or file
   manager to install unknown apps when Android asks. This is an APK install,
   not a phone reflash. Release assets include `SHASUMS256`.
2. Connect the Pixel's Tailscale app to the host's tailnet. Set up a phone
   screen lock: Agent Overflow uses Android's biometric/device-credential
   prompt when it opens.
3. On the Mac, open Settings → Remote access → Devices → Pair a device →
   Phone or tablet. Choose Full access to drive agents and answer approvals.
4. Open Agent Overflow on the Pixel and use its **in-app QR scanner** to
   scan the desktop's code (or paste the pairing link). Verify that both
   screens show the same number, then allow the device on the Mac.
5. Test opening a thread, sending a message, answering an approval, and
   opening the terminal. Then background/resume the phone, briefly disconnect
   and reconnect Tailscale, and test over cellular with Wi-Fi off. Reconnection
   should recover automatically; offline content is read-only.

A QR invitation expires and is single-use. Generate a fresh one if pairing did
not complete. Keep the app installed: uninstalling discards its key, pairing,
and cached threads. A normally signed APK update preserves that data. A
previous **debug** install has a different signing key and cannot be updated
in place with the release APK; uninstall it once, install the release APK,
and pair again. Do not use the destructive emulator smoke to install onto a
phone you have already paired for daily use.

The backend supplies newer web bundles automatically. Native/plugin changes
still require an APK update; a bundle cannot install a native plugin. APKs
must keep the same signing key and application id across releases. Android's
[signing](https://developer.android.com/studio/publish/app-signing) and
[versioning](https://developer.android.com/studio/publish/versioning) rules
apply even to private sideloaded apps.

## Test production builds before a release

On the Mac, `make build` creates the production app at
`bin/agent-overflow.app`. To exercise the normal installer without cutting a
release, package that build locally:

```sh
make build
mkdir -p dist/local-test
(cd bin && zip -qr ../dist/local-test/agent-overflow-darwin-arm64.zip agent-overflow.app)
./scripts/install.sh --macos ./dist/local-test/agent-overflow-darwin-arm64.zip
```

Quit the previous production app before installing/reopening it. Development
and harness instances have their own identities; configuring a harness does
not configure your production host. `make release-macos` is the formal
clean-tree release path after changes and release metadata are committed.

For Android, `make apk` builds the debug APK from the production SPA, suitable
for the emulator smoke. `make apk-release` builds a non-debuggable, signed APK
with release network policy and writes
`dist/release/<frontend-version>/agent-overflow-android.apk`.

Keep a local signing environment outside the checkout, for example at
`~/Library/Application Support/agent-overflow-release/android/signing.env`,
with access restricted to your account. Load it before building:

```sh
source "$HOME/Library/Application Support/agent-overflow-release/android/signing.env"
make apk-release
```

Release signing uses four environment variables:

- `AO_ANDROID_KEYSTORE`: absolute path to the persistent keystore.
- `AO_ANDROID_STORE_PASSWORD`: keystore password.
- `AO_ANDROID_KEY_ALIAS`: signing key alias.
- `AO_ANDROID_KEY_PASSWORD`: key password.

Keep these outside the checkout and load them into the build environment;
never commit signing keys or passwords. Back up the keystore and its passwords
securely: losing them prevents updates to existing installations. The build
refuses missing signing configuration and verifies the resulting signature.
`mobile/shell-build.txt` is the APK version code: increase it for each APK
release, including native compatibility changes. The displayed version comes
from `frontend/package.json`; web-bundle-only updates do not require a new APK.

The tag/manual GitHub workflow builds Android alongside the desktop artifacts,
then includes it in the checksum manifest. Configure these repository secrets
before running it:

- `AO_ANDROID_KEYSTORE_BASE64`: base64 bytes of the same keystore used locally.
- `AO_ANDROID_STORE_PASSWORD`, `AO_ANDROID_KEY_ALIAS`, `AO_ANDROID_KEY_PASSWORD`.
- Optional `AO_ANDROID_GOOGLE_SERVICES_BASE64`: base64 bytes of the Firebase
  Android app's `google-services.json` for `dev.agentoverflow.app`.

A missing signing key fails the release instead of distributing an APK signed
with an ephemeral runner's debug key. A manual workflow build uploads artifacts
without publishing a GitHub release. A version tag publishes only after all
platform jobs pass. Local APK builds do not run the full desktop release gate;
run `make verify` before cutting a release.

## Notifications and troubleshooting

Background push additionally requires the APK's Firebase configuration and a
matching Firebase service-account credential in the Mac's Settings →
Notifications → Phone push. Allow notifications on the Pixel. These are
separate from Tailscale and pairing; connecting to threads does not need FCM.
The service-account key stays on the backend and must never be put in the APK.
See [mobile/AGENTS.md](../../mobile/AGENTS.md#push) for implementation and the
optional real-push smoke.

| Symptom | Check |
|---|---|
| Tailnet waits for login | Open and approve the app node's sign-in link; signing in the Mac's separate Tailscale app does not enroll it. |
| Tailnet URL is HTTP | Enable MagicDNS and HTTPS certificates in the tailnet admin panel. The app observes changes without restart. Check the reported tailnet error if TLS cannot attach. |
| QR names `127.0.0.1` or a LAN IP | The app node is not yet reachable. Finish tailnet setup and mint a fresh invitation. |
| HTTPS name cannot be reached | Check phone Tailscale, both devices' tailnet membership, ACL access to the app node's TCP 443, host sleep, and whether the backend is running. |
| Works on Wi-Fi, fails on cellular | Keep Tailscale connected on cellular; verify the tailnet path and relay availability. LAN addresses do not work off-site. |
| Pairing fails after reinstall | Revoke/forget the old device entry if desired, then pair the fresh installation. |
| Android says the app cannot update | Check signing-key continuity and APK version code; debug-to-release needs a one-time uninstall/re-pair. |
| Threads work, background notifications do not | Check Android permission, Firebase configuration in the APK, and backend Phone push status separately. |

Verification is layered: Go tests cover identity, revocation, TLS/listeners and
a local fake tailnet; client tests cover renewal/reconnect; browser harness
specs cover off-host pairing and live state; `make e2e-android` covers the real
WebView and native seams on an emulator using the debug APK's loopback
exception. It does not verify the release APK's HTTPS connection. Real
Tailscale sign-in, public
certificate issuance, Pixel biometrics, and cellular/DERP reach still require
the device checks above. A green local suite alone does not prove those paths.
