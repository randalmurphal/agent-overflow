# internal/localcontrol/

The owner-only rendezvous for a CLI on the same computer as a running backend.
`control.json` is atomically written with private permissions under the app data
root. It carries a launch credential and must never be printed, logged, copied
to a peer, or passed in argv. It is not a remotely reachable discovery service.
Only numeric loopback addresses are accepted; the client disables HTTP proxies
and redirects. RPCs use the existing transport WS wire and authorization.

Publishing and withdrawal belong to App startup/rebind/shutdown. Withdrawal
checks the launch token so an old shutdown cannot delete a newer launch's file.
Tests use temporary roots and fake servers, never the operator's data root.

`Page` serves the same local desktop-adoption seam: it fetches the existing
transport `/pageurl?host=webview` answer with a header credential and refuses a
non-loopback navigation. URL and single-use ticket remain separate. The adopted
window owns no App/service lifecycle; geometry changes use the existing RPC.
