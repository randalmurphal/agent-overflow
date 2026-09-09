# WSL backend launcher

This package discovers WSL distributions, installs the Linux payload, starts it,
owns its Windows lifetime boundary, parses bootstrap output, and carries
launcher RPC traffic. The GUI lives in cmd/agent-overflow-windows and persisted
selection lives in internal/wsldistro.

## Process and payload contracts

Use wsl.exe --exec with an explicit argument vector. Implicit shell mode
re-parses arguments through the user's login shell. Invoke /bin/sh -c explicitly
only when shell semantics are required.

Create the child suspended, assign it to a Job Object configured with
KILL_ON_JOB_CLOSE and SILENT_BREAKAWAY_OK, then resume it. This closes the WSL
session with the launcher without capturing Windows interop children. Never use
wsl --shutdown.

Install payloads through the Windows automount and atomically rename them into
the profile-specific destination. Do not stream binary payloads through wsl.exe
stdin. Drain stdout and stderr for the full child lifetime, including stdout
after the bootstrap sentinel.

The backend owns restored bind configuration. Launch with --print-url-fd 0 and
no injected --listen. The host may request one pinned-port reset only after a
proven unreachable listener.

## Bootstrap and authentication

Bootstrap.PageURL comes from the backend and contains no credential. Reject a
bootstrap record without it. Bootstrap.Token authenticates launcher probes and
connections; never place it in a URL.

Every WebView navigation obtains a fresh URL and one-time page ticket. Deliver
the ticket through uiwindow.DeliverPageTicket, separate from the logged URL.
Keep duplicated route constants pinned by drift tests because this package does
not link the transport server.

Forward a local session credential in X-AO-Session when available. Fetching is
best effort and an unavailable session core cannot prevent connection. Mark it
stale after an explicitly refused dial. Never put session credentials in URLs.

## Launcher connection

Only replayable notification traffic carries a replay cursor. Update, browser,
power, and native-network directives are ephemeral. Validate each directive and
dispatch blocking handlers away from the socket read loop.

Result-bearing RPCs have bounded pending-call and disconnect handling.
Distinguish explicit refusal from undelivered calls. ClassifyInstallAck stops
replacement after refusal and proceeds after ambiguous delivery failure. Report
calls are bounded end to end; cold notification activation may wait on its
caller's context.

ListDistros parses UTF-16LE with or without a BOM and tolerates changing column
widths. Native Linux and macOS return no distributions; WSL may use Windows
interop. Tests cover argv, adoption, stream draining, malformed bootstrap,
authentication, replay, RPC delivery classes, UTF-16, and replacement.
