# internal/devserverprobe

Answers "is something actually listening on this loopback URL right
now" for the dev-server chip. Triage's textual detection
(`internal/triage/dev_server_url.go`) is a candidate generator — a
`tail` of a file containing `http://localhost:5173` produces the same
meta as a Vite startup banner — and this package's TCP dial is the
ground truth that separates the two.

## Ownership

- Input validation is the trust boundary: only loopback HTTP(S) URLs
  (`localhost`, `127.0.0.0/8`, `[::1]`) are dialable. Anything else is
  an error, never a dial — the caller is a wire RPC
  (`ProbeDevServerURL`, loopback-only in
  `internal/transport/internalmethods.go`), and a prober that dials
  arbitrary hosts is an SSRF primitive.
- `localhost` resolves statically to `127.0.0.1` then `::1` — never
  through the system resolver — so dial targets stay deterministic and
  a server bound to a single address family is still found.
- An unreachable port is a `false` verdict, not an error. Errors are
  reserved for invalid input. Zoned addresses (`::1%eth0`) are invalid:
  meaningless on loopback and an unbounded cache-key space otherwise.
- Verdicts cache under one mutex with two TTLs, and this cache is the
  ONLY verdict memo — the frontend consumer (`utils/devServerProbe.ts`
  + `CommandOutput.svelte`'s probe effect) deliberately keeps none, so
  staleness has a single authority. Both TTLs must stay strictly below
  the frontend's probe cadences (1.5s unconfirmed retry / 5s confirmed
  re-verify) or a scheduled probe is answered from memory instead of
  the dialer. Dead re-checks sooner than live so a just-started server
  is noticed promptly.
- The entry cap bounds MEMORY, not dial rate (keys derive from
  model/tool-authored command output); dial concurrency is bounded by
  the transport's per-connection RPC cap.
- No single-flight, deliberately: a duplicate loopback dial is a
  ~microsecond connect+close, and the frontend dedupes in-flight probes
  per client. Add one only if a non-frontend Go caller appears for
  which duplicate dials actually cost something.

## Testing

- Never dial fixed ports in tests — listen on `:0` for live-path
  coverage and inject `dial` / `now` for everything else (address
  selection, cache TTLs, bounds).
