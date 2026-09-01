# Serve mode

`agent-overflow serve` runs the backend with no window, on a machine its
owner reaches from somewhere else. It is the deployment shape behind
`docs/specs/remote-access.md` §7: one always-on host with the projects and
the provider logins on it, and every screen the owner uses — a laptop
browser, a phone, a second machine's `agent-overflow --connect` — attached
to it over the transport.

This document is the operator's copy. The wire, the credentials and the
authorization rules are the spec's; what is here is what a person does.

## What serve is

```
agent-overflow serve [--listen <host:port>] [--data-dir <path>] [--reset-transport-port]
```

It is a BOOT MODE, not a CLI command. The verb selects which shell the
process runs in — the same choice `--harness` and `--connect` make — and
everything after it is an ordinary boot flag. `agent-overflow help` lists
it because a person types it; `internal/aocli` does not route it, because a
boot needs the embedded frontend assets and the whole transport/App graph,
which live in package `main`.

Three flags survive the verb, and they are the three that configure this
boot:

| Flag | Effect |
|---|---|
| `--listen <host:port>` | Bind exactly here, this launch only. Overrides the saved bind and the saved port, and writes neither. |
| `--data-dir <path>` | Put the app's data root somewhere other than the default config root. |
| `--reset-transport-port` | Discard the pinned port and take a fresh one. |

Anything that names a different mode is refused with a sentence saying so:
`--connect`, `--harness`, `--soak`, `--window`, `--print-url-fd`,
`--mock-provider`, and (transitively, because each already requires
`--soak`) the launcher-identity flags.

Running `serve` from inside an Agent Overflow agent session is refused too.
`AO_ENDPOINT` in the environment means this shell is already talking to a
backend, and starting a second one from inside the first is almost never
what somebody meant.

## What it does differently from a windowed boot

**It honors your saved network settings.** The bind toggle, the canonical
domain, the DNS hook and the tailnet switch from Settings → Network all
apply at boot, so a host reboots back onto the address it was reachable at.
An explicit `--listen` still wins for this launch, because naming an
address on the command line is an override on purpose.

**It prints where it is.** At ready, on stdout:

```
Agent Overflow is serving on 0.0.0.0:7777
  Open:    https://ao.example.com/
  Tailnet: https://host.tail1234.ts.net/
  Token:   <this launch's token>
```

The bound address is always printed. The rest appears only when it is a
fact — the URL comes from the same formatter Settings → Network reads, so
the two cannot disagree. A cleartext LAN URL prints a warning naming the
remedy, because that URL carries the token in the open.

The tailnet line is usually absent at this moment even when the node is
enabled: bring-up is asynchronous and a first sign-in is interactive.
Settings → Network is where you watch that finish.

The token is the launch credential. It is what a same-host
`agent-overflow --connect` uses to attach. A device on another machine
pairs instead and never sees it.

**It keeps credentials in files, not a keychain.** See below.

**It offers to pair your first device.** See below.

## First-device enrollment

Pairing needs an owner surface: something already trusted, that shows you
the verification number the new device is displaying and takes your yes or
no. Every other boot mode has a window. A serve host has a terminal, so on
a first interactive boot the terminal becomes it.

When you run `serve` from a terminal on a backend with no device that could
reach it, the console prints a pairing link, waits for a device to open it,
prints the verification number, and asks:

```
No device is paired with this backend yet.
Open this link on the device you want to use:

  http://192.168.1.20:7777/#pair=...

It works once, and it stops working in a few minutes.

Chrome on iPhone opened the link and is showing a number.
This backend's number is: 471 208

Confirm only if the device shows the same number.
Do the numbers match? [y/N]
```

Answer `y` only if the device shows the same number. Anything else cancels
the link, and the device holds nothing.

Details worth knowing:

- The device is enrolled as a **browser** with **full** access. Browser
  because that is what you are holding; full because this is the owner's
  first device on a backend with no other way in, and a view-only first
  device could not enroll a second one. Narrow a later device per-device
  in Settings → Access.
- "No device that could reach it" excludes two rows. The **local page
  channel** is this backend's own row, resolved on every boot for a window
  serve never opens. A **revoked** device is not access — so if you revoke
  your last device on a headless host, the next interactive `serve` offers
  enrollment again, which is the only way back in.
