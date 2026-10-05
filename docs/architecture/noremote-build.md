# Build without remote access

The `noremote` build tag produces a Windows/WSL release with no remote
access. Its only functional difference from the standard build is that
remote features are unavailable and cannot be turned on: no listener off
loopback, no remote CLI commands, no remote settings or pages. Local
history, settings and providers behave as in the standard build. The
standard build is unchanged by the tag's existence.

`internal/buildvariant` holds the constant `RemoteAccess` and the shared
refusal `ErrRemoteAccessUnavailable`. Code checks the constant at the owning
API, so the refusal does not depend on a caller.

## Enforcement

| Surface | Owner | Refusal |
|---|---|---|
| Remote-access dependencies | `internal/tailnet`, `internal/nearby`, `internal/acmecert`, `internal/push` | Tagged stubs refuse; tailscale, mDNS, ACME and FCM clients are not linked |
| Bound methods | `//ao:remote` in `internal/app`, `Dispatcher.InvokeForOrigin` | Every remote-only RPC is refused on every call path |
| Listeners | `transport.bindListener`, `ServeAuxiliary`, `PreviewLANSource` | The main bind and auxiliary listeners (the local `::1` one included) must be loopback, both as requested and as the kernel reports them; LAN preview listeners are refused |
| Outbound peer connections | `deviceclient.NewPinnedTransport` | Dials are refused |
| Settings | `settings.validateNetwork`, `sanitizeNetwork` | Remote network fields are refused on write and ignored on load; the listen port remains |
| Executable | `refuseRemoteBoot` in `main_buildvariant.go`, `internal/aocli` | `serve`, `--supervise`, `--connect`, `--frontend`, non-loopback `--listen` and the `remote`, `pair` and `service` commands are refused |
| Windows launcher | `nativenetwork.Run` | The LAN bridge never starts |
| Agent tools | `remoteMCPEnabled` | `ao-remote-tools` is never offered |
| UI | `<meta name="ao-remote-access" content="off">` in the served entry shell, read by `frontend/src/lib/transport/buildVariant.ts` | Remote settings pages, fields and actions are hidden before first paint, and the remote-only client machinery does not start |

Bound methods in the remote-access owner files are either `//ao:remote` or
listed with a reason in `remoteOwnerLocalMethods`
(`internal/app/app_remote_classification_test.go`); the test fails for an
unclassified method. `TestNoremoteBuildsLinkNoRemoteAccessDependencies`
checks the import graph of the WSL payload, the Windows launcher and the
harness binary.

Boot-time remote services stay wired. Without saved remote state and with
settings, dials and listeners refused, they have nothing to act on.

## Testing

Run Go tests for both variants:
`make go-test` and `make go-test GO_TEST_FLAGS='-tags noremote'`. A test of
remote behavior calls `remotetest.Require(t)` or carries `//go:build
!noremote`. `make harness-build` also builds `bin/agent-overflow-noremote`,
which `e2e/tests/noremote-build.spec.ts` runs. Frontend tests of the hidden
surfaces set the variant with `frontend/src/test/helpers/buildVariant.ts`.

## Release

`make release-wsl-noremote UPDATE_SOURCE=HOST/GROUP/PROJECT` runs
`scripts/build-release-noremote.sh`. It builds the payload and launcher with
the tag, links the GitLab project into the updater, checks with
`go version -m` that neither binary links tailscale or mDNS, and writes
`agent-overflow-wsl-noremote-amd64.exe`, `install.sh` and `SHASUMS256` to
`dist/release-noremote/VERSION`.

## Updates and installation

The linked project selects the GitLab feed in `internal/appupdate`
(`gitlab.go`). Every request runs `glab api --hostname HOST`, so the user's
glab login authenticates it and the app stores no token. A signed-out or
missing glab is reported as the update check's error. The WSL updater of a
noremote build installs only `agent-overflow-wsl-noremote-amd64.exe` and has
no feed when no project was linked; it never reads GitHub.

A GitLab release is installable when its links are named exactly
`agent-overflow-wsl-noremote-amd64.exe` and `SHASUMS256` and point under
`https://HOST/api/v4/` on the linked host, such as generic package files.
Other links are refused. First installation also downloads `install.sh` from
the release, so the release carries all three files.

First installation runs inside WSL, with glab signed in:

```sh
d=$(mktemp -d) && glab release download -R GROUP/PROJECT \
  -n agent-overflow-wsl-noremote-amd64.exe -n SHASUMS256 -n install.sh -D "$d" \
  && sh "$d/install.sh" --wsl --noremote --source "$d"
```

`install.sh --noremote` applies only to `--wsl` and needs `--source`; a
local directory source makes no network request.
