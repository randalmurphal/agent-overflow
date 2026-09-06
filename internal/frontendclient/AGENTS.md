# internal/frontendclient/

The desktop frontend for `--frontend` and `--connect <paired computer>`. It boots no `App`,
SQLite store, provider, workspace, LAN listener or execution engine. A small
receiver exposes local connection management and presentation services through
the ordinary `internal/transport` dispatcher; execution computers use that
transport's existing attached-backend carriers. No additional wire protocol.

- Bootstrap lists saved profiles without probing a host. The selected launch
  computer is a URL preference, not the controller's identity or a prerequisite
  for opening the window. The controller has no backend UUID or replica key.
- `mode=frontend` separates the local administrative connection from execution
  entries in the SPA. Lifecycle and admin events include the controller;
  computer catalogs, selection and all-computer RPCs exclude it.
- Reuse the primary App method IDs and package-owned data shapes. The wire
  signature test compares every exposed method against App without constructing
  an App. Do not grow a second implementation of account/project/execution APIs.
- Pairings stay in the installation's existing device profile directory; the
  device client owns cross-process refresh locking and route verification.
  Presentation files and the stable port pin live under `frontend/`, separate
  from the ordinary desktop window. The local origin owns browser preferences.
- Only loopback serves this frontend. Page tickets, cookies, origin checks,
  attached routes and scope checks are the normal transport implementation.
- Accepted pairing waits belong to the frontend lifetime. Shutdown cancels
  waits and SSH consoles, joins them, closes asset watchers and the event bus.
  Ending an SSH console never stops an independently installed remote service.
- Native updates belong to this frontend's process. Configure the existing
  `appupdate.Service` through `app.InitWindowUpdater` before serving the page;
  execution hosts are not involved. Relaunch as `--frontend`, preserving an
  explicit data root, so consumed invitations and removed hosts cannot block
  reopening. An empty profile catalog is a valid frontend boot.

`fixture_test.go` is the E2E process entry in the compiled test binary. It
requires the containment launcher and explicit isolated paths, and emits its
local page credential only over the owned parent pipe. It has no provider
invocation path. `make harness-build` compiles it; the desktop and compact
frontend-client specs drive the actual production SPA against separate hosts.

The old launch-token relay remains in `internal/clientmode`; token attachment
does not invent a durable pairing for a temporary same-host credential.
