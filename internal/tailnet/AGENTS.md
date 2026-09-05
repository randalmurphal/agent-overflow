# internal/tailnet/

This backend as a node on the owner's tailnet, using
`tailscale.com/tsnet` in userspace (`docs/specs/remote-access.md` §7,
"Anywhere access"). Off-network reach IS the tailnet: no public listener,
no tunnel, no port forward, so every path a request can take is one the
owner enrolled.

The package owns the node's LIFECYCLE and its published STATUS, and
nothing else. It never serves a request — the listeners it hands back go
to `internal/transport`, which answers them with the same mux, the same
credentials and the same per-call gate its main bind uses. It knows
nothing about settings either; `internal/app/app_tailnet.go` reconciles
the user's preference onto it.

## Layout

- `node.go`: `Node` (construct / start / listen / status / close),
  `Options`, `Status`, `StateDir`, and `Forget`.
- `doc.go`: the package's one-paragraph purpose.

## The three properties worth knowing before editing

**`envknob.SetNoLogsNoSupport()` runs before Start, always, once, with no
setting behind it.** tsnet otherwise streams backend logs to
`log.tailscale.com` for support purposes. This feature's whole posture is
that nothing leaves a path the owner controls, so the upload is not an
option we offer; the knob is a process property, so it is a `sync.Once`,
and it is documented at the call site rather than only here. Do not make
it configurable — a switch that turns log upload back on is a different
feature with its own consent conversation.

**`Node` is SINGLE USE, and `Close` is guarded by a started flag.**
`tsnet.Server.Close` on a server that never ran `Start` dereferences a
nil backend and PANICS (spike-verified against v1.102.3), and "a disable
arrives while an enable is still failing" is exactly that shape. So Close
checks the flag, is idempotent, and bounds its wait on the node's
teardown — one spike run had stragglers past 30s across 27 cycles, and a
reconciler that has to answer a person cannot block on them. A restart
builds a NEW `Node` over the SAME directory, which is what keeps the
identity; making `Node` itself restartable would mean carrying that flag
through two more states for no gain.

**The state directory is key material.** `StateDir(configRoot)` holds
`tailscaled.state` (the private node key, inside tsnet's persisted prefs
blob) and `tailscaled.log.conf` (a private logging id). tsnet chmods the
directory 0700 and the files 0600 itself and rebuilds the same node
identity from them on every later start. AT REST THAT DIRECTORY IS THE
NODE: possession of those bytes is possession of this backend's place on
the owner's tailnet, so anything that copies, backs up or serves the
config root has to treat them the way it treats the session signing key.
Disabling the feature KEEPS them — toggling off is not the same act as
leaving the tailnet — and `Forget` is the separate, explicit deletion.

## Status, not Up

`Start` returns as soon as the backend is constructed. It is deliberately
NOT built on tsnet's `Up`: `Up` waits for `ipn.Running` and never returns
while the node sits in `NeedsLogin`, so an app that called it would have
no way to show the owner the link that would end the wait. The link
arrives on the IPN bus as `Notify.BrowseToURL`, and this package
publishes it as `Status.AuthURL`, clearing it the moment the node joins —
a spent sign-in link left on screen is an instruction to do something
already done.

- One watcher goroutine owns the published status for the node's life,
  and RE-SUBSCRIBES if the bus ends while the node is alive. The
  alternative is a status frozen at whatever it last saw with nothing
  saying so.
- The identity fields (MagicDNS name, addresses, certificate names) are
  re-read on State snapshots and SelfChange, never heartbeats. Toggling
  HTTPS in the admin panel leaves State at Running; the regression is
  covered by `TestCertificateDomainsRefreshWhileRunning`.
- `Events()` is a coalesced depth-1 wake-up channel, closed by `Close`.
  A reader always reads the whole current status, so two changes that
  arrive before it looks are one thing to look at.
- `Listen` and `ListenTLS` REFUSE unless the node is Running, and the
  refusal names the state. That is not politeness: tsnet's own
  `ListenTLS` calls `Up` internally, so calling it early blocks forever
  with nothing saying why.
- `ListenTLS` additionally needs the tailnet to have MagicDNS and HTTPS
  enabled in its ADMIN PANEL, which no code here can substitute for.
  `Status.CertDomains` is how a caller checks before asking; an empty
  list means cleartext-over-WireGuard is the honest answer, and the
  status says so.

## Testing

**Never the real control plane, never a real tailnet.** `rig_test.go`
runs an in-process `testcontrol.Server` plus a loopback DERP/STUN pair,
and enforces two things rather than assuming them: the ambient
environment is refused if it carries `TS_CONTROL_URL`, `TS_AUTHKEY`,
`TS_CLIENT_SECRET` or `TS_ID_TOKEN`, and the control URL is asserted
loopback before a node is pointed at it. A test that silently registered
a device on the developer's own tailnet would leave a machine in their
admin panel and a node key on their disk.

The rig refuses root-path probes before delegating to `testcontrol`, which
panics on unknown routes. Local dev-server discovery can probe any listener;
an unrelated `GET /` must not crash the test process.

**`tailscale.com/tstest/integration` is TEST-ONLY.** It must never appear
in a production file here; `go list -deps ./...` over the non-test build
must not mention it, and the shipped binary must not link a DERP server.

**A case that needs bring-up calls `requireBringUpCapableHost` first.**
netstack needs a usable non-loopback interface with a route to build its
endpoint set from. Without one the node registers and then parks — never
erroring, never joining — so the case would hit its own timeout instead
of saying why. The skip names exactly that.

`integration_test.go` is the two-node story: this backend's node serving
the real `internal/transport` through `ServeAuxiliary`, and a second
tsnet node reaching it by tailnet address. The peer address net/http sees
there is a real 100.64/10 address, so it is the one place the off-host
admission rule is exercised against a peer nothing faked.

What stays live-only by construction: a real Tailscale sign-in, a real
`ts.net` certificate issuance, and DERP-relayed reach between two
machines on different networks.

## Anti-patterns

- Do NOT add a setting for the log-upload opt-out, or move it out of
  `Start`.
- Do NOT call `tsnet.Server.Close` outside `Node.Close`'s guard.
- Do NOT delete the state directory anywhere but `Forget`, and do not
  call `Forget` while a node is live: it takes a config root rather than
  a `Node` precisely because it cannot check, and deleting under a
  running node leaves a process holding an identity nothing records.
- Do NOT serve a request from this package. A listener goes to
  `internal/transport`; a second HTTP surface here would be a second
  credential story.
