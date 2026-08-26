# internal/eventchan/

The names of every event channel the backend pushes to the frontend, as
typed constants. Dependency-free by construction — it imports nothing,
not even from this repo, so every layer that emits can depend on it.

## Surface

- `type Channel string` + `String() string`.
- One exported constant per registered channel, grouped by prefix family
  (`provider:*`, `workflow:*`, `updater:*`, …).

The `harness:*` family carries four, and two of them exist only because
the harness now reaches INTO the frontend rather than only watching it:

| Channel | What rides it |
|---|---|
| `harness:mock` | mock-provider progress reports |
| `harness:replay` | replay playback state |
| `harness:ui-query` | `{id, spec}` — one request for the frontend bridge (`HarnessUIQuery`). Ephemeral: it is a directive whose waiter is gone in 10s, so replaying it to a reconnecting client is pure waste. |
| `harness:perf` | one folded frontend+backend sample per tick of an armed perf run (`HarnessPerfStart`). Full ring: a sample is a point in a series, and a watcher that reconnects wants what it missed. |

## The two-edit contract

A channel exists only when BOTH halves are present:

1. a constant here (the spelling), and
2. a `ChannelPolicy` row in `internal/transport/event_channels.go`
   (the audience + retention decision).

`internal/transport`'s `TestEveryEventChannelConstantHasAPolicyRow` and
`TestEveryChannelPolicyRowHasAConstant` fail on either half missing. The
constants are enumerated by AST-parsing this package's source, so there
is no third list to keep in sync — adding a constant is enough.

## Why the newtype, and what it does not do

`EventBus.Emit`, `(*App).emit` / `emitEvent`, `triage.NewRouter`'s emit
callback, `workflow/engine.Emitter`, and `screenshot.Installer.Emit` all
take a `Channel`. A channel *variable* therefore cannot cross into an
emit site without an explicit `eventchan.Channel(...)` conversion — which
is exactly what the harness escape hatches spell (`HarnessEmit`,
`harness.Replayer`). A caller-named channel that matches a registered
name inherits that row's audience (the harness is the intended forger,
gated by `--harness`/`--soak` + a LocalOnly receiver); an unrecognized
name lands on the registry's fail-closed loopback-only default.

What the type does NOT stop is an untyped string LITERAL: Go assigns
those to any string type. The root package's
`TestEmitSitesNameAnEventChannelConstant` closes that hole by AST-scanning
every production source for an emit call with a `BasicLit` first argument.
Both guards are load-bearing; neither is sufficient alone.

## Anti-patterns

- Do NOT import anything here. A dependency would make some emitting
  package unable to use it, and the whole point is that all of them can.
- Do NOT convert wire input into a constant's type as if it were
  registered. Subscribe frames, replay cursors, and the launcher's
  channel lists are peer-chosen strings; they stay `string` and look the
  registry up directly.
- Do NOT add a constant without its `ChannelPolicy` row (the tests will
  say so, but the row is the part that requires a decision — who may
  receive the frames, and how deep the replay ring is).
- Do NOT re-spell a channel here that another package already exports as
  a cross-process string contract. `notify.ActivatedChannel`,
  `notify.SendChannel`, and `selfupdate.ChannelInstall` are DEFINED as
  these constants (`string(eventchan.X)`) precisely so the two spellings
  cannot drift; they stay `string` because the Windows launcher carries
  them in subscribe frames.