- The offer happens **once per boot**, on a goroutine, while the backend
  serves. Ctrl-C during it shuts the backend down normally.
- Nothing here is a new identity rule. The console calls
  `MintDevicePairing`, `DevicePairingStatus` and `ConfirmDevicePairing` —
  the same methods the settings screen calls, in the same order — and the
  session core applies its own single-use link, proof of possession,
  expiry and confirmation window to each. Minting requires step-up, and an
  in-process call satisfies it as the host-present caller
  (`internal/app/app_authz.go` names that class); holding a terminal on the
  machine is the standing-at-it that requirement already recognises.

### Under a service manager

systemd and launchd hand a service `/dev/null` or a socket, not a terminal,
so there is nobody to compare a number with. In that case serve does not
mint a link at all. It logs one line:

> No device is paired with this backend, and nothing here can confirm a new
> one: stop the service and run `agent-overflow serve` from a terminal once
> to pair your first device.

Do that once, then start the service. After the first device exists, every
later device is paired from Settings → Access on the device you already
have, and the console never says anything again.

## Bind and port

Settings → Network holds both halves of the address, and a serve host
reads them at boot.

**Allow remote access** decides the host: off binds `127.0.0.1`, on binds
`0.0.0.0` so other machines can reach it.

**Port** decides the port. Leave it blank and Agent Overflow takes one on
first launch and keeps reusing it — fine for a desktop install, and not
what you want on a serve host, where the number is in every share URL and
every pairing link. Set it and that port is the one this install owns.

Precedence at boot, highest first:

1. `--listen host:port` on the command line. One launch, and it writes
   nothing: a debugging run must not move where the install lives.
2. The saved **Port**.
3. The port this backend last bound (`transport-port.json`, a cache).

A saved port that cannot be bound is a boot FAILURE naming the setting,
not a quiet move to somewhere else. That is deliberate: a backend nobody
can find at the address they have is worse than one that did not start.
Under systemd, `Restart=on-failure` plus `systemctl --user status
agent-overflow` is how you see it. Fix it in Settings → Network from a
device that is already paired, or start once with `--listen` to get in.

### What changing the port costs

The port is part of the origin, so it is not a cosmetic setting.

- **A browser** loses its session: the page cookie's name carries the
  port, and everything it saved (its local state and its offline thread
  copy) is scoped to the old origin. It has to pair or sign in again.
- **A paired app** (`agent-overflow --connect`) stores the endpoint the
  pairing link named and never rediscovers it. It keeps dialing the old
  port and reports a connection failure; the credential is not cleared,
  but it cannot reach the backend. Pair it again from the moved backend
  and the new link overwrites the endpoint.
- **Live connections** finish, then cannot reconnect.

Clearing the port back to blank does NOT move the listener. It means
"stop pinning this", and the backend stays where it is.

## Credential storage

A serve host stores every secret it holds in files under the config root,
mode 0600, owned by the user the process runs as:

| What | Where |
|---|---|
| Provider account credentials | the provider-accounts store under the config root |
| The browser companion's state key | the same root |
| The backend's own TLS certificate | one combined PEM under the config root |
| Device, session and pairing rows | the SQLite database under the config root |

Serve deliberately does not use the OS keychain, and the reason is that it
cannot rely on one. On Linux the Secret Service lives in the desktop
session's D-Bus; on macOS the login keychain is unlocked when a person logs
in. A backend started by systemd at boot, or by launchd before anyone signs
in, would either block on a prompt nobody can answer or silently fall back
— and "my provider logins disappeared after a reboot" is the shape that
second outcome takes.

What that means for you: **the config root is the secret.** Back it up as
one, restrict it as one, and do not put it on a share other accounts can
read. A full-disk-encrypted host protects it at rest; a running one
protects it with file permissions and nothing else.

## Platforms

| Platform | How serve runs |
|---|---|
| Linux | The ordinary binary, or a build with `-tags nogui` that links no GTK or WebKit at all. |
| macOS | The ordinary binary. `serve` never opens a window, so there is nothing a separate headless build would save. |
| Windows | Not directly. The Windows install is a launcher hosting a Linux backend inside WSL, and that launcher is already the supervisor. |
