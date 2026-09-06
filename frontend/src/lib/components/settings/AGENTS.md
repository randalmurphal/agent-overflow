# components/settings/

The Settings overlay: a nav rail of pages, one page component per topic,
and a search box over every control.

## Adding or moving a control

Three files describe a control, and a test ties them together.

- `sections.ts` is the page list: id, rail label, group, and the one-line
  description `SettingsView` renders above the page. A page component never
  renders its own title. In-page sub-sections use `SettingsHeader` with a
  sentence-case title, and a page that is a run of sections wraps them in
  `.settings-sections` (app.css), which rules and spaces between siblings;
  no section carries its own top margin or border. Remote access uses
  separate bordered cards for pairing and each network instead of a text wall.
- `pages.ts` maps each page id to its component; the record is total over
  `SettingsSection`, so a page without a component does not compile.
- `fields.ts` is the search index. Every `SettingsField` takes a required
  `id: SettingsFieldId` from it, so a control cannot be added without
  becoming searchable and a typo is a compile error. A block that is not a
  labelled row (a radio group, the accounts list, the auto-compact sliders)
  stamps `data-settings-field` and `data-settings-label` on its root itself.
  Shared provider controls use `providerFieldId(provider, slug)`;
  provider-specific ones are static entries under that provider's page.

`fields.test.ts` mounts every page with the shipped defaults and fails on a
registered id that does not render, a rendered anchor that is not
registered, or a label or hint that differs from the index. Mark an entry
`conditional` only when the default fixture genuinely cannot show it (a
platform-gated block, a control behind another setting). The index's
`label` and `hint` are canonical: change the row's copy and the index in
the same edit.

`keywords` carry what neither label nor hint says ("sleep" for the
keep-awake toggle). Search matches every query token against label, hint,
keywords, in-page heading, page label and group, and ranks label hits
first (`settingsSearch.ts`). A hit opens the page and `revealSettingsField`
scrolls to and flashes the anchor.

## Notifications

`NotificationsSection.svelte` is one page with THREE stacks, and the split
is the design rather than layout. The per-kind toggles under the master
switch answer "is this moment worth an interruption" — one per
`notify.Kind`, so nothing is silenceable only by turning everything off.
The **Quiet when** picker answers a different question, "is this screen
already looking", and its one key (`notifyQuietWhen`, four exclusive
readings) is read on the BACKEND MACHINE's own screen alone (`internal/app/app_notifications.go`); a paired phone is woken
either way, which is what its description says. The phone-push block stays
at the FOOT, because it answers the same question one screen down.

Phone push currently sends directly through Firebase. Its credential hint must
say that the service account belongs to the Firebase project built into the
Android APK; configuring an unrelated project cannot enable push for that APK.

Two rules for editing it:

- **The master switch HIDES rather than disables everything beneath it**,
  the SpinnerSection pattern. A stack of greyed-out toggles reads as broken.
- **Every per-kind toggle defaults ON, and "Quiet when" is a radio picker,
  not toggles** (default: this window is focused). It is one picker because
  the reading most people want, quiet about an open thread only while the
  app is focused, is the AND of two facts that independent toggles cannot
  express. `NotificationsSection.test.ts` treats it separately for exactly
  that reason.

## Deep links

`openSettingsOverlay(section)` takes a page id. A provider-scoped caller
routes through `providerSettingsSection(providerId)`, which lands a
dependent provider (Claude TUI) on its parent's page and throws for a
provider with no page rather than guessing.

## Provider pages

`ProviderSettingsPage.svelte` renders one provider's whole page (setup,
environment, context window, system prompt, tools, and the
Claude-only session sections). `ClaudeSettings` / `CodexSettings` are the
two instantiations. Every section renders regardless of the Enabled
toggle; there is no "enable it first" gate.

## Device pairing

The reachability warning considers both LAN binding and a running tailnet;
`GetNetworkSettings` is readable with `access:admin`, including on paired
phones. Unknown reachability must not recommend changing a working network.
`DevicesSection.test.ts` covers a tailnet-only host. PairDeviceModal reads the
captured computer's `pairing.networks.v1` capability before offering Local
network / Tailscale. It loads enabled networks before minting, defaults to LAN
when enabled, and sends the explicit choice without changing host settings.
Older hosts retain their automatic mint RPC. Never label an automatically
selected tailnet URL as a LAN invitation.

Remote access has four visible navigation pages: Connections, Pairing & network,
Accounts, and Agent access. Accounts owns both providers' sign-in controls;
provider configuration pages keep model and runtime settings. Account-switcher
management links open Accounts on the captured computer.

Pairing & network puts pairing first, followed by LAN and Tailscale cards.
Ports, domains and certificates stay under Advanced network settings; passkeys
and individual connection sessions are collapsed separately. Connections groups
existing computers before setup and exposes hostname-based labels and an explicit
nickname editor. Nicknames belong to this frontend, use stable backend identity,
and never rename the host or alter its connection address. Save/Cancel is explicit.
Setup help includes the destination page and headless service commands.

An offline saved connection exposes `ComputerAddress`. Its repair belongs to
the frontend's pairing, not settings on the selected computer. Go's host-only
`RepairBackendAddress` and the native route controller verify existing trust
before saving. Keep errors beside the entered address, leave failed input
editable, and reconnect only the captured connection if it still exists.

## Computer ownership

SettingsView captures a computer independently of the active thread. Its keyed
ComputerSettingsPage provides settingsComputer context to every child editor:
use that context's getters, mutators, scope checks, and `call` for a single
synchronously dispatched RPC. Remount on computer changes so a pending form
cannot write to a newly selected host. Provider-account and model stores also
receive the captured backend explicitly. Frontend preferences are local to this
frontend and overlay every computer's redacted settings snapshot.

A page's initial read belongs in onMount. Calling a loader that reads its cache
inside an effect subscribes the effect to the response it writes, creating an
unbounded read loop. Refresh subsequent changes through the settings event and
reconnect paths; never by observing a whole settings snapshot in that loader.

The editor preference is configurable on the selected computer with
`settings:read` / `settings:write`. Discovering its installed editors is a read of
that computer’s settings; launching an editor remains `host`-scoped. A phone can
configure a computer’s preference without being allowed to launch its desktop.
# SSH setup

`SSHConnectModal` is desktop/host-only and uses the existing AddBackend profile
flow. It compares both verification numbers before explicit confirmation.
Closing before confirmation cancels even late start/redemption responses;
closing during confirmation preserves the profile because the remote may have
accepted before its reply arrives. The installed service outlives its console.

Agent access is configured on the selected originating computer. Its peer
toggles never imply that two computers already share a phone's credentials.
The pair-and-enable action captures both endpoints, mints on the destination,
enrolls the source, compares backend identity and both verification numbers,
then confirms on the destination and opts in on the source. A failed comparison
cancels the pending invitation. Existing scope and step-up checks stay active.
Refresh this page on its own computer's reconnect and agent-computers:changed
event; a late read cannot replace a newer table or refill a closed page.
Existing peers appear once, outside the add selector. A failed Enable retains
an explicit Connect again action when this frontend can reach the destination;
an incomplete saved pairing must not lose its repair path after reloading.

A native select bound with Svelte's bind:value belongs in browser tests: the
installed happy-dom selector implementation recognizes :checked on inputs only,
so it cannot emulate Svelte reading a selected option. Do not alter production
select handling or fake selector results to make that DOM emulator pass.

Connection banners stay below the shared Settings/Workflows overlay layer.
On compact screens both occupy the top edge; a higher banner intercepts the
close button. The offline standalone-client browser flow exercises this seam.
