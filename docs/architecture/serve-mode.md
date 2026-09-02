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

## Running it as a service

```
agent-overflow service install [--listen <host:port>] [--binary <path>]
agent-overflow service update [--binary <path>]
agent-overflow service uninstall
agent-overflow service status
```

`install` writes the unit file and tells the platform's service manager to run
it from now on. What the unit starts is `<binary> supervise`, not
`<binary> serve` — the supervisor is the stable process the manager owns, and
the backend is its child (see [The supervisor](#the-supervisor)). Installing
over an older unit rewrites it, so an install that predates the supervisor is
migrated by running `install` again. Running it again replaces what is installed. `status` reports
what the manager says, and exits 1 when the backend is not running, so it reads
in a shell conditional. `uninstall` stops the service and deletes the unit and
NOTHING else — the config root, the history and the paired devices are all
still there, and installing again picks them back up.

**Pair your first device before installing.** A service manager gives the
backend no terminal, and the console enrollment above needs one. Run
`agent-overflow serve` by hand, pair, Ctrl-C, then install.

| Platform | What gets installed |
|---|---|
| Linux | A systemd USER unit at `$XDG_CONFIG_HOME/systemd/user/agent-overflow.service` (`~/.config/...` by default), starting `<binary> supervise`. `Restart=on-failure`, `WantedBy=default.target`. |
| macOS | A launchd LaunchAgent at `~/Library/LaunchAgents/com.agentoverflow.serve.plist`, `RunAtLoad` with `KeepAlive` on a bad exit. |
| Windows | Refused, naming the reason: the Windows install is a launcher that already supervises its backend inside WSL. Install the service INSIDE the WSL distribution, from the Linux binary there. |

`Restart=on-failure` and not `always`, on both platforms: a clean exit is you
stopping the backend, and a supervisor that restarts one of those cannot be
stopped.

### What a user service does when nobody is logged in

This is the part that surprises people, and it is different on each platform.

On **Linux**, a systemd user service lives in your login session. It stops when
your last session on the machine ends and starts again when you log in — so a
server you reach only over SSH is not serving between logins. Turn that off:

```sh
loginctl enable-linger $USER
```

`install` does not run it for you. Lingering changes how your session behaves
for everything on the machine, so it is your call, not the installer's. The
install output names the command.

On **macOS**, a LaunchAgent runs in the GUI login session. A Mac that reboots to
the login window is not serving until somebody signs in. There is no lingering
equivalent for a per-user agent; a Mac that must serve unattended needs
automatic login, or a different arrangement than this command installs.

Logs go where the platform puts them:

```sh
journalctl --user -u agent-overflow -f       # Linux
tail -f ~/Library/Logs/agent-overflow-serve.log   # macOS
```

### The two flags

`--binary <path>` names the executable the unit starts. It defaults to the
running binary's own resolved path, which is right for the usual case of one
file on `PATH`. Name a path instead when the file you actually maintain is
somewhere else — a symlink you repoint on upgrade, for instance, since the
default resolves THROUGH it and would pin the service to today's target.

`--listen <host:port>` is rarely what you want, and the reason is worth stating
plainly: a flag in the unit overrides the saved network settings on every
start. Settings → Network can then no longer move the backend — the setting
changes, the flag wins, and nothing says why. Set the bind toggle and the port
below instead, and keep `--listen` for a host whose address must be fixed
outside the app.

## The supervisor

A serve host is two processes: `agent-overflow supervise`, which the service
manager starts and which does nothing but decide what runs, and the backend it
spawns as a child. The split exists so a host can be updated without anyone
being at the machine — the process that swaps the backend cannot be the backend,
or the swap would take out the thing performing it.

**A supervisor is optional forever.** `agent-overflow serve` started by hand is
unchanged and knows nothing about any of this; the whole mechanism keys off one
environment marker the supervisor sets when it spawns a child, and its absence
means the backend behaves exactly as it did before the supervisor existed.

### What the supervisor owns

Everything lives under `<config root>/agent-overflow/runtime/`:

| Path | What it is |
|---|---|
| `service-state.json` | which version runs, and the one update record |
| `versions/<version>/agent-overflow` | staged binaries, immutable once written |
| `snapshot/` | the SQLite triple, copied while no process held it, for the length of a trial |
| `restore-marker.json` | present only while a rollback is mid-copy |

The state file is the whole selection, and the supervisor **fails closed on
one it cannot read**: an unknown schema, a version that could name something
outside `versions/`, a record in a state nobody defined. It exits non-zero and
starts nothing rather than guessing, because every guess it could make means
running a version the operator did not choose. The service manager's
`Restart=on-failure` then retries it, and it keeps refusing until a person
looks.

On a host with no state at all, the supervisor stages a copy of **its own
binary** under `versions/` and records that as active. That copy is what makes
the first update coherent: "the previous version" then names an immutable
directory rather than a file you may replace at any moment.

One consequence worth knowing: once a host is supervised, **replacing the
binary on disk and restarting the service does not change what serves.** The
replaced file supervises, and the previously selected version still runs as the
backend — which is correct (it is what keeps a committed update committed) and
is exactly the surprise worth naming. The supervisor logs a line saying so, with
`agent-overflow service update` as the fix.

### The update cycle

An update goes through six states, and the supervisor performs them one at a
time on a single goroutine, so no two can overlap:

1. **Accept.** The target's staged binary must exist and answer
   `agent-overflow __service-preflight` with a protocol this supervisor speaks.
   All of that happens before anything durable is written, so a target that
   cannot run costs nothing.
2. **Stop.** The running backend gets SIGTERM and the ordinary shutdown, so it
   closes provider sessions, flushes SQLite and drains the transport.
3. **Snapshot.** The database, its WAL and its shared-memory file are copied
   while **no process holds them**. That is the only moment the copy is safe,
   which is why it sits between the stop and the start rather than anywhere
   more convenient.
4. **Trial.** The target starts, told over the channel that it is a trial. It
   boots fully — migrations, transport bind, ready — and **answers RPCs**, but
   every subsystem that could take an action of its own waits at one gate.
5. **Prepared.** The trial says it got there. The supervisor writes the commit
   durably *first*, then deletes the snapshot, then tells the trial — which
   opens its gate and starts behaving like an ordinary backend.
6. **Or rollback.** A trial that exits, or that has not reported prepared
   within **120 seconds**, is stopped; a restore marker is written and fsynced;
   the snapshot goes back over the database; and the previous version restarts.

The trial's parked set is the second half of the rollback boundary. A restored
database undoes everything **inside** it, and nothing outside: a `git fetch`,
a refreshed provider credential, an ACME order, a retention sweep that deleted
attachment files, a workflow turn that spent real tokens. So none of those runs
until commit. Serving RPCs while parked is correct — that is what "prepared"
means.

### What you see when one rolls back

Nothing in the app changes version, and the journal says why:

```
supervise: update upd-… accepted: 1.4.0 -> 1.5.0
supervise: stopping the backend to snapshot the database
supervise: starting version 1.5.0 as a trial
supervise: rolling back update upd-…: the trial exited before reporting prepared: exit status 1
supervise: restarting version 1.4.0
```

The reason is also recorded in `service-state.json`, so it survives the log.
A client that asked for the update is told the outcome and the update's id when
the backend comes back, which is how it distinguishes "my update failed" from
"the backend restarted for some other reason".

**A supervisor killed at any point recovers from those files alone.** Killed
mid-trial, it finds the record still pending and tries again — twice, then it
rolls back, because a trial that reliably kills its supervisor would otherwise
loop forever on an unattended host. Killed mid-restore, the next boot finds the
marker and finishes the copy **before** it selects or spawns anything, so no
version ever opens a half-restored database.

### Disk

A trial needs room for a **second full copy of the database** for as long as it
runs, plus one directory per staged version. The supervisor deletes versions
nothing can select — anything that is neither running nor the one a rollback
would return to — after each commit.

### Updating locally

```
agent-overflow service update [--binary <path>]
```

This is the operator-present path, and it has no trial and no rollback for the
same reason: you are standing there. It asks the binary what version it is,
stops the unit, stages that binary under that version, selects it, and starts
the unit again. `--binary` names the file to install; it defaults to the running
one, so `agent-overflow service update` after replacing your binary on `PATH` is
the whole upgrade.

It is also how the **supervisor itself** is replaced, which nothing else can do:
replace the file the unit starts, then run this once. A staged version that
speaks a newer update protocol than the installed supervisor is refused with
that sentence, for exactly this reason.

### Updating over the wire

Settings → Updates on a supervised host offers the release list and installs
the one you pick, from wherever you are. The backend does the same work the
local command does, in the same order, and adds the download:

1. **Resolve.** The tag you picked, against the published release feed. Only
   releases that ship an artifact for THIS host and a `SHASUMS256` sidecar are
   offered, so a release for another platform is never in the list.
2. **Download.** Into a temporary file next to the version directories, hashed
   as it streams. Bytes the published checksum does not cover are refused and
   the file is deleted; nothing is installed.
3. **Verify.** The downloaded file is asked what it is, in its own process
   (`agent-overflow __service-preflight`), *before* anything is staged. A file
   that is not an Agent Overflow binary this host can run, or one that speaks a
   newer update protocol than the installed supervisor, is refused here — and
   that second refusal names `agent-overflow service update` as the fix,
   because replacing the supervisor is a thing only somebody at the machine can
   do.
4. **Stage.** Under the version the BINARY reported, not the tag, because a
   version directory is named for what is inside it.
5. **Request.** The supervisor takes it from there: the six states above, trial
   and all. A failure at any earlier step leaves the supervisor untouched.

The screen follows the flow live and then watches for the backend to come back.
The update carries an id, so what it shows after the restart is the outcome of
**your** update — committed, rolled back, or failed with the reason — rather
than "the backend restarted".

Two things it needs, and both are stated rather than assumed:

- **A step-up proof.** Installing different code on a machine is in the same
  set as minting a pairing link or changing the listener's binding: you are
  either at the machine, or you satisfy a passkey challenge from the device
  you are holding.
- **A release artifact this host can install as one file.** The Linux binaries
  qualify (the headless one takes `agent-overflow-headless-linux-amd64` and the
  ordinary one takes `agent-overflow-linux-amd64`). macOS does not: its release
  artifact is an app-bundle zip, which the supervisor stages nothing of, so a
  macOS serve host says so on the screen and is updated locally.

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

## Platforms and the headless binary

| Platform | How serve runs |
|---|---|
| Linux | `agent-overflow-headless-linux-amd64`, or the ordinary `agent-overflow-linux-amd64`. |
| macOS | The ordinary binary. There is no separate headless macOS artifact and there is not going to be one: `serve` never opens a window, and the desktop build's frameworks are already present on every Mac. |
| Windows | Not directly. The Windows install is a launcher hosting a Linux backend inside WSL, and that launcher is already the supervisor for that backend. |

The Linux **headless** artifact is the same source built with
`-tags production,nogui`: it links no GTK and no WebKit, so it installs on a
server that has neither, and `ldd` on it names no desktop library. That is the
same tag set the Windows launcher's WSL payload is built with — that binary is
this binary. `make go-build` compiles the tag on every run so it cannot rot
between releases.

Both Linux binaries serve identically. The ordinary one additionally links a
webview it never opens under `serve`, which costs disk and a handful of shared
library dependencies you would have to install. Use the headless one on a
server; use whichever you already have on a workstation.

It is a single file with nothing beside it. Put it on `PATH`
(`~/.local/bin/agent-overflow` is the conventional spot) and it is installed.
`scripts/install.sh` is for the DESKTOP artifacts — it writes a desktop entry
and an icon, which a server has no use for — so it is not the route here.

**The in-app updater is not what updates a serve host.** It is wired by the
desktop boot and by the WSL launcher's backend, and `serve` is neither, so it
stays unconfigured on both Linux binaries. A serve host is updated by its
supervisor instead — `agent-overflow service update` locally, or from any
client through Settings → Updates ("Updating over the wire" above) — and an
unsupervised `serve` is updated by replacing the file
and restarting it. That is also why a desktop install can never be
handed the headless artifact by accident: release assets are matched by exact
name rather than by looking for "linux" somewhere in one
(`internal/appupdate/assetmatch.go`).
