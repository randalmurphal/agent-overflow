# internal/devscan

The discovery half of the port gateway: what is listening on this
machine's loopback, which thread it belongs to, and which of those the
person can be offered a preview of. It publishes a list. It binds
nothing, proxies nothing, and never spawns a process.

`Scan(ctx, owners, allowed)` is the whole API, plus `PreviewPorts` for
the ports a gateway should hold a listener on. One `Scanner` belongs to
the App and is safe for concurrent use.

## The three sources are three different affordances

`DevServer.Source` is not a label, it is what the row can do:

- `allowed` — a port the owner hand-named in `network.previewPorts`.
  Allowed because they said so.
- `attributed` — a listener that traces back to a thread's provider
  session or one of its terminals, and that nobody named. Allowed
  automatically; a link is live for as long as the process is.
- `seen` — listening, answering like a page, owned by nothing this app
  started. Not allowed; it is the candidate list the "Allow port" action
  draws from.

Anything else that is listening does not appear at all.

**`allowed` wins over attribution, always.** "allowed" means "in the
persisted list"; "attributed" means "shared only by attribution". A port
that is both still carries its `threadId`, `pid` and `process`, but its
source stays `allowed` — the settings screen lists the `allowed` rows as
the persisted set it can take entries out of, so calling this row
attributed would hide the entry and leave no way to stop sharing it while
the process runs.

## Attribution is process facts, never names

Two rules, and the second exists because the first is not enough:
ancestry (a dev server is normally a grandchild of the session that ran
`npm run dev`) and process GROUP (a dev server that daemonised has
reparented to init, so the chain no longer reaches us, but
`procutil.ConfigureGroup` means it still carries our group id).

Never match on a command name. Every long-running `node` on the machine
is a `node`, and claiming one would put a preview link under a thread
that never started it — which is precisely the case `seen` plus an
explicit Allow exists to cover.

## An open port is not a page

A language server's RPC socket, a debug listener and a database are all
LISTEN on loopback. One bounded GET decides: 2xx with a document content
type, or 3xx that names where to go. The verdict is memoized per
port-and-pid for `probeVerdictTTL`, so the steady state at the 3s scan
cadence costs no dials.

The probe gates the `attributed` and `seen` halves. It does NOT gate
hand-named ports: the probe is a filter on candidates nobody chose, and
re-litigating a choice the owner already made would drop exactly the
backend API they named on purpose.

## Two rows exist that are not listening, on purpose

- An attributed port keeps its row for `attributedGrace` after its
  socket disappears. A dev server restarting is the common case and
  tearing the URL down for the two seconds that takes would make preview
  links unreliable in the one situation people use them.
- A hand-named port with nothing serving keeps its row forever, marked
  not listening, so the setting is visible on screen rather than only in
  the file.

A `seen` port that stopped listening simply goes: there is no preview
anyone could lose.

## Platform split

`enumerate_linux.go` reads `/proc/net/tcp{,6}`, then `/proc/<pid>/fd`
for only the inodes those tables named, then `/proc/<pid>/stat` for the
command and the parent chain. It takes a proc ROOT parameter so tests
run over a fixture tree; production passes `/proc`. This is also what
the Windows deployment runs, since that ships as a WSL payload.

`enumerate_darwin.go` shells out to `lsof` and `ps` with a 3s bound, and
keeps its parsers pure so tests never exec.

`enumerate_other.go` answers `ErrUnsupported` on every call. That is a
deliberate exception to answering once: "nothing is listening" and "this
build cannot look" are different sentences on screen, and the caller
surfaces the error and stops polling rather than printing an empty list
forever.

## Rules for changes here

- A pid this process cannot read is SKIPPED, never an error. A scan that
  failed whenever the machine had another user's process would never
  succeed on a shared host.
- Only loopback and wildcard binds count. A listener already on a
  routable address is somebody else's service, and the gateway could not
  bind its port anyway.
- Every walk is bounded — `maxAncestorDepth`, `maxProbesPerScan`,
  `maxVerdicts`, `probeTimeout`. This runs every 3 seconds; an unbounded
  loop here is a hang in the app, not a slow scan.
- The probe dial resolves `localhost` STATICALLY to `127.0.0.1` then
  `::1` and never asks a resolver, exactly as `internal/devserverprobe`
  does. A resolver answer is configuration this app must not be
  steerable by.
- The `json` tags on `DevServer` and `DevServerList` are the wire
  contract. Renaming one without the client half breaks the list
  silently.

## Tests

Fixture proc trees under `t.TempDir()` (`enumerate_linux_test.go`,
`scan_linux_test.go`) and `httptest` servers on ephemeral loopback ports
(`probe_test.go`). Never the machine's own `/proc` — that would assert
whatever happened to be running on the developer's box — and never a
spawned process or a real network hop.
