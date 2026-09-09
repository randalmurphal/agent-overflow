# Remote access: Mac host and Android phone

The Mac runs the backend and providers. The Android APK is a client of that
backend; an Android system image, Docker image, or public server is unnecessary.
Agent Overflow joins Tailscale as its **own node**, normally `agent-overflow`.
The Mac's separate Tailscale app does not have to be connected for that node to
serve the phone. For tailnet access, the phone's Tailscale app must be connected
to the same tailnet. A private LAN connection can work without Tailscale.

## Set up the host

1. Install the macOS release with `scripts/install.sh`, as described in the
   root README. Launch **Agent Overflow**, and confirm the local app works.
2. In the [Tailscale DNS settings](https://login.tailscale.com/admin/dns),
   enable MagicDNS and HTTPS certificates. HTTPS is required for the Android
   shell. Tailscale's confirmation explains that certificate names appear in
   public certificate-transparency logs; the app itself stays tailnet-only.
3. In Agent Overflow → Settings → Remote access → Allow device access → Tailscale, enable the node
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
2. Connect through the same LAN or the host's tailnet. Set up a phone
   screen lock: Agent Overflow uses Android's biometric/device-credential
   prompt when it opens.
3. On the Mac, open Settings → Remote access → Allow device access → Allow a device to connect.
   Choose **Local network** for a phone on the same LAN with Tailscale off, or
   **Tailscale** when the phone is connected to the tailnet, then **Phone or tablet**. Choose **My device** to drive agents, answer approvals, and join your personal
   devices. Use **View only** for limited access without joining the group.
4. Open Agent Overflow on the Pixel and use its **in-app QR scanner** to
   scan the desktop's code (or paste the pairing link). Verify that both
   screens show the same number, then allow the device on the Mac.
5. Test opening a thread, sending a message, answering an approval, and
   opening the terminal. Then background/resume the phone, briefly disconnect
   and reconnect Tailscale, and test over cellular with Wi-Fi off. Reconnection
   should recover automatically; offline content is read-only. Opening offline
   should show the connection banner without a burst of load-error toasts.
   Reconnecting restores saved panes and refreshes computer settings;
   an unavailable computer must not clear their last good state. Phone shortcuts
   load and save on the phone itself, independently of paired computers.

The network choice controls the invitation's initial connection, not a separate
pairing identity. After connecting, the device learns the host's other enabled
routes and verifies them before switching. You do not need separate LAN and
Tailscale pairings. A Tailscale URL still requires Tailscale to make that first
connection; it cannot bootstrap an offline phone over LAN. Older hosts choose
the invitation address automatically; update the host to choose explicitly.

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

Installing a new APK starts with its packaged UI, replacing any previously
downloaded web bundle while preserving pairing and frontend data. Later web
updates still apply normally. Reopening the same APK retains those web updates.

Pairing survives backend restarts. The phone renews its short-lived access
credential automatically; each successful renewal starts a fresh 30-day
refresh window. This is an inactivity limit, not monthly re-pairing.

## LAN access and changed addresses

Enable LAN access on the host's Remote access → Allow device access page, then pair through its invitation.
Use an APK with the native Network plugin: the invitation carries the private
certificate fingerprint, so Android verifies the host without installing a
system-wide CA. An older APK can continue using its public HTTPS tailnet route.

A reachable host advertises its enabled LAN and tailnet routes. Installed
clients remember a bounded set and verify the computer before changing routes.
Enabling and signing into Tailscale later updates connected clients automatically;
you do not need to pair again or restart them. Let the devices connect once while
a saved route still works so they can learn the new address. A device that was
offline cannot learn a new address after every address it knows becomes unreachable.
Switching routes preserves the pairing, conversations and frontend preferences.
It does not replay a failed command or upload automatically.

If the host changes IP or port and every saved route is unreachable, open
Settings → Remote access → Connect to a computer → the offline computer → Change address. Enter its new
HTTPS address and choose Verify & reconnect. This reuses the saved pairing's
trust. A replacement certificate or an unfamiliar public hostname may require
a new pairing link. A healthy saved route can advertise updated addresses and
certificate pins without this manual step.

## Device names

In **Remote access → Connect to a computer**, edit **Device name** to choose how this
installation appears on other devices. A phone saves its name locally, including
while offline. Connected computers receive changes immediately; offline ones
receive them on reconnect. Older computers must be updated to accept name changes.

To rename a host from your phone (including a headless host), select that computer
in settings and open **Remote access → Allow device access → Device name**.
Renaming changes no addresses, pairing keys, or permissions. A **Nickname** on a
connection remains an override visible only on the frontend where you set it.
Clear the name field and save to use the default again.

An older phone pairing mislabeled with the host's name is corrected when you
save the phone's Device name. No uninstall or re-pairing is needed. Existing
labels are not automatically guessed or overwritten on upgrade.

## Test production builds before a release

For the full installation rehearsal, use the same signed artifacts that will
eventually be published:

1. Sync the next version with `scripts/sync-release-version.sh`, review and
   commit the metadata, and run `make verify`. Keep Android's persistent signing
   secrets and matching Firebase app configuration in GitHub Actions as below.
2. Run **Actions → Release build → Run workflow** against that commit's branch.
   Leave the version input empty to use the committed version. This builds all
   platforms and uploads artifacts without creating a tag or GitHub release.
3. Download `agent-overflow-release-<version>` from the successful run and
   extract it. Use its included installer on Mac or inside WSL:

   ```sh
   sh ./install.sh --download --source .
   ```

   Run this from the extracted directory. It detects the platform and verifies
   the packaged checksums. Restart the app after installing to run the new version.
4. On the phone, download the smaller `android-raw` artifact from the same run,
   extract it in Files, and open the APK to install/update it. No USB or wireless
   debugging is needed. Allow installs from that browser or file manager if
   Android asks. GitHub Actions artifact downloads require GitHub sign-in.
5. Configure the host and pair through the ordinary settings screens. Existing
   signed installations keep their data; do not uninstall merely to update.
   Test push delivery and opening its thread with the sender configured below.

Keep the installed hosts on the candidate while testing: a newer host may
deliver a newer frontend bundle to the phone. The download page differs from
the eventual Releases page, but the binaries and installation paths are the
production ones. Desktop publisher signing is unchanged; Gatekeeper or
SmartScreen may still require first-launch confirmation as described in README.

A later version tag promotes the matching successful candidate's saved files;
it does not rebuild them. Promotion requires the exact tagged commit and version
and verifies the candidate manifest and checksums. Missing, expired, or ambiguous
candidates fail rather than substituting another build. Keep the tested run's
artifact until publication. A candidate is not offered by the public in-app
update checker before it is published.

For a local production build during development:

On the Mac, `make build` creates the production app at
`bin/agent-overflow.app`. To exercise the normal installer without cutting a
release, package that build locally:

```sh
make build
mkdir -p dist/local-test
(cd bin && zip -qr ../dist/local-test/agent-overflow-darwin-arm64.zip agent-overflow.app)
./scripts/install.sh --macos ./dist/local-test/agent-overflow-darwin-arm64.zip
```

Building or installing on macOS can leave the current app running: the new
bundle replaces its path, while the original signed bundle is retained in a
hidden sibling directory until no process uses it. Later builds/installs clean
up those retired copies. Fully quit and reopen to run the new version; building
alone does not update a separate installed copy. The macOS installer needs the
packaged `macos-bundle.sh` asset beside `install.sh` for local artifacts;
`--download` fetches and checksum-verifies it automatically.

Ad-hoc signing does not guarantee stable local-network permission identity
across builds. Apple recommends an Apple-issued signing identity for that:
[local network privacy](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).
If LAN access fails after changing builds, check System Settings → Privacy &
Security → Local Network for Agent Overflow. Preserving bundles avoids invalid
running code; it cannot override a denied OS permission.

Development and harness instances have their own identities; configuring a harness does
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

The manual GitHub workflow builds Android alongside the desktop artifacts,
then includes it in the checksum manifest. Configure these repository secrets
before running it:

- `AO_ANDROID_KEYSTORE_BASE64`: base64 bytes of the same keystore used locally.
- `AO_ANDROID_STORE_PASSWORD`, `AO_ANDROID_KEY_ALIAS`, `AO_ANDROID_KEY_PASSWORD`.
- Optional `AO_ANDROID_GOOGLE_SERVICES_BASE64`: base64 bytes of the Firebase
  Android app's `google-services.json` for `dev.agentoverflow.app`.

A missing signing key fails the release instead of distributing an APK signed
with an ephemeral runner's debug key. A manual workflow build uploads artifacts
without publishing a GitHub release. A version tag publishes the matching
successful candidate after verifying its saved artifacts. Local APK builds do not run the full desktop release gate;
run `make verify` before cutting a release.

## Notifications and troubleshooting

For your own devices, push travels from the running host to Google's Firebase
Cloud Messaging (FCM), then to Android. The host needs outbound internet access;
no additional cloud server, public listener, or router forwarding is needed.
Opening the notified thread still needs a working LAN or tailnet connection.
Any phone paired to that host can receive its notifications; the sender
credential belongs on the host, not on each phone.

1. Create a Firebase project and register its Android app with package name
   `dev.agentoverflow.app`. Download `google-services.json` following
   [Firebase's Android setup](https://firebase.google.com/docs/android/setup).
   This is app configuration, not a private sender credential. Put it in
   `mobile/android/app/google-services.json` for local builds, or store its
   base64 bytes as `AO_ANDROID_GOOGLE_SERVICES_BASE64` for GitHub builds.
2. Enable the Firebase Cloud Messaging HTTP v1 API. Create a dedicated service
   account in that project with the **Firebase Cloud Messaging API Admin** role
   and generate its JSON key. The host's current sender uses that key to
   authorize requests; see [FCM authorization](https://firebase.google.com/docs/cloud-messaging/send/v1-api).
3. On each trusted computer that should send push, save that private JSON key
   in Settings → Notifications → Phone push. Never put this key in the APK,
   source control, or another user's backend. The Firebase project must match
   the one configured in the APK; a credential from an unrelated project will
   not work. This is one-time setup for the trusted host, not a Firebase account
   setup step for every person installing the phone app. App updates preserve it.
4. Build/install the configured APK, unlock it and allow notifications. Enable
   the desired notification kinds on the host. Background the phone normally,
   finish a turn on the host, and verify both delivery and tap-to-thread.
   Android force-stop is not ordinary backgrounding; do not use it for this
   delivery check. Phone push status on the host reports sender failures.

This direct sender is intended for computers you trust with your Firebase
credential. A public APK can carry the app configuration, but its users must
not receive your private sender key. Push without per-user Firebase setup needs
an authenticated relay that holds the sender credential and restricts each
host to its authorized phones; that relay is not implemented. Independently
managed Firebase projects currently need matching APK builds. Core remote
access works without FCM or a relay.

See [mobile/AGENTS.md](../../mobile/AGENTS.md#push-notifications) for implementation and the
optional real-push smoke.

| Symptom | Check |
|---|---|
| Tailnet waits for login | Open and approve the app node's sign-in link; signing in the Mac's separate Tailscale app does not enroll it. |
| Tailnet URL is HTTP | Enable MagicDNS and HTTPS certificates in the tailnet admin panel. The app observes changes without restart. Check the reported tailnet error if TLS cannot attach. |
| QR names `127.0.0.1` | Enable LAN access or finish tailnet setup, then mint a fresh invitation. |
| QR names a LAN IP | Works on that reachable LAN with the current APK. For off-site access, enable the app's tailnet node. |
| HTTPS name cannot be reached | Check phone Tailscale, both devices' tailnet membership, ACL access to the app node's TCP 443, host sleep, and whether the backend is running. |
| Works on Wi-Fi, fails on cellular | Keep Tailscale connected on cellular; verify the tailnet path and relay availability. LAN addresses do not work off-site. |
| Pairing fails after reinstall | Revoke/forget the old device entry if desired, then pair the fresh installation. |
| Android says the app cannot update | Check signing-key continuity and APK version code; debug-to-release needs a one-time uninstall/re-pair. |
| Threads work, background notifications do not | Check Android permission, Firebase configuration in the APK, and backend Phone push status separately. |

Verification is layered. Go tests cover identity, revocation, TLS/listeners and
a local fake tailnet; client tests cover the connection state machine and
compatibility contracts. Recovery also needs these composed checks:

| Failure boundary | Required observable result | Exercised by |
|---|---|---|
| A replay disconnects, then another replay stalls despite traffic; the user switches threads and sends | The new watched thread recovers its timeline and running state, the send appears once, and its next draft survives | Compact interrupted-recovery browser test |
| One execution host crashes during a turn while another remains up | Navigation and sends on the surviving host work; the restarted host restores history and accepts work without reloading or pairing again | Desktop/compact multihost recovery flow; the optional older-artifact mode separately checks graceful restart compatibility |
| Android pauses the app, loses LAN, and the provider finishes offline | The same app process restores the answer and idle state after resume and network recovery | Android lifecycle cases, including the signed APK through native accessibility |

The test files own their scenarios; [the harness guide](../../e2e/AGENTS.md)
owns commands and artifact selection. A connection badge, visible cached rows,
or successful RPC alone is insufficient recovery evidence: check the rendered
answer, active/idle state, and message/draft ownership after the fault resolves.

The ordinary `make e2e-android` uses a debug APK and additionally checks native
pickers, pinned HTTPS, renewal, attachments, and frontend bundle updates. Its
signed-release mode exercises a non-debuggable APK without a WebView debug
connection. Both use an emulator; neither proves Pixel-specific biometrics,
real Tailscale sign-in, public certificate issuance, or cellular/DERP reach.
Those still require the device checks above. A skipped device test is not a pass.

## Connect two desktops

1. On the computer you want to work on, open **Settings → Remote access →
   Allow device access**. Enable **Local network** for LAN or sign AO's Tailscale
   node into your tailnet for remote access.
2. Choose **Allow a device to connect → Another computer** and leave that window open.
3. On the other desktop, open **Remote access → Connect to a computer**, select the
   computer shown under discovery, and connect. If it does not appear, type
   the address shown in its pairing window; no invitation link is needed.
4. Compare the six digits on both computers, then approve on the host.

For automatic tailnet discovery, enable AO's Tailscale node on both desktop
apps. The separate OS Tailscale app can route a typed address but does not
provide the embedded node's discovery list. A paired computer learns both
available routes; connecting again for LAN versus Tailscale is unnecessary.

On Windows, the desktop launcher exposes LAN access through its native
interfaces even when the backend runs inside WSL NAT. Allow Windows' normal
Private network firewall prompt if shown. AO does not change firewall rules or
WSL networking settings. Forwarding errors appear in the LAN settings.
Closing or restarting the host cancels unfinished setup; completed pairings
survive. Older builds still use invitation links. Protocol and validation:
[computer pairing](computer-pairing.md).

## Personal devices

Personal pairing (including `agent-overflow pair --lan` and first-run headless
setup) joins the devices' existing personal groups. Machines connect directly
with independent keys and sessions; one initial machine need not remain online.
Joining a desktop enables its LAN listener for reciprocal access. Existing
Tailscale settings are retained; a remote-only return connection still needs
that computer's Tailscale node to be configured. Removing a local connection
keeps it excluded on that frontend; revoking a personal device removes its group
membership as the other machines reconnect. Removing a computer from the group
withdraws its personal inbound sessions too; ordinary limited shares remain
independent. Offline computers cannot learn a removal until connectivity returns.

A phone paired with two previously separate computers joins their personal
catalogs and helps establish direct connections between them. It does not need
to remain open after those independent connections exist. Existing ordinary or
older full-access pairings do not automatically join the group: use a new
personal pairing to enroll them. Limited thread sharing never joins personal
devices, and agent remote-command permissions remain opt-in.

## Headless computers

On a desktop client, `agent-overflow --frontend` opens your saved computers
without starting a local execution host. Add a computer in Settings → Remote access → Connect to a computer,
or use `agent-overflow --connect '<invitation>'` for terminal pairing. Subsequent
frontend launches and updates work even when the originally paired host is off.

On a computer with the release binary on PATH, run `agent-overflow service
install`, then `agent-overflow pair --lan`. Open the invitation on the client,
compare the numbers, and enter the six digits in the terminal. The service
keeps running after the terminal or SSH connection closes. Existing services
can pair more devices with the same command. `service start`, `service stop`,
and `service status` control an installed host without its desktop app.

A Mac LaunchAgent requires a logged-in user. A Linux user service needs
lingering if it must survive logout; installation reports that command but
does not change it automatically. See [serve mode](serve-mode.md).
